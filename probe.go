package main

// THROWAWAY. Asks what this process's GitHub identity can enumerate, so we can
// choose an owner-discovery strategy for a host that has many organizations.
// Delete once the question is answered.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Netflix-Skunkworks/golang-index/internal/github"
)

func runEnumerationProbe(ctx context.Context, baseURL string, client *http.Client, enterpriseSlug, owner string) {
	if owner != "" {
		ctx = github.ContextWithOwner(ctx, owner)
		slog.Info("probe: owner context set", "owner", owner)
	}
	slog.Info("probe: starting", "baseURL", baseURL, "enterpriseSlug", enterpriseSlug)

	probeAccounts(ctx, baseURL, client)
	probeOrganizations(ctx, baseURL, client)
	probeAppInstallations(ctx, baseURL, client)
	probeInstallationRepos(ctx, baseURL, client)
	probeViewer(ctx, baseURL, client)
	if enterpriseSlug != "" {
		probeEnterpriseOrgs(ctx, baseURL, client, enterpriseSlug)
	}

	slog.Info("probe: done")
}

func probeAccounts(ctx context.Context, baseURL string, client *http.Client) {
	body, status, err := probeGET(ctx, client, baseURL+"/api/v3/users?since=0&per_page=100")
	if err != nil {
		slog.Error("probe: /users failed", "error", err)
		return
	}
	var accounts []struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(body, &accounts); err != nil {
		slog.Warn("probe: /users undecodable", "status", status, "body", snippet(body))
		return
	}
	var orgs []string
	for _, a := range accounts {
		if a.Type == "Organization" {
			orgs = append(orgs, a.Login)
		}
	}
	slog.Info("probe: /users", "status", status, "accountsOnFirstPage", len(accounts), "organizations", orgs)
}

func probeOrganizations(ctx context.Context, baseURL string, client *http.Client) {
	body, status, err := probeGET(ctx, client, baseURL+"/api/v3/organizations?per_page=100")
	if err != nil {
		slog.Error("probe: /organizations failed", "error", err)
		return
	}
	var orgs []struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(body, &orgs); err != nil {
		slog.Warn("probe: /organizations undecodable", "status", status, "body", snippet(body))
		return
	}
	logins := make([]string, 0, len(orgs))
	for _, o := range orgs {
		logins = append(logins, o.Login)
	}
	slog.Info("probe: /organizations", "status", status, "count", len(logins), "organizations", logins)
}

func probeAppInstallations(ctx context.Context, baseURL string, client *http.Client) {
	body, status, err := probeGET(ctx, client, baseURL+"/api/v3/app/installations?per_page=100")
	if err != nil {
		slog.Error("probe: /app/installations failed", "error", err)
		return
	}
	var installations []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &installations); err != nil {
		slog.Warn("probe: /app/installations undecodable", "status", status, "body", snippet(body))
		return
	}
	accounts := make([]string, 0, len(installations))
	for _, i := range installations {
		accounts = append(accounts, fmt.Sprintf("%s(%d)", i.Account.Login, i.ID))
	}
	slog.Info("probe: /app/installations", "status", status, "count", len(accounts), "installations", accounts)
}

// probeInstallationRepos asks what the current installation token can reach. If
// this works but no listing does, owner discovery has to come from outside
// GitHub.
func probeInstallationRepos(ctx context.Context, baseURL string, client *http.Client) {
	body, status, err := probeGET(ctx, client, baseURL+"/api/v3/installation/repositories?per_page=100")
	if err != nil {
		slog.Error("probe: /installation/repositories failed", "error", err)
		return
	}
	var page struct {
		TotalCount   int `json:"total_count"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		slog.Warn("probe: /installation/repositories undecodable", "status", status, "body", snippet(body))
		return
	}
	owners := map[string]bool{}
	for _, r := range page.Repositories {
		owners[strings.SplitN(r.FullName, "/", 2)[0]] = true
	}
	slog.Info("probe: /installation/repositories", "status", status, "totalCount", page.TotalCount, "ownersOnFirstPage", len(owners))
}

func probeViewer(ctx context.Context, baseURL string, client *http.Client) {
	body, status, err := probeGraphQL(ctx, client, baseURL, `{viewer{login __typename organizations(first:100){totalCount nodes{login}}}}`)
	if err != nil {
		slog.Error("probe: viewer failed", "error", err)
		return
	}
	slog.Info("probe: viewer", "status", status, "body", snippet(body))
}

func probeEnterpriseOrgs(ctx context.Context, baseURL string, client *http.Client, slug string) {
	query := fmt.Sprintf(`{enterprise(slug:%q){name organizations(first:100){totalCount nodes{login}}}}`, slug)
	body, status, err := probeGraphQL(ctx, client, baseURL, query)
	if err != nil {
		slog.Error("probe: enterprise organizations failed", "error", err)
		return
	}
	slog.Info("probe: enterprise organizations", "status", status, "slug", slug, "body", snippet(body))
}

func probeGET(ctx context.Context, client *http.Client, url string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	return probeDo(client, request)
}

func probeGraphQL(ctx context.Context, client *http.Client, baseURL, query string) ([]byte, int, error) {
	payload, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/graphql", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	return probeDo(client, request)
}

func probeDo(client *http.Client, request *http.Request) ([]byte, int, error) {
	resp, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		slog.Warn("probe: non-200", "url", request.URL.Path, "status", resp.StatusCode, "body", snippet(body))
	}
	return body, resp.StatusCode, nil
}

func snippet(body []byte) string {
	const max = 400
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
