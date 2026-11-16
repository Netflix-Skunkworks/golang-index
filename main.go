package main

import (
	"context"
	"encoding/json"
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

var githubHeaders headerFlag // -githubHeader, repeatable.
var githubOwnerHeader = flag.String("githubOwnerHeader", "", "name of a header to set, per request, to the repository owner the request concerns. for a proxy that selects credentials per organization")
var githubOwnersURL = flag.String("githubOwnersURL", "", "URL serving a JSON array of owner logins to index. when set, the host is not enumerated for owners")

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
	flag.Var(&githubHeaders, "githubHeader", "header to send on every github request, as name:value. repeatable")
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

	baseURL := *githubBaseURL
	if baseURL == "" {
		baseURL = fmt.Sprintf("https://%s", *githubHostName)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient = github.HeaderClient(httpClient, githubHeaders, *githubOwnerHeader)

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

	githubSCM := github.NewEnterpriseSCM(baseURL, httpClient)

	var ownerLister interface {
		Owners(context.Context) ([]string, error)
	} = githubSCM
	if *githubOwnersURL != "" {
		slog.Info("fetching owners from a remote source", "url", *githubOwnersURL)
		ownerLister = &urlOwners{url: *githubOwnersURL, client: &http.Client{Timeout: 30 * time.Second}}
	}

	server := newServer(*port, idb, *githubHostName)

	grp, grpCtx := errgroup.WithContext(ctx)

	allOwnersIndexer := &indexer.AllOwnersIndexer{
		DB:              idb,
		Lister:          ownerLister,
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

// headerFlag collects repeated name:value flags.
type headerFlag map[string]string

func (h headerFlag) String() string { return fmt.Sprint(map[string]string(h)) }

func (h *headerFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, ":")
	if !ok {
		return fmt.Errorf("header %q is not name:value", v)
	}
	if *h == nil {
		*h = headerFlag{}
	}
	(*h)[strings.TrimSpace(name)] = strings.TrimSpace(value)
	return nil
}

// urlOwners lists owners from a remote source serving a JSON array of logins.
type urlOwners struct {
	url    string
	client *http.Client
}

func (u *urlOwners) Owners(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("owners source %s: %s", u.url, resp.Status)
	}
	var owners []string
	if err := json.NewDecoder(resp.Body).Decode(&owners); err != nil {
		return nil, fmt.Errorf("owners source %s: %v", u.url, err)
	}
	return owners, nil
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
