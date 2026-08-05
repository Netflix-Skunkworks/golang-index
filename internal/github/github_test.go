package github

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shurcooL/githubv4"
)

const testGithubHostname = "github.somecompany.net"

type mockGithubClient struct {
	// index pointer for the stubResults slice
	resultsIdx int

	// stubbed results for queries
	stubbedResults []any
}

func (m *mockGithubClient) Query(ctx context.Context, query any, variables map[string]any) error {
	if len(m.stubbedResults) == 0 {
		return nil
	}
	if m.resultsIdx >= len(m.stubbedResults) {
		return fmt.Errorf("mockGithubClient: query #%d has no stubbed result (%d stubbed)", m.resultsIdx+1, len(m.stubbedResults))
	}

	// GitHub GraphQL client works by populating fields of the struct q with
	// the query response. Here, we mock that behavior by instead using the
	// stub results stored in the stubbedResults slice. We use a slice so that
	// we could stub multiple request/response cycles for testing paging
	// behavior. resultsIdx keeps track of which step in the multistep query
	// we're in.

	stubQueryResponse := reflect.ValueOf(query)
	stubQueryResponse = stubQueryResponse.Elem()
	stubQueryResponse.Set(reflect.ValueOf(m.stubbedResults[m.resultsIdx]))
	m.resultsIdx++
	return nil
}

func TestGoRepos_EmptyResponse(t *testing.T) {
	sut := NewGithubSCM(&mockGithubClient{}, "", testGithubHostname, nil)
	resultsChan := make(chan string)
	got, err := sut.GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected channel to be empty but it has %d results", len(resultsChan))
	}
}

func TestGoRepos_MultiplePages(t *testing.T) {
	responses := []struct {
		reposURLs   []string
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			reposURLs: []string{
				"https://github.somecompany.net/someorg/ftl-proxy",
				"https://github.somecompany.net/someorg/cloudgaming-ocgactl",
				"https://github.somecompany.net/someorg/cloudgaming-moby-fork",
			},
			hasNextPage: true,
			endCursor:   "somecursor",
		},
		{
			reposURLs: []string{
				"https://github.somecompany.net/someorg/cloudgaming-tdd-grafana",
				"https://github.somecompany.net/someorg/cloudgaming-game-input-go",
				"https://github.somecompany.net/someorg/cpie-proxyd",
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		response := buildRepoQueryResult(t, response.reposURLs, response.endCursor, response.hasNextPage)
		stubbedResponses = append(stubbedResponses, response)
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, "", testGithubHostname, nil)

	gotResults, err := sut.GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	wantResults := []string{
		"someorg/ftl-proxy",
		"someorg/cloudgaming-ocgactl",
		"someorg/cloudgaming-moby-fork",
		"someorg/cloudgaming-tdd-grafana",
		"someorg/cloudgaming-game-input-go",
		"someorg/cpie-proxyd",
	}

	if diff := cmp.Diff(wantResults, gotResults); diff != "" {
		t.Errorf("unexpected results from repos: -want +got: %s", diff)
	}
}

func TestGoRepos_SplitsWindowsOverTheResultCap(t *testing.T) {
	// A search hands over at most searchResultCap repos and says nothing about the
	// rest, so GoRepos narrows the creation-time window until every search matches
	// fewer than that. However the windows end up drawn, every repo has to come back
	// exactly once: a gap between two windows loses repos, an overlap repeats them.
	createdAt := make(map[string]time.Time, 3*searchResultCap)
	for i := range 3 * searchResultCap {
		// Spread over years, and in pairs sharing a second, so the halving has to
		// recurse a long way and still separate repos it cannot split apart.
		name := fmt.Sprintf("someorg/repo%04d", i)
		createdAt[name] = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i/2) * time.Hour)
	}

	search := &windowedRepoSearch{t: t, createdAt: createdAt, resultCap: searchResultCap}
	got, err := NewGithubSCM(search, "", testGithubHostname, nil).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if want := slices.Sorted(maps.Keys(createdAt)); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("GoRepos() returned %d repos, want the %d created: %s", len(got), len(want), firstDifference(want, slices.Sorted(slices.Values(got))))
	}
	if len(search.windows) < 2 {
		t.Errorf("GoRepos() searched %d windows, want more than one: the halving never ran", len(search.windows))
	}
}

func TestGoRepos_OneSecondOverTheResultCapComesBackShort(t *testing.T) {
	// More repos created in the same second than a search will hand over is the one
	// case narrowing the window cannot fix, since there is no smaller window to
	// narrow to. GoRepos returns what it can rather than failing, because a short
	// repo list still indexes.
	sameInstant := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	createdAt := make(map[string]time.Time, searchResultCap+1)
	for i := range searchResultCap + 1 {
		createdAt[fmt.Sprintf("someorg/repo%04d", i)] = sameInstant
	}

	search := &windowedRepoSearch{t: t, createdAt: createdAt, resultCap: searchResultCap}
	got, err := NewGithubSCM(search, "", testGithubHostname, nil).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != searchResultCap {
		t.Errorf("GoRepos() returned %d repos, want %d: all %d share a second, so only the cap can come back", len(got), searchResultCap, len(createdAt))
	}
}

// firstDifference describes where two sorted repo lists first disagree, so a
// failure names one repo rather than printing thousands.
func firstDifference(want, got []string) string {
	for i := range min(len(want), len(got)) {
		if want[i] != got[i] {
			return fmt.Sprintf("first difference at %d: want %q, got %q", i, want[i], got[i])
		}
	}
	if len(want) == len(got) {
		return "no difference"
	}
	if len(want) > len(got) {
		return fmt.Sprintf("missing from %d onwards, starting with %q", len(got), want[len(got)])
	}
	return fmt.Sprintf("unexpected from %d onwards, starting with %q", len(want), got[len(want)])
}

func TestRepoTags_EmptyResponse(t *testing.T) {
	sut := NewGithubSCM(&mockGithubClient{}, "", testGithubHostname, nil)
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no tags, but got %d results", len(got))
	}
}

func TestRepoTags_MultiplePages(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	responses := []struct {
		tags        []tagResponse
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			tags: []tagResponse{
				{tag: "v1.0.0", committedDate: date},
				{tag: "v1.1.0", committedDate: date},
			},
			endCursor:   "somecursor",
			hasNextPage: true,
		},
		{
			tags: []tagResponse{
				{tag: "v1.2.0", committedDate: date},
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponses(t, response.tags, response.endCursor, response.hasNextPage))
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, "", testGithubHostname, nil)
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}

	want := []Tag{
		{Name: "v1.0.0", Date: date},
		{Name: "v1.1.0", Date: date},
		{Name: "v1.2.0", Date: date},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoTags mismatch (-want +got):\n%s", diff)
	}
}

func TestRepoTags_CommitAndTaggerDates(t *testing.T) {
	committed := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tagged := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)

	stubbed := []any{buildTagQueryResponses(t, []tagResponse{
		{tag: "v1.0.0", committedDate: committed},
		{tag: "v1.1.0", taggerDate: tagged},
	}, "", false)}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbed}, "", testGithubHostname, nil)
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}

	want := []Tag{
		{Name: "v1.0.0", Date: committed},
		{Name: "v1.1.0", Date: tagged},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoTags mismatch (-want +got):\n%s", diff)
	}
}

func TestHeadCommit(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"

	tests := []struct {
		name          string
		response      headQueryResponse
		wantOID       string
		wantCommitted time.Time
	}{
		{
			name:          "has commit",
			response:      buildHeadQueryResponse(oid, commitDate),
			wantOID:       oid,
			wantCommitted: commitDate,
		},
		{
			name:     "empty repo has no commit",
			response: buildHeadQueryResponse("", time.Time{}),
			wantOID:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sut := NewGithubSCM(&mockGithubClient{stubbedResults: []any{tc.response}}, "", testGithubHostname, nil)

			gotOID, gotCommitted, err := sut.HeadCommit(t.Context(), "someorg/repo1")
			if err != nil {
				t.Fatal(err)
			}
			if gotOID != tc.wantOID {
				t.Errorf("HeadCommit(%q) oid = %q, want %q", "someorg/repo1", gotOID, tc.wantOID)
			}
			if !gotCommitted.Equal(tc.wantCommitted) {
				t.Errorf("HeadCommit(%q) committedAt = %v, want %v", "someorg/repo1", gotCommitted, tc.wantCommitted)
			}
		})
	}
}

func TestGoMod(t *testing.T) {
	const authToken = "test-token"
	const goMod = "module go.example.com/thing\n"

	server := createTestGoModServer(t, authToken, []tagResponse{{tag: "v1.0.0", goModContent: goMod}})
	sut := NewGithubSCM(nil, server.URL, testGithubHostname, TokenClient(authToken))

	t.Run("found", func(t *testing.T) {
		content, found, err := sut.GoMod(t.Context(), "someorg/repo1", "v1.0.0", "")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("expected go.mod to be found")
		}
		if got := string(content); got != goMod {
			t.Errorf("GoMod content = %q, want %q", got, goMod)
		}
	})

	t.Run("missing is not an error", func(t *testing.T) {
		_, found, err := sut.GoMod(t.Context(), "someorg/repo1", "v9.9.9", "")
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Error("expected missing go.mod to report found=false")
		}
	})
}

func TestModuleDirs(t *testing.T) {
	const oid = "abcdef0123456789abcdef0123456789abcdef01"
	const treeJSON = `{
		"tree": [
			{"path": "go.mod", "type": "blob"},
			{"path": "README.md", "type": "blob"},
			{"path": "tracing", "type": "tree"},
			{"path": "tracing/go.mod", "type": "blob"},
			{"path": "cmd", "type": "tree"},
			{"path": "cmd/tool", "type": "tree"},
			{"path": "cmd/tool/go.mod", "type": "blob"},
			{"path": "vendor/example.com/dep/go.mod", "type": "blob"},
			{"path": "testdata/fixture/go.mod", "type": "blob"}
		],
		"truncated": false
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := fmt.Sprintf("/api/v3/repos/someorg/repo1/git/trees/%s", oid); r.URL.Path != want {
			t.Errorf("request path = %q, want %q", r.URL.Path, want)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("recursive"); got != "1" {
			t.Errorf("recursive query param = %q, want %q", got, "1")
		}
		if _, err := w.Write([]byte(treeJSON)); err != nil {
			t.Errorf("writing tree response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	sut := NewGithubSCM(nil, server.URL, testGithubHostname, &http.Client{})
	got, err := sut.ModuleDirs(t.Context(), "someorg/repo1", oid)
	if err != nil {
		t.Fatal(err)
	}

	// Root and both submodules are modules; vendor and testdata are skipped.
	want := []string{"", "tracing", "cmd/tool"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ModuleDirs mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleDirs_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	sut := NewGithubSCM(nil, server.URL, testGithubHostname, &http.Client{})
	got, err := sut.ModuleDirs(t.Context(), "someorg/repo1", "deadbeef")
	if err != nil {
		t.Fatalf("ModuleDirs returned error for a 404, want none: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ModuleDirs = %v, want no dirs for a 404", got)
	}
}

func buildRepoQueryResult(t *testing.T, reposURLs []string, endCursor githubv4.String, hasNextPage bool) repoQueryResult {
	t.Helper()

	var edges []repoQueryEdge

	for _, repoURL := range reposURLs {
		var edge repoQueryEdge
		url, err := url.Parse(repoURL)
		if err != nil {
			t.Fatalf("error parsing repo url: %v", err)
		}

		edge.Node.Repo.URL = *githubv4.NewURI(githubv4.URI{URL: url})
		edges = append(edges, edge)
	}

	var q repoQueryResult
	q.Search.Edges = edges
	q.Search.PageInfo.EndCursor = endCursor
	q.Search.PageInfo.HasNextPage = hasNextPage
	return q
}

// windowedRepoSearch answers repo searches the way GitHub's does: it honours the
// created: window in the query, reports how many repos match it, and hands over at
// most resultCap of them without saying it withheld any.
type windowedRepoSearch struct {
	t *testing.T
	// createdAt maps a repo's "org/name" to when it was created.
	createdAt map[string]time.Time
	// resultCap stands in for searchResultCap, low enough that a test can exceed it.
	resultCap int
	// windows records the created: window of every search issued, in order, as
	// "<from>..<to>".
	windows []string
}

func (s *windowedRepoSearch) Query(_ context.Context, query any, variables map[string]any) error {
	s.t.Helper()

	searchQuery := string(variables["query"].(githubv4.String))
	_, window, ok := strings.Cut(searchQuery, "created:")
	if !ok {
		s.t.Fatalf("search query %q has no created: window", searchQuery)
	}
	s.windows = append(s.windows, window)

	from, to, ok := strings.Cut(window, "..")
	if !ok {
		s.t.Fatalf("created: window %q is not a range", window)
	}
	fromTime, err := time.Parse(time.RFC3339, from)
	if err != nil {
		s.t.Fatalf("parsing window start %q: %v", from, err)
	}
	toTime, err := time.Parse(time.RFC3339, to)
	if err != nil {
		s.t.Fatalf("parsing window end %q: %v", to, err)
	}

	var matching []string
	for name, created := range s.createdAt {
		if !created.Before(fromTime) && !created.After(toTime) {
			matching = append(matching, name)
		}
	}
	slices.Sort(matching)

	result := query.(*repoQueryResult)
	*result = buildRepoQueryResult(s.t, repoURLs(matching[:min(len(matching), s.resultCap)]), "", false)
	result.Search.RepositoryCount = len(matching)
	return nil
}

// repoURLs renders "org/name" repos as the URLs a search returns them at.
func repoURLs(orgRepoNames []string) []string {
	urls := make([]string, 0, len(orgRepoNames))
	for _, name := range orgRepoNames {
		urls = append(urls, "https://"+testGithubHostname+"/"+name)
	}
	return urls
}

type tagResponse struct {
	tag           string
	goModContent  string
	committedDate time.Time
	taggerDate    time.Time
}

func buildTagQueryResponses(t *testing.T, tags []tagResponse, endCursor githubv4.String, hasNextPage bool) tagQueryResponse {
	t.Helper()

	var edges []tagQueryEdge

	for _, tag := range tags {
		var edge tagQueryEdge
		edge.Node.Name = githubv4.String(tag.tag)
		if !tag.committedDate.IsZero() {
			edge.Node.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.committedDate})
		}
		if !tag.taggerDate.IsZero() {
			edge.Node.Target.Tag.Tagger.Date = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.taggerDate})
		}
		edges = append(edges, edge)
	}

	var q tagQueryResponse
	q.Repository.Refs.Edges = edges
	q.Repository.Refs.PageInfo.EndCursor = endCursor
	q.Repository.Refs.PageInfo.HasNextPage = hasNextPage

	return q
}

// buildHeadQueryResponse stubs the default branch's HEAD commit. A zero-value
// oid stands in for an empty repo with no default branch.
func buildHeadQueryResponse(oid string, committedDate time.Time) headQueryResponse {
	var q headQueryResponse
	q.Repository.DefaultBranchRef.Target.Commit.OID = githubv4.GitObjectID(oid)
	if !committedDate.IsZero() {
		q.Repository.DefaultBranchRef.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: committedDate})
	}
	return q
}

func createTestGoModServer(t *testing.T, authToken string, tags []tagResponse) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("token %s", authToken) {
			http.Error(w, "wrong Authorization header", http.StatusUnauthorized)
			return
		}

		for _, tag := range tags {
			if tag.goModContent == "" {
				continue
			}
			if strings.HasSuffix(r.URL.Path, "/"+goModRequestPath(tag.tag)) {
				if _, err := w.Write([]byte(tag.goModContent)); err != nil {
					// Can't call t.Fatal off the test goroutine, so use Errorf.
					t.Errorf("writing go.mod for %q: %v", tag.tag, err)
				}
				return
			}
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	return server
}

// goModRequestPath is the "<ref>/<path>" part of the go.mod URL for a tag: the
// root go.mod at "<tag>/go.mod", or the subdir go.mod at "<tag>/<subdir>/go.mod"
// for a "<subdir>/vX.Y.Z" tag. It matches the URL GoMod builds.
func goModRequestPath(tag string) string {
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		return tag + "/" + tag[:i] + "/go.mod"
	}
	return tag + "/go.mod"
}
