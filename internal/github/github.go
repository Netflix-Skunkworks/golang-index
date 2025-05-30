// Package github implements github querying logic.
package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
)

// githubClient wraps query interface from the shurcooL/githubv4 package so
// that we can mock github graphql query responses in tests.
type githubClient interface {
	// Matches https://pkg.go.dev/github.com/shurcooL/githubv4#Client.Query
	Query(ctx context.Context, query any, variables map[string]any) error
}

// A handle for specialised github querying.
type GithubSCM struct {
	graphqlClient githubClient
}

// Creates a new Github SCM.
func NewGithubSCM(client githubClient) *GithubSCM {
	return &GithubSCM{graphqlClient: client}
}

type repoQueryResult struct {
	Search struct {
		Edges    []repoQueryEdge
		PageInfo queryPageInfo
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

// Retrieves all golang repos. Returns results as slice of "orgname/reponame".
func (scm *GithubSCM) GoRepos(ctx context.Context) ([]string, error) {
	var results []string
	variables := map[string]any{
		"query":      githubv4.String("language:golang"),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var q repoQueryResult
	for {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying repositories: %w", err)
		}

		for _, edge := range q.Search.Edges {
			// TODO(issues/22): Make this not Netflix specific.
			corpName := strings.TrimPrefix(string(edge.Node.Repo.URL.String()), "https://github.netflix.net/")
			results = append(results, string(corpName))
		}

		if !q.Search.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Search.PageInfo.EndCursor)
	}

	return results, nil
}

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

// A repo tag and its creation date.
type RepoTag struct {
	Tag     string
	TagDate time.Time
}

// Retrieves all tags for a given repo.
func (scm *GithubSCM) TagsForRepo(ctx context.Context, orgRepoName string) ([]*RepoTag, error) {
	var q tagQueryResponse

	parts := strings.Split(orgRepoName, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("expected org/name format, but got %d parts from %s", len(parts), orgRepoName)
	}

	variables := map[string]any{
		"repoOrg":    githubv4.String(parts[0]),
		"repoName":   githubv4.String(parts[1]),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var results []*RepoTag
	// Page through all the results.
	for {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying tags for %s: %w", orgRepoName, err)
		}

		for _, t := range q.Repository.Refs.Edges {
			var tag RepoTag
			tag.Tag = string(t.Node.Name)

			// leightweight tags point directly to commits and have
			// `committedDate` timestamp stored on them directly. annotated
			// tags do not have a committedDate and instead store their
			// creation timestamp in the `tag.tagger.date` field. This logic is
			// needed so we correctly set tag date for both type of tags.
			if !t.Node.Target.Commit.CommittedDate.IsZero() {
				tag.TagDate = t.Node.Target.Commit.CommittedDate.Time
			} else if !t.Node.Target.Tag.Tagger.Date.IsZero() {
				tag.TagDate = t.Node.Target.Tag.Tagger.Date.Time
			}

			results = append(results, &tag)
		}
		if !q.Repository.Refs.PageInfo.HasNextPage {
			break
		}
		variables["tagsCursor"] = githubv4.NewString(q.Repository.Refs.PageInfo.EndCursor)
	}

	return results, nil
}
