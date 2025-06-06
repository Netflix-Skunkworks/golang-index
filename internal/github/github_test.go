package github

import (
	"context"
	"net/url"
	"reflect"
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
	sut := NewGithubSCM(&mockGithubClient{}, testGithubHostname)
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
				"https://github.somecompany.net/corp/ftl-proxy",
				"https://github.somecompany.net/corp/cloudgaming-ocgactl",
				"https://github.somecompany.net/corp/cloudgaming-moby-fork",
			},
			hasNextPage: true,
			endCursor:   "somecursor",
		},
		{
			reposURLs: []string{
				"https://github.somecompany.net/corp/cloudgaming-tdd-grafana",
				"https://github.somecompany.net/corp/cloudgaming-game-input-go",
				"https://github.somecompany.net/corp/cpie-proxyd",
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		response := buildRepoQueryResult(t, response.reposURLs, response.endCursor, response.hasNextPage)
		stubbedResponses = append(stubbedResponses, response)
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, testGithubHostname)

	gotResults, err := sut.GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	wantResults := []string{
		"corp/ftl-proxy",
		"corp/cloudgaming-ocgactl",
		"corp/cloudgaming-moby-fork",
		"corp/cloudgaming-tdd-grafana",
		"corp/cloudgaming-game-input-go",
		"corp/cpie-proxyd",
	}

	if diff := cmp.Diff(wantResults, gotResults); diff != "" {
		t.Errorf("unexpected results from repos: -want +got: %s", diff)
	}
}

func TestTagsForRepo_EmptyResponse(t *testing.T) {
	sut := NewGithubSCM(&mockGithubClient{}, testGithubHostname)
	got, err := sut.TagsForRepo(t.Context(), "corp/repo1")
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
				{tag: "_gheMigrationPR-435", committedDate: date, goModContent: "module stash.corp.company.com/corp/repo1\n"},
				{tag: "_gheMigrationPR-436", committedDate: date},
				{tag: "_gheMigrationPR-437", committedDate: date},
			},
			endCursor:   "somecursor",
			hasNextPage: true,
		},
		{
			tags: []tagResponse{
				{tag: "_gheMigrationPR-438", committedDate: date},
				{tag: "_gheMigrationPR-439", committedDate: date, goModContent: "module stash.corp.company.com/corp/repo1\n"},
				{tag: "_gheMigrationPR-430", committedDate: date},
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponse(t, response.tags, response.endCursor, response.hasNextPage))
		for _, tag := range response.tags {
			stubbedResponses = append(stubbedResponses, buildGoModQueryResult(t, tag.goModContent))
		}
	}

	wantTags := []*RepoTag{
		{Tag: "_gheMigrationPR-435", TagDate: date, ModulePath: "stash.corp.company.com/corp/repo1"},
		{Tag: "_gheMigrationPR-436", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
		{Tag: "_gheMigrationPR-437", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
		{Tag: "_gheMigrationPR-438", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
		{Tag: "_gheMigrationPR-439", TagDate: date, ModulePath: "stash.corp.company.com/corp/repo1"},
		{Tag: "_gheMigrationPR-430", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, testGithubHostname)
	gotTags, err := sut.TagsForRepo(t.Context(), "corp/repo1")
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("unexpected tags: -want, +got: %s", diff)
	}
}

func TestTagsForRepo_HandlesCommitsAndAnnotatedTags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	responses := []struct {
		tags []tagResponse
	}{
		{
			tags: []tagResponse{
				{tag: "_gheMigrationPR-435", committedDate: date, goModContent: "module stash.corp.company.com/corp/repo1\n"},
				{tag: "_gheMigrationPR-436", taggerDate: date},
				{tag: "_gheMigrationPR-437", taggerDate: date},
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponse(t, response.tags, "", false))
		for _, tag := range response.tags {
			stubbedResponses = append(stubbedResponses, buildGoModQueryResult(t, tag.goModContent))
		}
	}

	wantTags := []*RepoTag{
		{Tag: "_gheMigrationPR-435", TagDate: date, ModulePath: "stash.corp.company.com/corp/repo1"},
		{Tag: "_gheMigrationPR-436", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
		{Tag: "_gheMigrationPR-437", TagDate: date, ModulePath: "github.somecompany.net/corp/repo1"},
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, testGithubHostname)
	gotTags, err := sut.TagsForRepo(t.Context(), "corp/repo1")
	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(wantTags, gotTags); diff != "" {
		t.Errorf("unexpected tags: -want, +got: %s", diff)
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

func buildTagQueryResponse(t *testing.T, tags []tagResponse, endCursor githubv4.String, hasNextPage bool) tagQueryResponse {
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

func buildGoModQueryResult(t *testing.T, rootGoModContents string) goModQueryResult {
	t.Helper()

	var q goModQueryResult
	if rootGoModContents != "" {
		q.Repository.RootGoMod.Blob.Text = rootGoModContents
	}
	return q
}
