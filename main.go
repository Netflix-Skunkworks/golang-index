package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

var port = flag.Int("port", 8081, "port to listen on")
var githubHostName = flag.String("githubHostName", "", "github host to query. should be your enterprise host - ex: github.mycompany.net")
var githubAuthToken = flag.String("githubAuthToken", "", "github auth token")

var allReposReindexWorkCheckPeriod = flag.Duration("allReposReindexWorkCheckPeriod", 5*time.Minute, "duration describing the frequency to poll for work")
var allReposReindexPeriod = flag.Duration("allReposReindexPeriod", 24*time.Hour, "duration between re-indexing list of all repos")
var allReposReindexTTL = flag.Duration("allReposReindexTTL", 5*time.Minute, "TTL that an indexing worker has for re-indexing list of all repos")

var repoTagsReindexingWorkCheckPeriod = flag.Duration("repoTagsReindexingWorkCheckPeriod", 5*time.Minute, "duration describing the frequency to poll for work. only occurs when no work is found: if work was previously found, instant eager re-poll occurs. note that a 1-60s jitter is added to this duration")
var repoTagsReindexingWorkers = flag.Int("repoTagsReindexingWorkers", 10, "number of workers that concurrently perform repo tag re-indexing")
var repoTagsReindexPeriod = flag.Duration("repoTagsReindexPeriod", 24*time.Hour, "duration between re-indexing all tags for a particular repo")
var repoTagsReindexTTL = flag.Duration("repoTagsReindexTTL", 10*time.Minute, "TTL that an indexing worker has for re-indexing all tags for a particular repo")

func main() {
	flag.Parse()

	if *githubHostName == "" || *githubAuthToken == "" {
		fmt.Println("--githubHostName (no http/https: github.mycompany.net) and --githubAuthToken are required")
		os.Exit(1)
	}

	ctx := context.Background()

	pgUsername, pgPassword, pgHost, pgPort, pgDbname, err := postgresDetails()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	idb, err := db.NewDB(pgUsername, pgPassword, pgHost, pgPort, pgDbname)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fullHost := fmt.Sprintf("https://%s/api/graphql", *githubHostName)
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: *githubAuthToken})
	graphqlClient := githubv4.NewEnterpriseClient(fullHost, oauth2.NewClient(ctx, src))

	githubSCM := github.NewGithubSCM(graphqlClient)

	server := newServer(*port, idb)

	grp, grpCtx := errgroup.WithContext(ctx)

	// TODO(jbarkhuysen): This should probably be in a function that's tested.
	grp.Go(func() error {
		// Periodically re-index all repos.
		for {
			shouldReindex, err := idb.NextReindexAllReposWork(grpCtx, *allReposReindexTTL, *allReposReindexPeriod)
			if err != nil {
				return fmt.Errorf("error fetching next reindex all repos work: %v", err)
			}
			if !shouldReindex {
				fmt.Println("should re-index all Go repos: no. waiting 5m to try again")
				// Wait and check again.
				select {
				case <-time.After(*allReposReindexWorkCheckPeriod):
				case <-grpCtx.Done():
					return grpCtx.Err()
				}
				continue
			}
			fmt.Println("should re-index all Go repos: yes")
			allRepos, err := githubSCM.GoRepos(grpCtx)
			if err != nil {
				// TODO(issues/21): Handle 429 Too Many requests by performing exponential backoff.
				return fmt.Errorf("error fetching all Go repos: %v", err)
			}
			if err := idb.StoreRepos(ctx, allRepos); err != nil {
				return fmt.Errorf("error storing all repos: %v", err)
			}
			fmt.Printf("finished re-indexing all Go repos. saw %d repos\n", len(allRepos))

			// No point in eagerly checking for new work: there's only one work
			// item and we just worked on it.
			select {
			case <-time.After(*allReposReindexWorkCheckPeriod):
			case <-grpCtx.Done():
				return grpCtx.Err()
			}
		}
	})
	for workerID := range *repoTagsReindexingWorkers {
		workerID := workerID

		// TODO(jbarkhuysen): This should probably be in a function that's tested.
		grp.Go(func() error {
			// Periodically re-index a repo's tags.
			for {
				repoToReindex, gotWork, err := idb.NextReindexRepoTagsWork(grpCtx, *repoTagsReindexTTL, *repoTagsReindexPeriod)
				if err != nil {
					return fmt.Errorf("error fetching next reindex repo tags work: %v", err)
				}
				if !gotWork {
					// Wait with (1s-60s) jitter and check again.
					jitter := time.Duration((rand.Intn(60) + 1) * 1e9)
					waitTime := *repoTagsReindexingWorkCheckPeriod + jitter
					fmt.Printf("repo tags re-indexing worker %d: no work, waiting %v to try again\n", workerID, waitTime)
					select {
					case <-time.After(waitTime):
					case <-grpCtx.Done():
						return grpCtx.Err()
					}
					continue
				}
				fmt.Printf("repo tags re-indexing worker %d: got work for repo %s\n", workerID, repoToReindex)
				repoTags, err := githubSCM.TagsForRepo(grpCtx, repoToReindex)
				if err != nil {
					// TODO(issues/21): Handle 429 Too Many requests by performing exponential backoff.
					return fmt.Errorf("erroring fetching all repo tags: %v", err)
				}
				if len(repoTags) == 0 {
					continue
				}
				var dbRepoTags []*db.RepoTag
				for _, rt := range repoTags {
					dbRepoTags = append(dbRepoTags, &db.RepoTag{OrgRepoName: repoToReindex, TagName: rt.Tag, Created: rt.TagDate})
				}
				fmt.Printf("repo tags re-indexing worker %d: finished re-indexing repo %s, got %d tags... storing results\n", workerID, repoToReindex, len(repoTags))
				if err := idb.StoreRepoTags(grpCtx, dbRepoTags); err != nil {
					return fmt.Errorf("error storing repo tags: %v", err)
				}
				fmt.Printf("repo tags re-indexing worker %d: finished re-indexing repo %s, got %d tags... done\n", workerID, repoToReindex, len(repoTags))

				// Eagerly check for new work rather than waiting again.
			}
		})
	}
	go func() {
		// TODO(jbarkhuysen): Split out the http.Handler and then put this in a grp.Go.
		if err := server.listenAndServe(); err != nil {
			panic(err)
		}
	}()

	if err := grp.Wait(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("shutting down gracefully")
}

func postgresDetails() (username string, password string, host string, port uint16, dbname string, _ error) {
	username = os.Getenv("POSTGRES_USERNAME")
	if username == "" {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_USERNAME is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB")
	}
	password = os.Getenv("POSTGRES_PASSWORD")
	if password == "" {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_PASSWORD is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB")
	}
	host = os.Getenv("POSTGRES_HOST")
	if host == "" {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_HOST is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB")
	}
	portStr := os.Getenv("POSTGRES_PORT")
	if portStr == "" {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_PORT is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB")
	}
	portUint64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_PORT is invalid: %v", err)
	}
	dbname = os.Getenv("POSTGRES_DB")
	if dbname == "" {
		return "", "", "", 0, "", fmt.Errorf("POSTGRES_DB is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB")
	}

	return username, password, host, uint16(portUint64), dbname, nil
}
