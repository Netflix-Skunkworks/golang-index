package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

var port = flag.Int("port", 8081, "port to listen on")
var githubHostName = flag.String("githubHostName", "", "github host to query. should be your enterprise host - ex: github.mycompany.net")
var githubAuthToken = flag.String("githubAuthToken", "", "github auth token")
var reindexIntervalHours = flag.Int("reindexHours", 12, "number of hours to wait between each re-indexing")

func main() {
	flag.Parse()

	if *githubHostName == "" || *githubAuthToken == "" {
		fmt.Println("--githubHostName (no http/https: github.mycompany.net) and --githubAuthToken are required")
		os.Exit(1)
	}

	ctx := context.Background()
	fullHost := fmt.Sprintf("https://%s/api/graphql", *githubHostName)
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: *githubAuthToken})
	graphqlClient := githubv4.NewEnterpriseClient(fullHost, oauth2.NewClient(ctx, src))

	index := newIndex(graphqlClient)
	if err := index.build(ctx); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	server := newServer(*port, index)

	ticker := time.NewTicker(time.Duration(*reindexIntervalHours) * time.Hour)
	go func() {
		for range ticker.C {
			fmt.Println("starting new reindexing")

			updatedIndex := newIndex(graphqlClient)
			if err := updatedIndex.build(ctx); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			server.updateIndex(updatedIndex)

			fmt.Println("updated server index")
		}
	}()

	if err := server.listenAndServe(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
