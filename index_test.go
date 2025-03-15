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
	stubbedResults []interface{}
}

func (m *mockGithubClient) Query(ctx context.Context, q interface{}, variables map[string]interface{}) error {
	if len(m.stubbedResults) == 0 {
		return nil
	}

	// GitHub GraphQL client works by populating fields of the struct q with
	// the query response. Here, we mock that behavior by instead using the
	// stub results stored in the stubbedResults slice. We use a slice so that
	// we could stub multiple request/response cycles for testing paging
	// behavior. resultsIdx keeps track of which step in the multistep query
	// we're in.

	stubQueryResponse := reflect.ValueOf(q)
	stubQueryResponse = stubQueryResponse.Elem()
	stubQueryResponse.Set(reflect.ValueOf(m.stubbedResults[m.resultsIdx]))
	m.resultsIdx++
	return nil
}

func TestQueryingRepos(t *testing.T) {
	type responses []struct {
		reposURLs   []string
		endCursor   githubv4.String
		hasNextPage bool
	}

	for _, tc := range []struct {
		name        string
		responses   responses
		wantResults []string
	}{
		{
			name: "empty response",
		},
		{
			name: "single page",
			responses: responses{
				{
					reposURLs: []string{
						"https://github.netflix.net/corp/ftl-proxy",
						"https://github.netflix.net/corp/cloudgaming-ocgactl",
						"https://github.netflix.net/corp/cloudgaming-moby-fork",
					},
				},
			},
			wantResults: []string{
				"corp/ftl-proxy",
				"corp/cloudgaming-ocgactl",
				"corp/cloudgaming-moby-fork",
			},
		},
		{
			name: "multiple pages",
			responses: responses{
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
			},
			wantResults: []string{
				"corp/ftl-proxy",
				"corp/cloudgaming-ocgactl",
				"corp/cloudgaming-moby-fork",
				"corp/cloudgaming-tdd-grafana",
				"corp/cloudgaming-game-input-go",
				"corp/cpie-proxyd",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stubbedResponses []interface{}
			for _, response := range tc.responses {
				response, err := buildRepoQueryResponse(response.reposURLs, response.endCursor, response.hasNextPage)
				if err != nil {
					t.Fatalf("unexpected error building query response: %v", err)
				}
				stubbedResponses = append(stubbedResponses, response)
			}

			index := newIndex(context.Background(), &mockGithubClient{stubbedResults: stubbedResponses})
			resultsChan := make(chan string, len(tc.wantResults))

			index.repos(context.Background(), resultsChan)

			var results []string
			for r := range resultsChan {
				results = append(results, r)
			}

			if !cmp.Equal(tc.wantResults, results) {
				t.Errorf("want results: %v, got: %v", tc.wantResults, results)
			}
		})
	}
}

func TestQueryingTags(t *testing.T) {
	type responses []struct {
		tags        []repoTag
		endCursor   githubv4.String
		hasNextPage bool
	}

	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)

	for _, tc := range []struct {
		name      string
		responses responses
		wantTags  map[string][]*repoTag
	}{
		{
			name: "empty response",
			wantTags: map[string][]*repoTag{
				"corp/repo1": nil,
			},
		},
		{
			name: "single page",
			responses: responses{
				{
					tags: []repoTag{
						{tag: "_gheMigrationPR-435", commitDate: date},
						{tag: "_gheMigrationPR-436", commitDate: date},
						{tag: "_gheMigrationPR-437", commitDate: date},
					},
				},
			},
			wantTags: map[string][]*repoTag{
				"corp/repo1": []*repoTag{
					{tag: "_gheMigrationPR-435", commitDate: date},
					{tag: "_gheMigrationPR-436", commitDate: date},
					{tag: "_gheMigrationPR-437", commitDate: date},
				},
			},
		},
		{
			name: "multiple pages",
			responses: responses{
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
			},
			wantTags: map[string][]*repoTag{
				"corp/repo1": []*repoTag{
					{tag: "_gheMigrationPR-435", commitDate: date},
					{tag: "_gheMigrationPR-436", commitDate: date},
					{tag: "_gheMigrationPR-437", commitDate: date},
					{tag: "_gheMigrationPR-438", commitDate: date},
					{tag: "_gheMigrationPR-439", commitDate: date},
					{tag: "_gheMigrationPR-430", commitDate: date},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repos := make(chan string, 1)
			repos <- "corp/repo1"
			close(repos)

			var stubbedResponses []interface{}
			for _, response := range tc.responses {
				stubbedResponses = append(stubbedResponses, buildTagsQueryResponse(response.tags, response.endCursor, response.hasNextPage))
			}

			index := newIndex(context.Background(), &mockGithubClient{stubbedResults: stubbedResponses})
			err := index.tagsForRepos(context.Background(), repos)
			if err != nil {
				t.Fatalf("unexpected error generating tags for repos: %v", err)
			}

			if !cmp.Equal(tc.wantTags, index.repoTags, cmp.AllowUnexported(repoTag{})) {
				t.Errorf("wanted tags: %v, got: %v", tc.wantTags, index.repoTags)
			}
		})
	}
}

func buildRepoQueryResponse(reposURLs []string, endCursor githubv4.String, hasNextPage bool) (interface{}, error) {
	var edges []repoQueryEdge

	for _, repoURL := range reposURLs {
		var edge repoQueryEdge
		url, err := url.Parse(repoURL)
		if err != nil {
			return nil, err
		}

		edge.Node.Repo.URL = *githubv4.NewURI(githubv4.URI{URL: url})
		edges = append(edges, edge)
	}

	var q repoQueryResult
	q.Search.Edges = edges
	q.Search.PageInfo.EndCursor = endCursor
	q.Search.PageInfo.HasNextPage = hasNextPage
	return q, nil
}

func buildTagsQueryResponse(tags []repoTag, endCursor githubv4.String, hasNextPage bool) interface{} {
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
