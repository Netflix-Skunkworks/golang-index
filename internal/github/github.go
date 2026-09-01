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

// Listing an owner's repos, or a page of accounts, takes longer than the per-repo
// queries do.
const listingTimeout = 60 * time.Second

// repoAccessDenied distinguishes a denied repo from a rate limit, which GitHub
// also reports as 403.
func repoAccessDenied(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	rateLimited := resp.Header.Get("Gh-Limited-By") != "" ||
		resp.Header.Get("Retry-After") != "" ||
		resp.Header.Get("X-RateLimit-Remaining") == "0"
	return !rateLimited
}

func redirected(resp *http.Response) bool {
	return resp.StatusCode >= 300 && resp.StatusCode < 400
}

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
	// retryDelay, when non-zero, replaces accountsRetryDelay so tests don't sleep.
	retryDelay time.Duration
}

// NewGithubSCM creates a new Github SCM.
func NewGithubSCM(client githubClient, baseURL string, httpClient *http.Client) *GithubSCM {
	return &GithubSCM{
		graphqlClient: client,
		baseURL:       baseURL,
		httpClient:    withoutRedirects(httpClient),
	}
}

// withoutRedirects returns a copy of client that hands a 3xx response back rather
// than following it. A redirect leads off baseURL, and so away from any proxy in
// front of the GitHub host that supplies the credentials; where it leads is worth
// reporting rather than following. A nil client, which the tests that exercise
// only GraphQL pass, stays nil.
func withoutRedirects(httpClient *http.Client) *http.Client {
	if httpClient == nil {
		return nil
	}
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// NewEnterpriseSCM builds a GithubSCM for a GitHub Enterprise host, wiring the
// GraphQL client (at baseURL+"/api/graphql") and the raw/REST calls to the same
// httpClient.
func NewEnterpriseSCM(baseURL string, httpClient *http.Client) *GithubSCM {
	scm := NewGithubSCM(nil, baseURL, httpClient)
	scm.graphqlClient = githubv4.NewEnterpriseClient(baseURL+"/api/graphql", scm.httpClient)
	return scm
}

type queryPageInfo struct {
	EndCursor   githubv4.String
	HasNextPage bool
}

// GitHub caps the accounts listing at 100 per page.
const accountsPageSize = 100

const (
	accountsAttempts   = 3
	accountsRetryDelay = 5 * time.Second
)

// ownerReposQuery fetches (and then holds the response for) a page of one owner's
// repositories. ownerAffiliations defaults to OWNER and COLLABORATOR, which would
// have every member of an organization page through that organization's repos
// again.
type ownerReposQuery struct {
	RepositoryOwner struct {
		Repositories ownerRepoPage `graphql:"repositories(first: 100, isFork: false, ownerAffiliations: [OWNER], after: $reposCursor)"`
	} `graphql:"repositoryOwner(login: $ownerLogin)"`
}

type ownerRepoPage struct {
	Nodes    []ownerRepoNode
	PageInfo queryPageInfo
}

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

// OwnerGoRepos returns the Go repos an owner owns, as a slice of
// "orgname/reponame".
//
// A repo counts as a Go repo when GitHub detected Go among its languages, wherever
// Go sits in that ranking, or when it has a go.mod at its root. Forks are skipped,
// since a fork declares the module path its upstream already declares.
//
// An owner GitHub no longer knows comes back with no repos rather than an error.
func (scm *GithubSCM) OwnerGoRepos(ctx context.Context, ownerLogin string) ([]string, error) {
	var names []string
	var cursor *githubv4.String
	for {
		page, err := scm.ownerReposPage(ctx, ownerLogin, cursor)
		if err != nil {
			return nil, fmt.Errorf("error querying repositories of %s: %w", ownerLogin, err)
		}
		for _, repo := range page.Nodes {
			if holdsGo(repo) {
				names = append(names, string(repo.NameWithOwner))
			}
		}
		if !page.PageInfo.HasNextPage {
			return names, nil
		}
		cursor = githubv4.NewString(page.PageInfo.EndCursor)
	}
}

func (scm *GithubSCM) ownerReposPage(ctx context.Context, ownerLogin string, cursor *githubv4.String) (ownerRepoPage, error) {
	queryCtx, cancel := context.WithTimeout(ctx, listingTimeout)
	defer cancel()

	var q ownerReposQuery
	variables := map[string]any{"ownerLogin": githubv4.String(ownerLogin), "reposCursor": cursor}
	if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
		return ownerRepoPage{}, err
	}
	return q.RepositoryOwner.Repositories, nil
}

func holdsGo(repo ownerRepoNode) bool {
	if repo.RootGoMod != nil {
		return true
	}
	return slices.ContainsFunc(repo.Languages.Nodes, func(l languageNode) bool { return l.Name == goLanguage })
}

// Login names the account to GraphQL; ID is what pages the listing.
type account struct {
	ID    int    `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Owners lists the login of every account on the host that can own repositories.
// GitHub's REST accounts listing enumerates organizations alongside users in one
// stream, so a single paged call covers every owner.
func (scm *GithubSCM) Owners(ctx context.Context) ([]string, error) {
	var logins []string
	// The listing pages by the id of the last account handed over, and ends with an
	// empty page.
	sinceID := 0
	for {
		accounts, err := scm.accountsSince(ctx, sinceID)
		if err != nil {
			return nil, err
		}
		if len(accounts) == 0 {
			slog.Info(fmt.Sprintf("Owner listing found %d repository owners", len(logins)))
			return logins, nil
		}
		for _, a := range accounts {
			// An allowlist, not a bot blocklist: repositoryOwner rejects any non-owner
			// type, so admitting one would queue a work item that can never complete.
			if a.Type == "User" || a.Type == "Organization" {
				logins = append(logins, a.Login)
			}
		}
		sinceID = accounts[len(accounts)-1].ID
	}
}

// accountsSince reads one page of the accounts listing, retrying a few times: the
// listing pages by the last id seen, so a page lost to a slow host would take every
// account after it and quietly shorten the listing.
func (scm *GithubSCM) accountsSince(ctx context.Context, sinceID int) ([]account, error) {
	delay := scm.retryDelay
	if delay == 0 {
		delay = accountsRetryDelay
	}

	var err error
	for attempt := range accountsAttempts {
		if attempt > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var accounts []account
		if accounts, err = scm.accountsPage(ctx, sinceID); err == nil {
			return accounts, nil
		}
		slog.Warn(fmt.Sprintf("Retrying accounts listing (attempt %d of %d): %v", attempt+1, accountsAttempts, err))
	}
	return nil, err
}

func (scm *GithubSCM) accountsPage(ctx context.Context, sinceID int) ([]account, error) {
	queryCtx, cancel := context.WithTimeout(ctx, listingTimeout)
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
func (scm *GithubSCM) RepoTags(ctx context.Context, orgRepoName string) (tags []Tag, retryable bool, err error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, false, fmt.Errorf("RepoTags: %v", err)
	}

	variables := map[string]any{
		"repoOrg":    githubv4.String(repo.org),
		"repoName":   githubv4.String(repo.name),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var q tagQueryResponse
	// Page through all the results.
	for {
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, true, fmt.Errorf("error querying tags for %s: %v", repo.fullName(), err)
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

	return tags, false, nil
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
func (scm *GithubSCM) HeadCommit(ctx context.Context, orgRepoName string) (oid string, committed time.Time, retryable bool, err error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("HeadCommit: %v", err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var q headQueryResponse
	variables := map[string]any{
		"repoOrg":  githubv4.String(repo.org),
		"repoName": githubv4.String(repo.name),
	}
	if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
		return "", time.Time{}, true, fmt.Errorf("error querying HEAD for %s: %v", repo.fullName(), err)
	}

	commit := q.Repository.DefaultBranchRef.Target.Commit
	return string(commit.OID), commit.CommittedDate.UTC(), false, nil
}

// GoMod fetches the raw go.mod at subdir (the repo root when subdir is "") for
// the given ref (a tag or commit). found is false, with a nil error, when the
// repo has no such go.mod: that's a 404, which is common enough that we don't
// treat it as an error or log it.
func (scm *GithubSCM) GoMod(ctx context.Context, orgRepoName, ref, subdir string) (content []byte, found, retryable bool, err error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, false, false, fmt.Errorf("GoMod: %v", err)
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
		return nil, false, false, fmt.Errorf("error building raw github API request: %v", err)
	}

	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, false, true, fmt.Errorf("error querying raw github API for go.mod contents: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, false, false, nil
	}
	if redirected(resp) {
		return nil, false, false, fmt.Errorf("raw github API redirected %s to %q; the repo has probably been renamed", repo.fullName(), resp.Header.Get("Location"))
	}
	if repoAccessDenied(resp) {
		return nil, false, false, fmt.Errorf("access to %s denied by the raw github API", repo.fullName())
	}
	if resp.StatusCode != 200 {
		return nil, false, true, fmt.Errorf("unexpected status code from raw github API. Status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, true, fmt.Errorf("error reading raw github API response: %v", err)
	}

	return bodyBytes, true, false, nil
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
func (scm *GithubSCM) ModuleDirs(ctx context.Context, orgRepoName, ref string) (subdirs []string, retryable bool, err error) {
	repo, err := newRepo(orgRepoName)
	if err != nil {
		return nil, false, fmt.Errorf("ModuleDirs: %v", err)
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
		return nil, false, fmt.Errorf("error building git trees API request: %v", err)
	}

	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, true, fmt.Errorf("error querying git trees API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, false, nil
	}
	if redirected(resp) {
		return nil, false, fmt.Errorf("git trees API redirected %s to %q; the repo has probably been renamed", repo.fullName(), resp.Header.Get("Location"))
	}
	if repoAccessDenied(resp) {
		return nil, false, fmt.Errorf("access to %s denied by the git trees API", repo.fullName())
	}
	if resp.StatusCode != 200 {
		return nil, true, fmt.Errorf("unexpected status code from git trees API. Status code: %d", resp.StatusCode)
	}

	var tree treeResponse
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, true, fmt.Errorf("error decoding git trees API response: %v", err)
	}
	if tree.Truncated {
		slog.Warn(fmt.Sprintf("git tree for %s is truncated; some modules may be missing", repo.fullName()))
	}

	for _, entry := range tree.Tree {
		if entry.Type != "blob" {
			continue
		}
		if subdir, ok := moduleSubdir(entry.Path); ok {
			subdirs = append(subdirs, subdir)
		}
	}
	return subdirs, false, nil
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
