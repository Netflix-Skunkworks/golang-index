// Package github implements github querying logic.
package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/shurcooL/githubv4"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// githubClient wraps query interface from the shurcooL/githubv4 package so
// that we can mock github graphql query responses in tests.
type githubClient interface {
	// Matches https://pkg.go.dev/github.com/shurcooL/githubv4#Client.Query
	Query(ctx context.Context, query any, variables map[string]any) error
}

// A handle for specialised github querying.
type GithubSCM struct {
	graphqlClient   githubClient
	githubHostName  string
	githubAuthToken string
	useRawHTTPS     bool
}

// Creates a new Github SCM.
func NewGithubSCM(client githubClient, githubHostName, githubAuthToken string, useRawHTTPS bool) *GithubSCM {
	return &GithubSCM{graphqlClient: client,
		githubHostName:  githubHostName,
		githubAuthToken: githubAuthToken,
		useRawHTTPS:     useRawHTTPS,
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
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
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
	Tag        string
	TagDate    time.Time
	ModulePath string
}

// Retrieves all tags for a given repo.
func (scm *GithubSCM) TagsForRepo(ctx context.Context, orgRepoName string) ([]*RepoTag, error) {
	var q tagQueryResponse

	repo, err := newRepo(scm.githubHostName, orgRepoName)
	if err != nil {
		return nil, fmt.Errorf("TagsForRepo: %v", err)
	}

	variables := map[string]any{
		"repoOrg":    githubv4.String(repo.org),
		"repoName":   githubv4.String(repo.name),
		"tagsCursor": (*githubv4.String)(nil),
	}

	var results []*RepoTag
	// Page through all the results.
	for {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := scm.graphqlClient.Query(queryCtx, &q, variables); err != nil {
			return nil, fmt.Errorf("error querying tags for %s: %w", repo.fullName(), err)
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
				tag.TagDate = t.Node.Target.Commit.CommittedDate.UTC()
			} else if !t.Node.Target.Tag.Tagger.Date.IsZero() {
				tag.TagDate = t.Node.Target.Tag.Tagger.Date.UTC()
			}

			modulePath := repo.asModulePath()

			goModModulePath, found, err := scm.modulePathFromGoMod(ctx, repo, tag.Tag)
			if err != nil {
				// if go.mod file was found but turned out to be invalid, we want to skip the tag entirely
				if found {
					slog.Error(fmt.Sprintf("found go.mod file for %s but it's invalid: %v. Skipping the tag", repo.fullName(), err))
					continue
				}

				slog.Error(fmt.Sprintf("error getting go.mod file for %s: %v. Defaulting to github url for module path", repo.fullName(), err))
			}

			if found {
				modulePath = goModModulePath
			} else {
				slog.Info(fmt.Sprintf("unable to find go.mod file in the root of the project for %s. Defaulting to github url for module path", repo.fullName()))
			}

			tag.ModulePath = modulePath
			results = append(results, &tag)
		}

		if !q.Repository.Refs.PageInfo.HasNextPage {
			break
		}

		variables["tagsCursor"] = githubv4.NewString(q.Repository.Refs.PageInfo.EndCursor)
	}

	return results, nil
}

// goModForRepo retrieves go.mod file for the repository so that we can inspect
// its content and determine if the module path matches the repo URL or if the
// module path is different and needs to be updated in the index. The latter
// commonly occurs when a module has been migrated from one vcs to another
// without changing the module path.
func (scm *GithubSCM) modulePathFromGoMod(ctx context.Context, repo repo, tag string) (string, bool, error) {
	protocol := "http://"
	if scm.useRawHTTPS {
		protocol = "https://"
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s%s/raw/%s/%s/%s/go.mod", protocol, scm.githubHostName, repo.org, repo.name, tag),
		nil,
	)
	if err != nil {
		return "", false, fmt.Errorf("error building raw github API request: %v", err)
	}
	request.Header.Set("Authorization", fmt.Sprintf("token %s", scm.githubAuthToken))

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", false, fmt.Errorf("error querying raw github API for go.mod contents: %v", err)
	}
	defer resp.Body.Close()

	// we expect 404 to be returned for a lot of repos which don't have go.mod
	// file in the root of the directory. This avoid extra noise in logs by not
	// logging such case as an error.
	if resp.StatusCode == 404 {
		return "", false, nil
	}

	if resp.StatusCode != 200 {
		return "", false, fmt.Errorf("unexpected status code from raw github API. Status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, fmt.Errorf("error reading raw github API response: %v", err)
	}

	file, err := modfile.Parse("go.mod", bodyBytes, nil)
	if err != nil {
		return "", false, fmt.Errorf("error parsing go.mod file for %s (tag: %s): %v", repo.fullName(), tag, err)
	}

	if file.Module != nil {
		err := module.CheckPath(file.Module.Mod.Path)
		if err != nil {
			return "", true, fmt.Errorf("invalid module path found for %s (tag: %s): %v", repo.fullName(), tag, err)
		}

		return file.Module.Mod.Path, true, nil
	}

	return "", false, nil
}
