// Package github implements github querying logic.
package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying repositories: %w", err)
		}

		for _, edge := range q.Search.Edges {
			corpName := strings.TrimPrefix(string(edge.Node.Repo.URL.String()), fmt.Sprintf("https://%s/", scm.githubHostName))
			results = append(results, string(corpName))
		}

		if !q.Search.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Search.PageInfo.EndCursor)
	}

	return results, nil
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
			return nil, fmt.Errorf("error querying tags for %s: %w", repo.fullName(), err)
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
		return "", time.Time{}, fmt.Errorf("error querying HEAD for %s: %w", repo.fullName(), err)
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
