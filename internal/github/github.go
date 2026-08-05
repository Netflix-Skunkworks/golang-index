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
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
)

const queryTimeout = 10 * time.Second

// githubClient wraps query interface from the shurcooL/githubv4 package so
// that we can mock github graphql query responses in tests.
type githubClient interface {
	// Matches https://pkg.go.dev/github.com/shurcooL/githubv4#Client.Query
	Query(ctx context.Context, query any, variables map[string]any) error
}

// A handle for specialised github querying.
type GithubSCM struct {
	graphqlClient githubClient
	// baseURL is where requests are sent; it may be a proxy in front of the
	// GitHub Enterprise host, so it can differ from githubHostName.
	baseURL string
	// githubHostName is the GitHub Enterprise host. GoRepos strips it from repo
	// URLs to yield "org/name"; it is not used to connect.
	githubHostName string
	httpClient     *http.Client
}

// Creates a new Github SCM.
func NewGithubSCM(client githubClient, baseURL, githubHostName string, httpClient *http.Client) *GithubSCM {
	return &GithubSCM{
		graphqlClient:  client,
		baseURL:        baseURL,
		githubHostName: githubHostName,
		httpClient:     httpClient,
	}
}

// NewEnterpriseSCM builds a GithubSCM for a GitHub Enterprise host, wiring the
// GraphQL client (at baseURL+"/api/graphql") and the raw/REST calls to the same
// httpClient. baseURL is where requests are sent (possibly a proxy);
// githubHostName is the enterprise host used for module paths and repo URLs.
func NewEnterpriseSCM(baseURL, githubHostName string, httpClient *http.Client) *GithubSCM {
	graphql := githubv4.NewEnterpriseClient(baseURL+"/api/graphql", httpClient)
	return NewGithubSCM(graphql, baseURL, githubHostName, httpClient)
}

type repoQueryResult struct {
	Search struct {
		// RepositoryCount is how many repos match the query, which can be more than
		// the search will hand over; see searchResultCap.
		RepositoryCount int
		Edges           []repoQueryEdge
		PageInfo        queryPageInfo
	} `graphql:"search(query: $query, type: REPOSITORY, first: 100, after: $tagsCursor)"`
}

type repoQueryEdge struct {
	Node struct {
		Repo struct {
			URL githubv4.URI
		} `graphql:"... on Repository"`
	}
}

type queryPageInfo struct {
	EndCursor   githubv4.String
	HasNextPage bool
}

// searchResultCap is how many results one search hands over however far a caller
// pages. Past it the search reports no next page rather than an error, so a query
// matching more repos than this loses the remainder silently.
const searchResultCap = 1000

// Retrieves all golang repos. Returns results as slice of "orgname/reponame".
//
// A search hands over at most searchResultCap repos, so this searches windows of
// creation time instead of once for everything, halving a window that matches more
// than the cap until each holds fewer. GitHub reports how many repos a query
// matches even when it won't hand them all over, which is what makes the halving
// answer to a measurement rather than a guess.
func (scm *GithubSCM) GoRepos(ctx context.Context) ([]string, error) {
	// Wide enough on both sides that no repo falls outside: earlier than GitHub
	// itself, and far enough ahead to include repos created during the search.
	earliest := time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

	names, matched, err := scm.goReposCreatedBetween(ctx, earliest, latest)
	if err != nil {
		return nil, err
	}

	// The windows don't overlap, so a repeat means one repo was handed over twice.
	results := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		results = append(results, name)
	}

	if len(results) < matched {
		slog.Warn(fmt.Sprintf("Found %d Go repos but GitHub reports %d match; some are missing from the index", len(results), matched))
	}
	return results, nil
}

// goReposCreatedBetween returns the Go repos created from and to inclusive, and how
// many GitHub reports match that window. A window matching more than searchResultCap
// is halved, since paging cannot reach past the cap. A one-second window over the
// cap cannot be halved any further, so it comes back short.
func (scm *GithubSCM) goReposCreatedBetween(ctx context.Context, from, to time.Time) (names []string, matched int, _ error) {
	query := fmt.Sprintf("language:golang created:%s..%s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	names, matched, err := scm.searchRepos(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	if matched <= searchResultCap {
		return names, matched, nil
	}
	if !from.Before(to) {
		slog.Warn(fmt.Sprintf("%d Go repos were created in the one second at %s, which is more than a search can return; some are missing from the index", matched, from.Format(time.RFC3339)))
		return names, matched, nil
	}

	// Halve on a whole second, the precision GitHub records creation times at, so
	// the two windows are adjacent with no instant falling between them.
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
	earlier, _, err := scm.goReposCreatedBetween(ctx, from, mid)
	if err != nil {
		return nil, 0, err
	}
	later, _, err := scm.goReposCreatedBetween(ctx, mid.Add(time.Second), to)
	if err != nil {
		return nil, 0, err
	}
	return append(earlier, later...), matched, nil
}

// searchRepos runs one repo search, paging to the end of what it will hand over.
// matched is how many repos GitHub reports the query matches, which can be more.
func (scm *GithubSCM) searchRepos(ctx context.Context, searchQuery string) (names []string, matched int, _ error) {
	variables := map[string]any{
		"query":      githubv4.String(searchQuery),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var q repoQueryResult
	for {
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, 0, fmt.Errorf("error querying repositories: %v", err)
		}

		for _, edge := range q.Search.Edges {
			corpName := strings.TrimPrefix(string(edge.Node.Repo.URL.String()), fmt.Sprintf("https://%s/", scm.githubHostName))
			names = append(names, string(corpName))
		}

		if !q.Search.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Search.PageInfo.EndCursor)
	}

	return names, q.Search.RepositoryCount, nil
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
