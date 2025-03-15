package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

var port = flag.Int("port", 8081, "port to listen on")
var githubHostName = flag.String("githubHostName", "", "github host to query. should be your enterprise host - ex: github.mycompany.net")
var githubAuthToken = flag.String("githubAuthToken", "", "github auth token")

const githubResultsPerPage = 100
const tagWorkers = 10

func main() {
	flag.Parse()
	ctx := context.Background()

	if *githubHostName == "" || *githubAuthToken == "" {
		fmt.Println("--githubHostName (no http/https: github.mycompany.net) and --githubAuthToken are required")
		os.Exit(1)
	}

	fullHost := fmt.Sprintf("https://%s/api/graphql", *githubHostName)
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: *githubAuthToken})
	httpClient := oauth2.NewClient(ctx, src)
	graphqlClient := githubv4.NewEnterpriseClient(fullHost, httpClient)

	index := newIndex(ctx, graphqlClient)

	// TODO(jeanbza): This should re-run periodically.
	repoNames := make(chan string, 2*githubResultsPerPage)
	grp, grpCtx := errgroup.WithContext(ctx)
	grp.Go(func() error {
		return index.repos(grpCtx, repoNames)
	})
	for j := 0; j < tagWorkers; j++ {
		grp.Go(func() error {
			return index.tagsForRepos(grpCtx, repoNames)
		})
	}
	if err := grp.Wait(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	s := newServer(*port, index)
	s.listenAndServe()
}
