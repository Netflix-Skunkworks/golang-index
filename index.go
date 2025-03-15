package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shurcooL/githubv4"
)

type githubClient interface {
	Query(ctx context.Context, q interface{}, variables map[string]interface{}) error
}

type index struct {
	// v4 API
	graphqlClient githubClient

	mu sync.RWMutex
	// Map of repo name to tags.
	repoTags map[string][]*repoTag
}

type repoTag struct {
	tag        string
	commitDate time.Time
}

func newIndex(ctx context.Context, client githubClient) *index {
	return &index{
		graphqlClient: client,
		repoTags:      make(map[string][]*repoTag),
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

// Get all of the golang repos.
//
// Only one of this function should be run at a time.
func (i *index) repos(ctx context.Context, results chan<- string) error {
	defer close(results)

	var q repoQueryResult

	variables := map[string]interface{}{
		"query":      githubv4.String("language:golang"),
		"tagsCursor": (*githubv4.String)(nil),
	}

	for {
		if err := i.graphqlClient.Query(ctx, &q, variables); err != nil {
			return fmt.Errorf("error querying repositories: %w", err)
		}

		fmt.Printf("received %d repo results from github!\n", len(q.Search.Edges))

		for _, edge := range q.Search.Edges {
			corpName := strings.TrimPrefix(string(edge.Node.Repo.URL.URL.String()), "https://github.netflix.net/")
			results <- string(corpName)
		}

		if !q.Search.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Search.PageInfo.EndCursor)
	}

	return nil
}

// Get all the tags for the repos.
//
// Multiple of this function can be run concurrently. Each invocation pulls a
// different repo from the queue and works on it independently.
func (i *index) tagsForRepos(ctx context.Context, repos <-chan string) error {
	for {
		repoName, more := <-repos
		fmt.Println("looking for tags for", repoName)
		if !more {
			fmt.Println("done looking for tags")
			break
		}

		tags, err := i.tagsForRepo(ctx, repoName)
		if err != nil {
			return fmt.Errorf("error getting tags for %s: %w", repoName, err)
		}
		fmt.Printf("got %d tags for %s\n", len(tags), repoName)

		// TODO(jeanbza): If we get a lot of lock contention, consider
		// batching this.
		i.mu.Lock()
		i.repoTags[repoName] = tags
		i.mu.Unlock()
	}

	return nil
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
				AbbreviatedOid githubv4.String
				CommittedDate  githubv4.DateTime
			} `graphql:"... on Commit"`
		}
	}
}

func (i *index) tagsForRepo(ctx context.Context, repoName string) ([]*repoTag, error) {
	var q tagQueryResponse

	parts := strings.Split(repoName, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("expected org/name format, but got %d parts from %s", len(parts), repoName)
	}

	variables := map[string]interface{}{
		"repoOrg":    githubv4.String(parts[0]),
		"repoName":   githubv4.String(parts[1]),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var tags []*repoTag
	// Page through all the results.
	for {
		if err := i.graphqlClient.Query(ctx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying tags for %s: %w", repoName, err)
		}
		for _, t := range q.Repository.Refs.Edges {
			tags = append(tags, &repoTag{tag: string(t.Node.Name), commitDate: t.Node.Target.Commit.CommittedDate.Time})
		}
		if !q.Repository.Refs.PageInfo.HasNextPage {
			break
		}
		variables["tagsCursor"] = githubv4.NewString(q.Repository.Refs.PageInfo.EndCursor)
	}

	return tags, nil
}
