// Package github implements github querying logic.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
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
	// codeSearchOrgs are the organizations reposWithGoMod searches one at a time.
	// Empty searches the whole instance.
	codeSearchOrgs []string
}

// Creates a new Github SCM.
func NewGithubSCM(client githubClient, baseURL, githubHostName string, httpClient *http.Client, codeSearchOrgs ...string) *GithubSCM {
	return &GithubSCM{
		graphqlClient:  client,
		baseURL:        baseURL,
		githubHostName: githubHostName,
		httpClient:     httpClient,
		codeSearchOrgs: codeSearchOrgs,
	}
}

// NewEnterpriseSCM builds a GithubSCM for a GitHub Enterprise host, wiring the
// GraphQL client (at baseURL+"/api/graphql") and the raw/REST calls to the same
// httpClient. baseURL is where requests are sent (possibly a proxy);
// githubHostName is the enterprise host used for module paths and repo URLs.
// codeSearchOrgs are the organizations to search for go.mod files one at a time,
// empty for the whole instance.
func NewEnterpriseSCM(baseURL, githubHostName string, httpClient *http.Client, codeSearchOrgs ...string) *GithubSCM {
	graphql := githubv4.NewEnterpriseClient(baseURL+"/api/graphql", httpClient)
	return NewGithubSCM(graphql, baseURL, githubHostName, httpClient, codeSearchOrgs...)
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
// pages. Past it a search reports no next page rather than an error, losing the
// rest.
const searchResultCap = 1000

// Retrieves all golang repos. Returns results as slice of "orgname/reponame".
//
// The union of two searches. They overlap heavily, but neither contains the other:
// repo search knows only the language GitHub calls a repo's primary, and code search
// only finds repos holding a go.mod. Blocks for minutes, since GitHub allows few
// code searches a minute.
func (scm *GithubSCM) GoRepos(ctx context.Context) ([]string, error) {
	byLanguage, err := scm.goReposByLanguage(ctx)
	if err != nil {
		return nil, err
	}
	withGoMod, err := scm.reposWithGoMod(ctx)
	if err != nil {
		return nil, err
	}
	return dedupe(append(byLanguage, withGoMod...)), nil
}

func dedupe(orgRepoNames []string) []string {
	unique := make([]string, 0, len(orgRepoNames))
	seen := make(map[string]bool, len(orgRepoNames))
	for _, name := range orgRepoNames {
		if seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	return unique
}

// goReposByLanguage returns the repos GitHub records Go as the primary language of,
// searching windows of creation time narrow enough to stay under searchResultCap.
func (scm *GithubSCM) goReposByLanguage(ctx context.Context) ([]string, error) {
	// Wider on both sides than any repo's creation time.
	earliest := time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

	names, matched, err := scm.goReposCreatedBetween(ctx, earliest, latest)
	if err != nil {
		return nil, err
	}

	results := dedupe(names)
	if len(results) < matched {
		slog.Warn(fmt.Sprintf("Found %d repos with Go as the primary language but GitHub reports %d match; some are missing from the index", len(results), matched))
	}
	return results, nil
}

// goReposCreatedBetween returns the Go repos created from and to inclusive, and how
// many GitHub reports match. An over-cap window is halved until it isn't; a
// one-second window that still is comes back short.
func (scm *GithubSCM) goReposCreatedBetween(ctx context.Context, from, to time.Time) ([]string, int, error) {
	query := fmt.Sprintf("language:golang created:%s..%s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	// An over-cap window gets halved and its results dropped, so don't page it.
	names, matched, err := scm.searchRepos(ctx, query, from.Before(to))
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

	// Whole seconds, the precision GitHub records creation times at, so the halves
	// are adjacent with no instant between them.
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

// searchRepos runs one repo search, paging to the end of what it will hand over,
// along with the number of repos GitHub reports the query matches, which can be
// more. When stopWhenOverCap is set, no names come back once that count passes
// searchResultCap.
func (scm *GithubSCM) searchRepos(ctx context.Context, searchQuery string, stopWhenOverCap bool) ([]string, int, error) {
	var names []string
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
		if stopWhenOverCap && q.Search.RepositoryCount > searchResultCap {
			return nil, q.Search.RepositoryCount, nil
		}

		for _, edge := range q.Search.Edges {
			orgRepoName := strings.TrimPrefix(edge.Node.Repo.URL.String(), fmt.Sprintf("https://%s/", scm.githubHostName))
			names = append(names, orgRepoName)
		}

		if !q.Search.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Search.PageInfo.EndCursor)
	}

	return names, q.Search.RepositoryCount, nil
}

// largestGoModBytes ends the size range reposWithGoMod halves. No go.mod comes
// close to it.
const largestGoModBytes = 64 << 10

// codeSearchPageSize is the most results GitHub will put in one code search page.
const codeSearchPageSize = 100

// codeSearchRateLimitWaits bounds the waits for one page, so a refusal mistaken for
// a rate limit cannot loop forever.
const codeSearchRateLimitWaits = 10

// codeSearchResponse is the subset of a code search response we read.
type codeSearchResponse struct {
	TotalCount int `json:"total_count"`
	Items      []struct {
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	} `json:"items"`
}

// reposWithGoMod returns the repos holding a go.mod, at any path, as "org/name".
// One search per organization in codeSearchOrgs, since an instance-wide code search
// is more than a proxy in front of GitHub may serve. Scoping to an organization
// reaches no user-owned repo, which then only goReposByLanguage covers; with no
// organization configured, one instance-wide search runs instead.
func (scm *GithubSCM) reposWithGoMod(ctx context.Context) ([]string, error) {
	orgs := scm.codeSearchOrgs
	if len(orgs) == 0 {
		orgs = []string{""}
	}

	var names []string
	var matched int
	for _, org := range orgs {
		orgNames, orgMatched, err := scm.reposWithGoModSized(ctx, org, 0, largestGoModBytes)
		if err != nil {
			return nil, err
		}
		names = append(names, orgNames...)
		matched += orgMatched
	}

	results := dedupe(names)
	// One match per file, not per repo, so the shortfall is against the files.
	if len(names) < matched {
		slog.Warn(fmt.Sprintf("Found %d go.mod files but GitHub reports %d match; some repos are missing from the index", len(names), matched))
	}
	return results, nil
}

// reposWithGoModSized returns the repos in org holding a go.mod of from to to
// bytes inclusive, one entry per file, and how many files GitHub reports match.
// An empty org searches the whole instance. Code search's only qualifier that
// partitions go.mod files is file size, so an over-cap range is halved until it
// isn't, the way goReposByLanguage halves a time window; a single size that still
// is comes back short.
func (scm *GithubSCM) reposWithGoModSized(ctx context.Context, org string, from, to int) ([]string, int, error) {
	query := fmt.Sprintf("filename:go.mod size:%d..%d", from, to)
	if org != "" {
		query += " org:" + org
	}
	// An over-cap range gets halved and its results dropped, so don't page it.
	names, matched, err := scm.searchCode(ctx, query, from < to)
	if err != nil {
		return nil, 0, err
	}
	if matched <= searchResultCap {
		return names, matched, nil
	}
	if from >= to {
		slog.Warn(fmt.Sprintf("%d go.mod files are exactly %d bytes, which is more than a search can return; some repos are missing from the index", matched, from))
		return names, matched, nil
	}

	mid := from + (to-from)/2
	smaller, _, err := scm.reposWithGoModSized(ctx, org, from, mid)
	if err != nil {
		return nil, 0, err
	}
	larger, _, err := scm.reposWithGoModSized(ctx, org, mid+1, to)
	if err != nil {
		return nil, 0, err
	}
	return append(smaller, larger...), matched, nil
}

// searchCode runs one code search, paging to the end of what it will hand over and
// returning the repo each result sits in, along with the number of files GitHub
// reports the query matches, which can be more. When stopWhenOverCap is set, no
// names come back once that count passes searchResultCap.
func (scm *GithubSCM) searchCode(ctx context.Context, searchQuery string, stopWhenOverCap bool) ([]string, int, error) {
	var names []string
	var matched int
	// Asking past the cap is an error rather than an empty page.
	for page := 1; page <= searchResultCap/codeSearchPageSize; page++ {
		result, err := scm.codeSearchPage(ctx, searchQuery, page)
		if err != nil {
			return nil, 0, err
		}
		matched = result.TotalCount
		if stopWhenOverCap && matched > searchResultCap {
			return nil, matched, nil
		}
		for _, item := range result.Items {
			names = append(names, item.Repository.FullName)
		}
		if len(result.Items) < codeSearchPageSize {
			break
		}
	}
	return names, matched, nil
}

// codeSearchPage fetches one page of code search results, waiting out the rate
// limit when it has been spent.
func (scm *GithubSCM) codeSearchPage(ctx context.Context, searchQuery string, page int) (*codeSearchResponse, error) {
	searchURL := fmt.Sprintf("%s/api/v3/search/code?q=%s&per_page=%d&page=%d",
		scm.baseURL, url.QueryEscape(searchQuery), codeSearchPageSize, page)

	for waits := 0; ; waits++ {
		result, wait, err := scm.codeSearchAttempt(ctx, searchURL)
		if err != nil {
			return nil, err
		}
		if wait == 0 {
			return result, nil
		}
		if waits == codeSearchRateLimitWaits {
			return nil, fmt.Errorf("code search still rate limited after %d waits", waits)
		}
		slog.Info(fmt.Sprintf("Code search rate limit spent, waiting %v", wait.Round(time.Second)))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// codeSearchAttempt makes one code search request. A non-zero wait means the rate
// limiter turned it away, there is no result, and it should be repeated after that
// long.
func (scm *GithubSCM) codeSearchAttempt(ctx context.Context, searchURL string) (*codeSearchResponse, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("error building code search request: %v", err)
	}
	resp, err := scm.httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("error querying code search: %w", err)
	}
	defer resp.Body.Close()

	if wait := retryAfter(resp); wait > 0 {
		return nil, wait, nil
	}
	if resp.StatusCode != 200 {
		return nil, 0, fmt.Errorf("unexpected status code from code search. Status code: %d", resp.StatusCode)
	}

	var decoded codeSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, 0, fmt.Errorf("error decoding code search response: %v", err)
	}
	return &decoded, 0, nil
}

// retryAfter reports how long to wait before repeating a request GitHub's rate
// limiter turned away, and zero for one it did not. GitHub signals its two limits
// differently: the primary leaves no requests remaining and says when the window
// resets, the secondary sends Retry-After. A refusal carrying neither is not about
// rate, so waiting would not help.
func retryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if after, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return max(time.Duration(after)*time.Second, time.Second)
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return 0
	}
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0
	}
	return max(time.Until(time.Unix(reset, 0)), time.Second)
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
