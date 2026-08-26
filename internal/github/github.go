// Package github implements github querying logic.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
)

const queryTimeout = 10 * time.Second

const sweepQueryTimeout = 60 * time.Second

// githubClient wraps query interface from the shurcooL/githubv4 package so
// that we can mock github graphql query responses in tests.
type githubClient interface {
	// Matches https://pkg.go.dev/github.com/shurcooL/githubv4#Client.Query
	Query(ctx context.Context, query any, variables map[string]any) error
}

// A handle for specialised github querying.
type GithubSCM struct {
	graphqlClient githubClient
	// baseURL is where requests are sent; it may be a proxy in front of the GitHub
	// Enterprise host, so it can differ from the host repos are named for.
	baseURL    string
	httpClient *http.Client
	// retryDelay, when non-zero, replaces sweepRetryDelay so tests don't sleep.
	retryDelay time.Duration
}

// NewGithubSCM creates a new Github SCM.
func NewGithubSCM(client githubClient, baseURL string, httpClient *http.Client) *GithubSCM {
	return &GithubSCM{
		graphqlClient: client,
		baseURL:       baseURL,
		httpClient:    httpClient,
	}
}

// NewEnterpriseSCM builds a GithubSCM for a GitHub Enterprise host, wiring the
// GraphQL client (at baseURL+"/api/graphql") and the raw/REST calls to the same
// httpClient.
func NewEnterpriseSCM(baseURL string, httpClient *http.Client) *GithubSCM {
	graphql := githubv4.NewEnterpriseClient(baseURL+"/api/graphql", httpClient)
	return NewGithubSCM(graphql, baseURL, httpClient)
}

type queryPageInfo struct {
	EndCursor   githubv4.String
	HasNextPage bool
}

// The most their APIs hand over at once.
const (
	ownerBatchSize   = 100
	accountsPageSize = 100
)

const (
	batchedRepoPageSize = 20
	ownerRepoPageSize   = 100
)

// Every page of a sweep carries the cursor to the next, so a failed page costs the
// pages after it as well.
const (
	sweepQueryAttempts = 3
	sweepRetryDelay    = 5 * time.Second
)

// A query carries a single cursor for every owner in it, so an owner paged past its
// first page is a batch of one.
type ownerBatch struct {
	ownerIDs     []githubv4.ID
	cursor       *githubv4.String
	repoPageSize int
}

type ownerReposQuery struct {
	Nodes []ownerNode `graphql:"nodes(ids: $ownerIDs)"`
}

type ownerNode struct {
	Owner struct {
		Repositories struct {
			Nodes    []ownerRepoNode
			PageInfo queryPageInfo
		} `graphql:"repositories(first: $repoPageSize, isFork: false, after: $reposCursor)"`
		ID githubv4.ID
	} `graphql:"... on RepositoryOwner"`
}

// Owners times repositories times languages has to stay inside the 500,000 nodes a
// query may traverse.
type ownerRepoNode struct {
	NameWithOwner githubv4.String
	Languages     struct {
		Nodes []languageNode
	} `graphql:"languages(first: 20, orderBy: {field: SIZE, direction: DESC})"`
	RootGoMod *struct {
		OID githubv4.GitObjectID `graphql:"oid"`
	} `graphql:"rootGoMod: object(expression: \"HEAD:go.mod\")"`
}

type languageNode struct {
	Name githubv4.String
}

const goLanguage = "Go"

// GoRepos retrieves all Go repos. Returns results as slice of "orgname/reponame".
//
// A repo counts as a Go repo when GitHub detected Go among its languages, wherever
// Go sits in that ranking, or when it has a go.mod at its root. Forks are skipped,
// since a fork declares the module path its upstream already declares.
//
// Reads every repository owner on the host, so it blocks for minutes where there
// are many repos.
//
// Owners a query can't read even on retry are left out and the sweep carries on:
// storing a repo list never removes a repo, so the next sweep reaches them. Only
// every query failing is an error.
func (scm *GithubSCM) GoRepos(ctx context.Context) ([]string, error) {
	ownerIDs, err := scm.ownerIDs(ctx)
	if err != nil {
		return nil, err
	}

	// Owners paged past their first page join the end, so this grows as it is read.
	unswept := make([]ownerBatch, 0, len(ownerIDs)/ownerBatchSize+1)
	for batch := range slices.Chunk(ownerIDs, ownerBatchSize) {
		unswept = append(unswept, ownerBatch{ownerIDs: batch, repoPageSize: batchedRepoPageSize})
	}

	var goRepos []string
	var failed, ownersGivenUp int
	for i := 0; i < len(unswept); i++ {
		names, unfinished, err := scm.goReposPage(ctx, unswept[i])
		if err != nil {
			// Giving up on a page gives up the pages after it too.
			failed++
			ownersGivenUp += len(unswept[i].ownerIDs)
			slog.Error(err.Error())
			continue
		}
		goRepos = append(goRepos, names...)
		unswept = append(unswept, unfinished...)
	}

	if failed == len(unswept) {
		return nil, fmt.Errorf("all %d Go repo sweep queries failed", failed)
	}
	slog.Info(fmt.Sprintf("Go repo sweep found %d repos listing Go across %d repository owners. %d of %d queries failed, leaving %d owners partly unread",
		len(goRepos), len(ownerIDs), failed, len(unswept), ownersGivenUp))
	return goRepos, nil
}

func (scm *GithubSCM) retrying(ctx context.Context, request func() error) error {
	delay := scm.retryDelay
	if delay == 0 {
		delay = sweepRetryDelay
	}

	var err error
	for attempt := range sweepQueryAttempts {
		if attempt > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = request(); err == nil {
			return nil
		}
		slog.Warn(fmt.Sprintf("Retrying Go repo sweep request (attempt %d of %d): %v", attempt+1, sweepQueryAttempts, err))
	}
	return err
}

// goReposPage returns the "org/name" of the batch's repos holding Go, and a batch
// for each owner whose repositories carry on past this page.
func (scm *GithubSCM) goReposPage(ctx context.Context, batch ownerBatch) ([]string, []ownerBatch, error) {
	var names []string
	var unfinished []ownerBatch
	err := scm.retrying(ctx, func() error {
		var err error
		names, unfinished, err = scm.goReposPageOnce(ctx, batch)
		return err
	})
	return names, unfinished, err
}

func (scm *GithubSCM) goReposPageOnce(ctx context.Context, batch ownerBatch) ([]string, []ownerBatch, error) {
	queryCtx, cancel := context.WithTimeout(ctx, sweepQueryTimeout)
	defer cancel()

	var q ownerReposQuery
	variables := map[string]any{
		"ownerIDs":     batch.ownerIDs,
		"repoPageSize": githubv4.Int(batch.repoPageSize),
		"reposCursor":  batch.cursor,
	}
	if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
		return nil, nil, fmt.Errorf("error querying repositories of %d owners starting at %v: %w", len(batch.ownerIDs), batch.ownerIDs[0], err)
	}

	var names []string
	var unfinished []ownerBatch
	for _, node := range q.Nodes {
		owner := node.Owner
		for _, repo := range owner.Repositories.Nodes {
			holdsGo := repo.RootGoMod != nil ||
				slices.ContainsFunc(repo.Languages.Nodes, func(l languageNode) bool { return l.Name == goLanguage })
			if holdsGo {
				names = append(names, string(repo.NameWithOwner))
			}
		}
		if owner.Repositories.PageInfo.HasNextPage {
			unfinished = append(unfinished, ownerBatch{
				ownerIDs:     []githubv4.ID{owner.ID},
				cursor:       githubv4.NewString(owner.Repositories.PageInfo.EndCursor),
				repoPageSize: ownerRepoPageSize,
			})
		}
	}
	return names, unfinished, nil
}

// NodeID identifies the account to GraphQL; ID is what pages the listing.
type account struct {
	ID     int    `json:"id"`
	NodeID string `json:"node_id"`
}

// ownerIDs lists the node ID of every repository owner on the host. GitHub's
// accounts listing holds organizations alongside users, so it alone covers every
// owner; GraphQL offers no connection over them.
//
// TODO(jbarkhuysen): Give owners their own indexing stage and table, the way repos
// have one. Enumerating them into memory on every pass ties the two together: they
// can't be scheduled apart, a sweep that fails re-reads every account before it can
// retry, and there is nowhere to record which owners a pass got through.
func (scm *GithubSCM) ownerIDs(ctx context.Context) ([]githubv4.ID, error) {
	var ids []githubv4.ID
	// The listing pages by the id of the last account handed over, and ends with an
	// empty page.
	sinceID := 0
	for {
		accounts, err := scm.accountsSince(ctx, sinceID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			return ids, nil
		}
		for _, a := range accounts {
			ids = append(ids, githubv4.ID(a.NodeID))
		}
		sinceID = accounts[len(accounts)-1].ID
	}
}

func (scm *GithubSCM) accountsSince(ctx context.Context, sinceID int) ([]account, error) {
	var accounts []account
	err := scm.retrying(ctx, func() error {
		var err error
		accounts, err = scm.accountsPage(ctx, sinceID)
		return err
	})
	return accounts, err
}

func (scm *GithubSCM) accountsPage(ctx context.Context, sinceID int) ([]account, error) {
	queryCtx, cancel := context.WithTimeout(ctx, sweepQueryTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		queryCtx,
		http.MethodGet,
		fmt.Sprintf("%s/api/v3/users?since=%d&per_page=%d", scm.baseURL, sinceID, accountsPageSize),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("error building accounts request: %w", err)
	}

	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("error querying accounts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d listing accounts", resp.StatusCode)
	}

	var accounts []account
	if err := json.NewDecoder(resp.Body).Decode(&accounts); err != nil {
		return nil, fmt.Errorf("error decoding accounts response: %w", err)
	}
	return accounts, nil
}

// tagQueryResponse fetches (and then holds response for) the tags for a repo.
type tagQueryResponse struct {
	Repository struct {
		Refs struct {
			Edges    []tagQueryEdge
			PageInfo queryPageInfo
		} `graphql:"refs(refPrefix: \"refs/tags/\", orderBy: {field: TAG_COMMIT_DATE, direction: DESC}, first: 100, after: $tagsCursor)"`
	} `graphql:"repository(owner: $repoOrg, name: $repoName)"`
}

type tagQueryEdge struct {
	Node struct {
		Name   githubv4.String
		Target struct {
			Commit struct {
				CommittedDate githubv4.DateTime
			} `graphql:"... on Commit"`
			Tag struct {
				Tagger struct {
					Date githubv4.DateTime
				}
			} `graphql:"... on Tag"`
		}
	}
}

// Tag is one of a repo's git tags and when it was created: the commit date for
// a lightweight tag, or the tagger date for an annotated one.
type Tag struct {
	Name string
	Date time.Time
}

// RepoTags returns all of a repo's git tags, ordered newest first.
func (scm *GithubSCM) RepoTags(ctx context.Context, orgRepoName string) ([]Tag, error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, fmt.Errorf("RepoTags: %v", err)
	}

	variables := map[string]any{
		"repoOrg":    githubv4.String(repo.org),
		"repoName":   githubv4.String(repo.name),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var tags []Tag
	var q tagQueryResponse
	// Page through all the results.
	for {
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying tags for %s: %v", repo.fullName(), err)
		}

		for _, edge := range q.Repository.Refs.Edges {
			tag := Tag{Name: string(edge.Node.Name)}

			// Lightweight tags point directly to commits and carry a
			// `committedDate` timestamp. Annotated tags do not; they store their
			// creation timestamp in the `tag.tagger.date` field instead. This
			// logic sets the tag date correctly for both types of tags.
			if !edge.Node.Target.Commit.CommittedDate.IsZero() {
				tag.Date = edge.Node.Target.Commit.CommittedDate.UTC()
			} else if !edge.Node.Target.Tag.Tagger.Date.IsZero() {
				tag.Date = edge.Node.Target.Tag.Tagger.Date.UTC()
			}

			tags = append(tags, tag)
		}

		if !q.Repository.Refs.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Repository.Refs.PageInfo.EndCursor)
	}

	return tags, nil
}

// headQueryResponse fetches (and then holds the response for) the default
// branch's HEAD commit for a repo.
type headQueryResponse struct {
	Repository struct {
		DefaultBranchRef struct {
			Target struct {
				Commit struct {
					OID           githubv4.GitObjectID `graphql:"oid"`
					CommittedDate githubv4.DateTime
				} `graphql:"... on Commit"`
			}
		}
	} `graphql:"repository(owner: $repoOrg, name: $repoName)"`
}

// HeadCommit returns the default branch's HEAD commit oid and commit time. The
// oid is empty when the repo has no commits.
func (scm *GithubSCM) HeadCommit(ctx context.Context, orgRepoName string) (string, time.Time, error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("HeadCommit: %v", err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var q headQueryResponse
	variables := map[string]any{
		"repoOrg":  githubv4.String(repo.org),
		"repoName": githubv4.String(repo.name),
	}
	if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
		return "", time.Time{}, fmt.Errorf("error querying HEAD for %s: %v", repo.fullName(), err)
	}

	commit := q.Repository.DefaultBranchRef.Target.Commit
	return string(commit.OID), commit.CommittedDate.UTC(), nil
}

// GoMod fetches the raw go.mod at subdir (the repo root when subdir is "") for
// the given ref (a tag or commit). found is false, with a nil error, when the
// repo has no such go.mod: that's a 404, which is common enough that we don't
// treat it as an error or log it.
func (scm *GithubSCM) GoMod(ctx context.Context, orgRepoName, ref, subdir string) ([]byte, bool, error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, false, fmt.Errorf("GoMod: %v", err)
	}

	goModPath := "go.mod"
	if subdir != "" {
		goModPath = subdir + "/go.mod"
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/raw/%s/%s/%s/%s", scm.baseURL, repo.org, repo.name, ref, goModPath),
		nil,
	)
	if err != nil {
		return nil, false, fmt.Errorf("error building raw github API request: %v", err)
	}

	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("error querying raw github API for go.mod contents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, false, nil
	}

	if resp.StatusCode != 200 {
		return nil, false, fmt.Errorf("unexpected status code from raw github API. Status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("error reading raw github API response: %v", err)
	}

	return bodyBytes, true, nil
}

// treeResponse is the subset of the git trees API response we read.
type treeResponse struct {
	Tree []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	} `json:"tree"`
	Truncated bool `json:"truncated"`
}

// ModuleDirs returns the repo-relative directories that hold a go.mod at ref;
// the repo root is the empty string. Non-module directories are skipped (see
// moduleSubdir).
func (scm *GithubSCM) ModuleDirs(ctx context.Context, orgRepoName, ref string) ([]string, error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, fmt.Errorf("ModuleDirs: %v", err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(
		queryCtx,
		http.MethodGet,
		fmt.Sprintf("%s/api/v3/repos/%s/%s/git/trees/%s?recursive=1", scm.baseURL, repo.org, repo.name, ref),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("error building git trees API request: %v", err)
	}

	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("error querying git trees API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status code from git trees API. Status code: %d", resp.StatusCode)
	}

	var tree treeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, fmt.Errorf("error decoding git trees API response: %v", err)
	}
	if tree.Truncated {
		slog.Warn(fmt.Sprintf("git tree for %s is truncated; some modules may be missing", repo.fullName()))
	}

	var subdirs []string
	for _, entry := range tree.Tree {
		if entry.Type != "blob" {
			continue
		}
		if subdir, ok := moduleSubdir(entry.Path); ok {
			subdirs = append(subdirs, subdir)
		}
	}
	return subdirs, nil
}

// moduleSubdir reports whether treePath is a repo module's go.mod and, if so,
// the directory holding it (the repo root is the empty string). It skips go.mod
// files under conventional non-module directories: vendor, testdata, and dot- or
// underscore-prefixed dirs (matching how the go tool ignores them).
func moduleSubdir(treePath string) (subdir string, ok bool) {
	dir, file := path.Split(treePath)
	if file != "go.mod" {
		return "", false
	}
	subdir = strings.TrimSuffix(dir, "/")
	for elem := range strings.SplitSeq(subdir, "/") {
		if elem == "vendor" || elem == "testdata" || strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "_") {
			return "", false
		}
	}
	return subdir, true
}
