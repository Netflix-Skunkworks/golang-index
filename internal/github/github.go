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
	"golang.org/x/mod/semver"
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
	// baseURL is where requests are sent; it may be a proxy in front of the
	// GitHub Enterprise host, so it can differ from githubHostName.
	baseURL string
	// githubHostName is used for module paths and repo URLs, not for connecting.
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

// A module version parsed from a repo's git tag, plus when the tag was made.
type RepoTag struct {
	Version    string
	TagDate    time.Time
	ModulePath string
}

// moduleVersionFromTag reads a git tag as a Go module version. A bare "vX.Y.Z"
// tag is the root module; a "<subdir>/vX.Y.Z" tag is the module in <subdir> (Go
// treats the tag prefix as the module's path under the repo root). ok is false
// when the version isn't canonical semver, like a "v1"/"v2" pointer or a tag
// with build metadata, which don't name a real release.
func moduleVersionFromTag(tag string) (subdir, version string, ok bool) {
	version = tag
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		subdir, version = tag[:i], tag[i+1:]
	}
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return "", "", false
	}
	return subdir, version, true
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
			rawTag := string(t.Node.Name)
			subdir, version, ok := moduleVersionFromTag(rawTag)
			if !ok {
				slog.Debug(fmt.Sprintf("skipping tag %q for %s: not a module version", rawTag, repo.fullName()))
				continue
			}

			var tag RepoTag
			tag.Version = version

			// Lightweight tags point directly to commits and have
			// `committedDate` timestamp stored on them directly. Annotated
			// tags do not have a committedDate and instead store their
			// creation timestamp in the `tag.tagger.date` field. This logic is
			// needed so we correctly set tag date for both types of tags.
			if !t.Node.Target.Commit.CommittedDate.IsZero() {
				tag.TagDate = t.Node.Target.Commit.CommittedDate.UTC()
			} else if !t.Node.Target.Tag.Tagger.Date.IsZero() {
				tag.TagDate = t.Node.Target.Tag.Tagger.Date.UTC()
			}

			modulePath := repo.asModulePath()
			if subdir != "" {
				modulePath += "/" + subdir
			}

			goModModulePath, found, err := scm.modulePathFromGoMod(ctx, repo, rawTag, subdir)
			switch {
			case err != nil:
				slog.Error(fmt.Sprintf("error getting go.mod file for %s (tag %s): %v. Defaulting to github url for module path", repo.fullName(), rawTag, err))
			case found:
				modulePath = goModModulePath
			default:
				slog.Info(fmt.Sprintf("unable to find go.mod file for %s (tag %s). Defaulting to github url for module path", repo.fullName(), rawTag))
			}

			// Skip a version whose major doesn't match its module path (say a v2+
			// tag on a path with no /vN suffix). The real index wouldn't have it.
			if err := module.Check(modulePath, version); err != nil {
				slog.Debug(fmt.Sprintf("skipping tag %q for %s: %v", rawTag, repo.fullName(), err))
				continue
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

// modulePathFromGoMod reads the module path declared in the go.mod at subdir
// (the repo root when subdir is ""), at the commit the tag points to. We trust
// that path over one built from the repo URL: it's the only place that shows a
// /vN suffix or a vanity/moved path (e.g. a module that switched VCS host but
// kept its old path). found is false, with a nil error, when there's no go.mod
// or it has no module line, so the caller falls back to the repo URL.
func (scm *GithubSCM) modulePathFromGoMod(ctx context.Context, repo repo, tag, subdir string) (string, bool, error) {
	goModPath := "go.mod"
	if subdir != "" {
		goModPath = subdir + "/go.mod"
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/raw/%s/%s/%s/%s", scm.baseURL, repo.org, repo.name, tag, goModPath),
		nil,
	)
	if err != nil {
		return "", false, fmt.Errorf("error building raw github API request: %v", err)
	}

	resp, err := scm.httpClient.Do(request)
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

	modulePath := modfile.ModulePath(bodyBytes)
	if modulePath == "" {
		return "", false, nil
	}

	return modulePath, true, nil
}
