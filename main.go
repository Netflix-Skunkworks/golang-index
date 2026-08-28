package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/indexer"
	"golang.org/x/sync/errgroup"
)

var port = flag.Int("port", 8081, "port to listen on")
var githubHostName = flag.String("githubHostName", "", "github host to query. should be your enterprise host - ex: github.mycompany.net")
var githubBaseURL = flag.String("githubBaseURL", "", "base URL for API and raw requests, e.g. https://gitproxy.mycompany.net to route through a proxy. defaults to https://<githubHostName>. module paths and repo URLs always use -githubHostName")

var githubAuthToken = flag.String("githubAuthToken", "", "github personal access token. alternative to -githubTLSClientCertFile/-githubTLSClientKeyFile")
var githubTLSClientCertFile = flag.String("githubTLSClientCertFile", "", "client certificate for mutual-TLS auth to the github host. alternative to -githubAuthToken")
var githubTLSClientKeyFile = flag.String("githubTLSClientKeyFile", "", "client key for mutual-TLS auth to the github host")
var githubTLSCACertFile = flag.String("githubTLSCACertFile", "", "optional CA bundle to verify the github host's server certificate")

var allOwnersReindexWorkCheckPeriod = flag.Duration("allOwnersReindexWorkCheckPeriod", 5*time.Minute, "duration describing the frequency to poll for work")
var allOwnersReindexPeriod = flag.Duration("allOwnersReindexPeriod", 24*time.Hour, "duration between re-indexing list of all repository owners")
var allOwnersReindexTTL = flag.Duration("allOwnersReindexTTL", 30*time.Minute, "TTL that an indexing worker has for re-indexing list of all repository owners. Must exceed how long it takes to list every account on the github host")

var ownerReposReindexingWorkCheckPeriod = flag.Duration("ownerReposReindexingWorkCheckPeriod", 5*time.Minute, "duration describing the frequency to poll for work. only occurs when no work is found: if work was previously found, instant eager re-poll occurs. note that a 1-60s jitter is added to this duration")
var ownerReposReindexingWorkers = flag.Int("ownerReposReindexingWorkers", 10, "number of workers that concurrently perform owner repo re-indexing")
var ownerReposReindexPeriod = flag.Duration("ownerReposReindexPeriod", 24*time.Hour, "duration between re-indexing all Go repos for a particular owner")
var ownerReposReindexTTL = flag.Duration("ownerReposReindexTTL", time.Hour, "TTL that an indexing worker has for re-indexing all Go repos for a particular owner. Must exceed how long it takes to page through the repos of the largest owner on the github host")

var repoTagsReindexingWorkCheckPeriod = flag.Duration("repoTagsReindexingWorkCheckPeriod", 5*time.Minute, "duration describing the frequency to poll for work. only occurs when no work is found: if work was previously found, instant eager re-poll occurs. note that a 1-60s jitter is added to this duration")
var repoTagsReindexingWorkers = flag.Int("repoTagsReindexingWorkers", 10, "number of workers that concurrently perform repo tag re-indexing")
var repoTagsReindexPeriod = flag.Duration("repoTagsReindexPeriod", 24*time.Hour, "duration between re-indexing all tags for a particular repo")
var repoTagsReindexTTL = flag.Duration("repoTagsReindexTTL", 10*time.Minute, "TTL that an indexing worker has for re-indexing all tags for a particular repo")

func main() {
	flag.Parse()

	if *githubHostName == "" {
		slog.Info("--githubHostName is required (no http/https: github.mycompany.net)")
		os.Exit(1)
	}

	httpClient, err := newGithubHTTPClient()
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	ctx := context.Background()

	pgUsername, pgPassword, pgHost, pgPort, pgDbname, err := postgresDetails()
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	idb, err := db.NewDB(pgUsername, pgPassword, pgHost, pgPort, pgDbname)
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}

	baseURL := *githubBaseURL
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://%s", *githubHostName)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	githubSCM := github.NewEnterpriseSCM(baseURL, httpClient)

	server := newServer(*port, idb, *githubHostName)

	grp, grpCtx := errgroup.WithContext(ctx)

	allOwnersIndexer := &indexer.AllOwnersIndexer{
		DB:              idb,
		Lister:          githubSCM,
		WorkCheckPeriod: *allOwnersReindexWorkCheckPeriod,
		ReindexTTL:      *allOwnersReindexTTL,
		ReindexPeriod:   *allOwnersReindexPeriod,
	}
	grp.Go(func() error { return allOwnersIndexer.Run(grpCtx) })

	for workerID := range *ownerReposReindexingWorkers {
		ownerReposIndexer := &indexer.OwnerReposIndexer{
			DB:              idb,
			Lister:          githubSCM,
			WorkerID:        workerID,
			WorkCheckPeriod: *ownerReposReindexingWorkCheckPeriod,
			ReindexTTL:      *ownerReposReindexTTL,
			ReindexPeriod:   *ownerReposReindexPeriod,
		}
		grp.Go(func() error { return ownerReposIndexer.Run(grpCtx) })
	}

	for workerID := range *repoTagsReindexingWorkers {
		repoTagsIndexer := &indexer.RepoTagsIndexer{
			DB:                idb,
			SCM:               githubSCM,
			DefaultModuleHost: *githubHostName,
			WorkerID:          workerID,
			WorkCheckPeriod:   *repoTagsReindexingWorkCheckPeriod,
			ReindexTTL:        *repoTagsReindexTTL,
			ReindexPeriod:     *repoTagsReindexPeriod,
		}
		grp.Go(func() error { return repoTagsIndexer.Run(grpCtx) })
	}
	go func() {
		// TODO(jbarkhuysen): Split out the http.Handler and then put this in a grp.Go.
		if err := server.listenAndServe(); err != nil {
			panic(err)
		}
	}()

	if err := grp.Wait(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	slog.Info("shutting down gracefully")
}

func newGithubHTTPClient() (*http.Client, error) {
	switch {
	case *githubTLSClientCertFile != "" || *githubTLSClientKeyFile != "":
		if *githubTLSClientCertFile == "" || *githubTLSClientKeyFile == "" {
			return nil, fmt.Errorf("both -githubTLSClientCertFile and -githubTLSClientKeyFile are required for mutual-TLS auth")
		}
		slog.Info("github auth: mutual TLS", "host", *githubHostName, "clientCert", *githubTLSClientCertFile, "caCert", *githubTLSCACertFile)
		return github.MTLSClient(*githubTLSClientCertFile, *githubTLSClientKeyFile, *githubTLSCACertFile)
	case *githubAuthToken != "":
		slog.Info("github auth: personal access token", "host", *githubHostName)
		return github.TokenClient(*githubAuthToken), nil
	default:
		// No in-app auth: requests go out unauthenticated. Use this when a proxy
		// in front of the host (see -githubBaseURL) supplies the credentials.
		slog.Info("github auth: none (delegated to -githubBaseURL proxy)", "host", *githubHostName)
		return &http.Client{}, nil
	}
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
