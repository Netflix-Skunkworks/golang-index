package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func TestTagsForRepo_EmptyResponse(t *testing.T) {
	sut := NewGithubSCM(&mockGithubClient{}, "", testGithubHostname, nil)
	got, err := sut.TagsForRepo(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no tags, but got %d results", len(got))
	}
}

func TestTagsForRepo_MultiplePages(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	responses := []struct {
		tags        []tagResponse
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			tags: []tagResponse{
				{tag: "v1.0.0", committedDate: date, goModContent: "module stash.someorg.company.com/someorg/repo1\n"},
				{tag: "v1.1.0", committedDate: date},
				{tag: "v1.2.0", committedDate: date},
			},
			endCursor:   "somecursor",
			hasNextPage: true,
		},
		{
			tags: []tagResponse{
				{tag: "v1.3.0", committedDate: date},
				{tag: "v1.4.0", committedDate: date, goModContent: "module invalid/module/path"},
				{tag: "v0.9.0", committedDate: date},
			},
		},
	}

	authToken := "test-token"
	var tagResponses []tagResponse
	for _, resp := range responses {
		tagResponses = append(tagResponses, resp.tags...)
	}
	server := createTestGoModServer(t, authToken, tagResponses)
	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponses(t, response.tags, response.endCursor, response.hasNextPage))
	}

	wantTags := []*RepoTag{
		{Version: "v1.0.0", TagDate: date, ModulePath: "stash.someorg.company.com/someorg/repo1"},
		{Version: "v1.1.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
		{Version: "v1.2.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
		{Version: "v1.3.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
		// v1.4.0's go.mod has a bad module path, so it's skipped.
		{Version: "v0.9.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, server.URL, testGithubHostname, TokenClient(authToken))
	gotTags, err := sut.TagsForRepo(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("TagsForRepo(%q) mismatch (-want +got):\n%s", "someorg/repo1", diff)
	}
}

func TestTagsForRepo_HandlesCommitsAndAnnotatedTags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	tags := []tagResponse{
		{tag: "v1.0.0", committedDate: date, goModContent: "module stash.someorg.company.com/someorg/repo1\n"},
		{tag: "v1.1.0", taggerDate: date},
		{tag: "v1.2.0", taggerDate: date},
	}

	gotTags := runTagsForRepo(t, tags)

	wantTags := []*RepoTag{
		{Version: "v1.0.0", TagDate: date, ModulePath: "stash.someorg.company.com/someorg/repo1"},
		{Version: "v1.1.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
		{Version: "v1.2.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("TagsForRepo(%q) mismatch (-want +got):\n%s", "someorg/repo1", diff)
	}
}

func TestTagsForRepo_SubdirectoryAndNonModuleTags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	tags := []tagResponse{
		{tag: "v1.0.0", committedDate: date},
		{tag: "tracing/v0.2.2", committedDate: date},
		{tag: "cmd/tool/v0.1.0", committedDate: date},
		// Valid semver but not canonical (vN, vN.N pointers, build metadata): skipped.
		{tag: "v1", committedDate: date},
		{tag: "v2", committedDate: date},
		{tag: "v1.2", committedDate: date},
		{tag: "tracing/v2", committedDate: date},
		{tag: "v1.0.0+incompatible", committedDate: date},
		// Not semver at all: skipped.
		{tag: "_gheMigrationPR-435", committedDate: date},
		{tag: "docs/latest", committedDate: date},
		{tag: "slides/2", committedDate: date},
	}

	gotTags := runTagsForRepo(t, tags)

	wantTags := []*RepoTag{
		{Version: "v1.0.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
		{Version: "v0.2.2", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1/tracing"},
		{Version: "v0.1.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1/cmd/tool"},
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("TagsForRepo(%q) mismatch (-want +got):\n%s", "someorg/repo1", diff)
	}
}

func TestTagsForRepo_SubdirectoryModuleGoMod(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	tags := []tagResponse{
		// Subdir module at v2+: keep the /v2 from its go.mod.
		{tag: "tracing/v2.0.0", committedDate: date, goModContent: "module go.example.com/monorepo/tracing/v2\n"},
		// go.mod declares a vanity/moved path: use it as-is.
		{tag: "auth/v1.4.0", committedDate: date, goModContent: "module vanity.example.com/auth\n"},
		// No go.mod: fall back to the path from the repo URL.
		{tag: "metrics/v0.5.0", committedDate: date},
	}

	gotTags := runTagsForRepo(t, tags)

	wantTags := []*RepoTag{
		{Version: "v2.0.0", TagDate: date, ModulePath: "go.example.com/monorepo/tracing/v2"},
		{Version: "v1.4.0", TagDate: date, ModulePath: "vanity.example.com/auth"},
		{Version: "v0.5.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1/metrics"},
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("TagsForRepo(%q) mismatch (-want +got):\n%s", "someorg/repo1", diff)
	}
}

func TestTagsForRepo_SkipsMajorVersionMismatch(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	tags := []tagResponse{
		// Root v2+ with no go.mod: path has no /v2 suffix, so skip it.
		{tag: "v2.0.0", committedDate: date},
		// Root v2+ but the go.mod path is v0/v1, so skip it.
		{tag: "v2.1.0", committedDate: date, goModContent: "module vanity.example.com/thing\n"},
		// Subdir v3 tag but the go.mod is /v2, so skip it.
		{tag: "tracing/v3.0.0", committedDate: date, goModContent: "module go.example.com/monorepo/tracing/v2\n"},
		// A plain v1 tag still comes through.
		{tag: "v1.5.0", committedDate: date},
	}

	gotTags := runTagsForRepo(t, tags)

	wantTags := []*RepoTag{
		{Version: "v1.5.0", TagDate: date, ModulePath: testGithubHostname + "/someorg/repo1"},
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("TagsForRepo(%q) mismatch (-want +got):\n%s", "someorg/repo1", diff)
	}
}

func TestModuleVersionFromTag(t *testing.T) {
	type result struct {
		subdir  string
		version string
		ok      bool
	}
	tests := []struct {
		name string
		tag  string
		want result
	}{
		{name: "root version", tag: "v1.0.0", want: result{version: "v1.0.0", ok: true}},
		{name: "root prerelease", tag: "v1.0.0-rc.1", want: result{version: "v1.0.0-rc.1", ok: true}},
		{name: "root major version 2", tag: "v2.3.4", want: result{version: "v2.3.4", ok: true}},
		{name: "subdir version", tag: "tracing/v0.2.2", want: result{subdir: "tracing", version: "v0.2.2", ok: true}},
		{name: "nested subdir version", tag: "cmd/tool/v0.1.0", want: result{subdir: "cmd/tool", version: "v0.1.0", ok: true}},
		{name: "major-only pointer", tag: "v1", want: result{}},
		{name: "major-only pointer v2", tag: "v2", want: result{}},
		{name: "minor-only pointer", tag: "v1.2", want: result{}},
		{name: "subdir major-only pointer", tag: "tracing/v2", want: result{}},
		{name: "build metadata", tag: "v1.0.0+incompatible", want: result{}},
		{name: "missing v prefix", tag: "1.0.0", want: result{}},
		{name: "not a version", tag: "_gheMigrationPR-435", want: result{}},
		{name: "subdir non-version", tag: "docs/latest", want: result{}},
		{name: "bare number", tag: "slides/2", want: result{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subdir, version, ok := moduleVersionFromTag(tc.tag)
			got := result{subdir: subdir, version: version, ok: ok}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(result{})); diff != "" {
				t.Errorf("moduleVersionFromTag(%q) mismatch (-want +got):\n%s", tc.tag, diff)
			}
		})
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

// runTagsForRepo runs TagsForRepo against a test go.mod server built from a
// single page of tags. The server's host:port is the baseURL we send requests
// to; the module host is testGithubHostname. They differ on purpose, like in
// production where baseURL is often a proxy in front of the real GHE host.
func runTagsForRepo(t *testing.T, tags []tagResponse) []*RepoTag {
	t.Helper()

	const authToken = "test-token"
	server := createTestGoModServer(t, authToken, tags)
	stubbedResponses := []any{buildTagQueryResponses(t, tags, "", false)}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, server.URL, testGithubHostname, TokenClient(authToken))
	got, err := sut.TagsForRepo(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}
	return got
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
// for a "<subdir>/vX.Y.Z" tag. It matches the URL modulePathFromGoMod builds.
func goModRequestPath(tag string) string {
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		return tag + "/" + tag[:i] + "/go.mod"
	}
	return tag + "/go.mod"
}
