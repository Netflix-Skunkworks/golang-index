package main

import (
	"context"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/shurcooL/githubv4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockGithubClient struct {
	stubResultID int
	stubResult   []interface{}
}

func (m *mockGithubClient) Query(ctx context.Context, q interface{}, variables map[string]interface{}) error {
	if len(m.stubResult) == 0 {
		return nil
	}

	stubQueryResponse := reflect.ValueOf(q)
	stubQueryResponse = stubQueryResponse.Elem()
	stubQueryResponse.Set(reflect.ValueOf(m.stubResult[m.stubResultID]))
	m.stubResultID++
	return nil
}

func TestQueryingRepos(t *testing.T) {
	type responses []struct {
		reposURLs   []string
		endCursor   githubv4.String
		hasNextPage bool
	}

	tests := []struct {
		name        string
		responses   responses
		wantResults []string
	}{
		{
			name:        "empty response",
			wantResults: []string{},
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
					hasNextPage: false,
					endCursor:   "",
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
					hasNextPage: false,
					endCursor:   "",
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stubResponses []interface{}
			for _, response := range tt.responses {
				stubResponses = append(stubResponses, buildRepoQueryResponse(t, response.reposURLs, response.endCursor, response.hasNextPage))
			}

			index := newIndex(context.Background(), &mockGithubClient{stubResult: stubResponses})

			resultsChan := make(chan string, len(tt.wantResults))
			index.repos(context.Background(), resultsChan)

			results := []string{}
			for r := range resultsChan {
				results = append(results, r)
			}

			assert.Equal(t, tt.wantResults, results)
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

	tests := []struct {
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
					endCursor:   "",
					hasNextPage: false,
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
					endCursor:   "",
					hasNextPage: false,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repos := make(chan string, 3)
			for _, repo := range []string{"corp/repo1"} {
				repos <- repo
			}
			close(repos)

			var stubResponses []interface{}
			for _, response := range tt.responses {
				stubResponses = append(stubResponses, buildTagsQueryResponse(t, response.tags, response.endCursor, response.hasNextPage))
			}

			index := newIndex(context.Background(), &mockGithubClient{stubResult: stubResponses})
			err := index.tagsForRepos(context.Background(), repos)
			require.NoError(t, err)

			assert.Equal(t, tt.wantTags, index.repoTags)
		})
	}
}

func buildRepoQueryResponse(t *testing.T, reposURLs []string, endCursor githubv4.String, hasNextPage bool) interface{} {
	var edges []struct {
		Node struct {
			Repo struct {
				URL githubv4.URI
			} `graphql:"... on Repository"`
		}
	}

	for _, repoURL := range reposURLs {
		edge := struct {
			Node struct {
				Repo struct {
					URL githubv4.URI
				} `graphql:"... on Repository"`
			}
		}{}

		url, err := url.Parse(repoURL)
		require.NoError(t, err)
		edge.Node.Repo.URL = *githubv4.NewURI(githubv4.URI{URL: url})
		edges = append(edges, edge)
	}

	q := struct {
		Search struct {
			Edges []struct {
				Node struct {
					Repo struct {
						URL githubv4.URI
					} `graphql:"... on Repository"`
				}
			}
			PageInfo struct {
				EndCursor   githubv4.String
				HasNextPage bool
			}
		} `graphql:"search(query: $query, type: REPOSITORY, first: 100, after: $tagsCursor)"`
	}{}

	q.Search.Edges = edges
	q.Search.PageInfo.EndCursor = endCursor
	q.Search.PageInfo.HasNextPage = hasNextPage

	return q
}

func buildTagsQueryResponse(t *testing.T, tags []repoTag, endCursor githubv4.String, hasNextPage bool) interface{} {
	var edges []struct {
		Node struct {
			Name   githubv4.String
			Target struct {
				Commit struct {
					AbbreviatedOid githubv4.String
					CommittedDate  githubv4.DateTime
				} `graphql:"... on Commit"`
			}
		}
	}

	for _, tag := range tags {
		edge := struct {
			Node struct {
				Name   githubv4.String
				Target struct {
					Commit struct {
						AbbreviatedOid githubv4.String
						CommittedDate  githubv4.DateTime
					} `graphql:"... on Commit"`
				}
			}
		}{}

		edge.Node.Name = githubv4.String(tag.tag)
		edge.Node.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.commitDate})

		edges = append(edges, edge)
	}

	q := struct {
		Repository struct {
			Refs struct {
				Edges []struct {
					Node struct {
						Name   githubv4.String
						Target struct {
							Commit struct {
								AbbreviatedOid githubv4.String
								CommittedDate  githubv4.DateTime
							} `graphql:"... on Commit"`
						}
					}
				}
				PageInfo struct {
					EndCursor   githubv4.String
					HasNextPage bool
				}
			} `graphql:"refs(refPrefix: \"refs/tags/\", orderBy: {field: TAG_COMMIT_DATE, direction: DESC}, first: 100, after: $tagsCursor)"`
		} `graphql:"repository(owner: $repoOrg, name: $repoName)"`
	}{}

	q.Repository.Refs.Edges = edges
	q.Repository.Refs.PageInfo.EndCursor = endCursor
	q.Repository.Refs.PageInfo.HasNextPage = hasNextPage

	return q
}
