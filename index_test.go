package main

import (
	"context"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shurcooL/githubv4"
)

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

func TestRepos_EmptyResponse(t *testing.T) {
	index := newIndex(&mockGithubClient{})
	resultsChan := make(chan string)

	if err := index.repos(t.Context(), resultsChan); err != nil {
		t.Fatal(err)
	}

	if len(resultsChan) != 0 {
		t.Errorf("expected channel to be empty but it has %d results", len(resultsChan))
	}
}

func TestRepos_MultiplePages(t *testing.T) {
	responses := []struct {
		reposURLs   []string
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			reposURLs: []string{
				"https://github.netflix.net/corp/ftl-proxy",
				"https://github.netflix.net/corp/cloudgaming-ocgactl",
				"https://github.netflix.net/corp/cloudgaming-moby-fork",
			},
			hasNextPage: true,
			endCursor:   "somecursor",
		},
		{
			reposURLs: []string{
				"https://github.netflix.net/corp/cloudgaming-tdd-grafana",
				"https://github.netflix.net/corp/cloudgaming-game-input-go",
				"https://github.netflix.net/corp/cpie-proxyd",
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		response := buildRepoQueryResult(t, response.reposURLs, response.endCursor, response.hasNextPage)
		stubbedResponses = append(stubbedResponses, response)
	}

	index := newIndex(&mockGithubClient{stubbedResults: stubbedResponses})
	resultsChan := make(chan string, 100)

	if err := index.repos(t.Context(), resultsChan); err != nil {
		t.Fatal(err)
	}

	var results []string
	for r := range resultsChan {
		results = append(results, r)
	}

	wantResults := []string{
		"corp/ftl-proxy",
		"corp/cloudgaming-ocgactl",
		"corp/cloudgaming-moby-fork",
		"corp/cloudgaming-tdd-grafana",
		"corp/cloudgaming-game-input-go",
		"corp/cpie-proxyd",
	}

	if diff := cmp.Diff(wantResults, results); diff != "" {
		t.Errorf("unexpected results from repos: -want +got: %s", diff)
	}
}

func TestTagsForRepos_EmptyResponse(t *testing.T) {
	repos := make(chan string, 1)
	repos <- "corp/repo1"
	close(repos)

	index := newIndex(&mockGithubClient{})
	if err := index.tagsForRepos(t.Context(), repos); err != nil {
		t.Fatal(err)
	}

	gotTags := index.repoTags["corp/repo1"]

	if len(gotTags) != 0 {
		t.Errorf("expected no tags, but got %d results", len(gotTags))
	}
}

func TestTagsForRepos_MultiplePages(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	responses := []struct {
		tags        []repoTag
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			tags: []repoTag{
				{tag: "_gheMigrationPR-435", commitDate: date},
				{tag: "_gheMigrationPR-436", commitDate: date},
				{tag: "_gheMigrationPR-437", commitDate: date},
			},
			endCursor:   "somecursor",
			hasNextPage: true,
		},
		{
			tags: []repoTag{
				{tag: "_gheMigrationPR-438", commitDate: date},
				{tag: "_gheMigrationPR-439", commitDate: date},
				{tag: "_gheMigrationPR-430", commitDate: date},
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponse(response.tags, response.endCursor, response.hasNextPage))
	}

	wantTags := map[string][]*repoTag{
		"corp/repo1": {
			{tag: "_gheMigrationPR-435", commitDate: date},
			{tag: "_gheMigrationPR-436", commitDate: date},
			{tag: "_gheMigrationPR-437", commitDate: date},
			{tag: "_gheMigrationPR-438", commitDate: date},
			{tag: "_gheMigrationPR-439", commitDate: date},
			{tag: "_gheMigrationPR-430", commitDate: date},
		},
	}

	repos := make(chan string, 1)
	repos <- "corp/repo1"
	close(repos)

	index := newIndex(&mockGithubClient{stubbedResults: stubbedResponses})
	if err := index.tagsForRepos(t.Context(), repos); err != nil {
		t.Fatal(err)
	}

	if !cmp.Equal(wantTags, index.repoTags, cmp.AllowUnexported(repoTag{})) {
		t.Errorf("wanted tags: %v, got: %v", wantTags, index.repoTags)
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

func buildTagQueryResponse(tags []repoTag, endCursor githubv4.String, hasNextPage bool) tagQueryResponse {
	var edges []tagQueryEdge

	for _, tag := range tags {
		var edge tagQueryEdge
		edge.Node.Name = githubv4.String(tag.tag)
		edge.Node.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.commitDate})
		edges = append(edges, edge)
	}

	var q tagQueryResponse
	q.Repository.Refs.Edges = edges
	q.Repository.Refs.PageInfo.EndCursor = endCursor
	q.Repository.Refs.PageInfo.HasNextPage = hasNextPage
	return q
}
