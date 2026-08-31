package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/shurcooL/githubv4"
)

type mockGithubClient struct {
	// index pointer for the stubResults slice
	resultsIdx int

	// stubbed results for queries
	stubbedResults []any
}

func (m *mockGithubClient) Query(ctx context.Context, query any, variables map[string]any) error {
	if len(m.stubbedResults) == 0 {
		return nil
	}
	if m.resultsIdx >= len(m.stubbedResults) {
		return fmt.Errorf("mockGithubClient: query #%d has no stubbed result (%d stubbed)", m.resultsIdx+1, len(m.stubbedResults))
	}

	// GitHub GraphQL client works by populating fields of the struct q with
	// the query response. Here, we mock that behavior by instead using the
	// stub results stored in the stubbedResults slice. We use a slice so that
	// we could stub multiple request/response cycles for testing paging
	// behavior. resultsIdx keeps track of which step in the multistep query
	// we're in.

	stubQueryResponse := reflect.ValueOf(query)
	stubQueryResponse = stubQueryResponse.Elem()
	stubQueryResponse.Set(reflect.ValueOf(m.stubbedResults[m.resultsIdx]))
	m.resultsIdx++
	return nil
}

func TestRepoTags_EmptyResponse(t *testing.T) {
	sut := NewGithubSCM(&mockGithubClient{}, "", nil)
	got, _, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no tags, but got %d results", len(got))
	}
}

func TestRepoTags_MultiplePages(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	responses := []struct {
		tags        []tagResponse
		endCursor   githubv4.String
		hasNextPage bool
	}{
		{
			tags: []tagResponse{
				{tag: "v1.0.0", committedDate: date},
				{tag: "v1.1.0", committedDate: date},
			},
			endCursor:   "somecursor",
			hasNextPage: true,
		},
		{
			tags: []tagResponse{
				{tag: "v1.2.0", committedDate: date},
			},
		},
	}

	var stubbedResponses []any
	for _, response := range responses {
		stubbedResponses = append(stubbedResponses, buildTagQueryResponses(t, response.tags, response.endCursor, response.hasNextPage))
	}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbedResponses}, "", nil)
	got, _, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}

	want := []Tag{
		{Name: "v1.0.0", Date: date},
		{Name: "v1.1.0", Date: date},
		{Name: "v1.2.0", Date: date},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoTags mismatch (-want +got):\n%s", diff)
	}
}

func TestRepoTags_CommitAndTaggerDates(t *testing.T) {
	committed := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tagged := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)

	stubbed := []any{buildTagQueryResponses(t, []tagResponse{
		{tag: "v1.0.0", committedDate: committed},
		{tag: "v1.1.0", taggerDate: tagged},
	}, "", false)}

	sut := NewGithubSCM(&mockGithubClient{stubbedResults: stubbed}, "", nil)
	got, _, err := sut.RepoTags(t.Context(), "someorg/repo1")
	if err != nil {
		t.Fatal(err)
	}

	want := []Tag{
		{Name: "v1.0.0", Date: committed},
		{Name: "v1.1.0", Date: tagged},
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoTags mismatch (-want +got):\n%s", diff)
	}
}

func TestHeadCommit(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"

	tests := []struct {
		name          string
		response      headQueryResponse
		wantOID       string
		wantCommitted time.Time
	}{
		{
			name:          "has commit",
			response:      buildHeadQueryResponse(oid, commitDate),
			wantOID:       oid,
			wantCommitted: commitDate,
		},
		{
			name:     "empty repo has no commit",
			response: buildHeadQueryResponse("", time.Time{}),
			wantOID:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sut := NewGithubSCM(&mockGithubClient{stubbedResults: []any{tc.response}}, "", nil)

			gotOID, gotCommitted, _, err := sut.HeadCommit(t.Context(), "someorg/repo1")
			if err != nil {
				t.Fatal(err)
			}
			if gotOID != tc.wantOID {
				t.Errorf("HeadCommit(%q) oid = %q, want %q", "someorg/repo1", gotOID, tc.wantOID)
			}
			if !gotCommitted.Equal(tc.wantCommitted) {
				t.Errorf("HeadCommit(%q) committedAt = %v, want %v", "someorg/repo1", gotCommitted, tc.wantCommitted)
			}
		})
	}
}

func TestGoMod(t *testing.T) {
	const authToken = "test-token"
	const goMod = "module go.example.com/thing\n"

	server := createTestGoModServer(t, authToken, []tagResponse{{tag: "v1.0.0", goModContent: goMod}})
	sut := NewGithubSCM(nil, server.URL, TokenClient(authToken))

	t.Run("found", func(t *testing.T) {
		content, found, _, err := sut.GoMod(t.Context(), "someorg/repo1", "v1.0.0", "")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("expected go.mod to be found")
		}
		if got := string(content); got != goMod {
			t.Errorf("GoMod content = %q, want %q", got, goMod)
		}
	})

	t.Run("missing is not an error", func(t *testing.T) {
		_, found, _, err := sut.GoMod(t.Context(), "someorg/repo1", "v9.9.9", "")
		if err != nil {
			t.Fatal(err)
		}
		if found {
			t.Error("expected missing go.mod to report found=false")
		}
	})
}

func TestModuleDirs(t *testing.T) {
	const oid = "abcdef0123456789abcdef0123456789abcdef01"
	const treeJSON = `{
		"tree": [
			{"path": "go.mod", "type": "blob"},
			{"path": "README.md", "type": "blob"},
			{"path": "tracing", "type": "tree"},
			{"path": "tracing/go.mod", "type": "blob"},
			{"path": "cmd", "type": "tree"},
			{"path": "cmd/tool", "type": "tree"},
			{"path": "cmd/tool/go.mod", "type": "blob"},
			{"path": "vendor/example.com/dep/go.mod", "type": "blob"},
			{"path": "testdata/fixture/go.mod", "type": "blob"}
		],
		"truncated": false
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := fmt.Sprintf("/api/v3/repos/someorg/repo1/git/trees/%s", oid); r.URL.Path != want {
			t.Errorf("request path = %q, want %q", r.URL.Path, want)
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("recursive"); got != "1" {
			t.Errorf("recursive query param = %q, want %q", got, "1")
		}
		if _, err := w.Write([]byte(treeJSON)); err != nil {
			t.Errorf("writing tree response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	sut := NewGithubSCM(nil, server.URL, &http.Client{})
	got, _, err := sut.ModuleDirs(t.Context(), "someorg/repo1", oid)
	if err != nil {
		t.Fatal(err)
	}

	// Root and both submodules are modules; vendor and testdata are skipped.
	want := []string{"", "tracing", "cmd/tool"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ModuleDirs mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleDirs_NotFound(t *testing.T) {
	got, _, err := respondingSCM(t, http.StatusNotFound, nil, "").ModuleDirs(t.Context(), "someorg/repo1", "deadbeef")
	if err != nil {
		t.Fatalf("ModuleDirs returned error for a 404, want none: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ModuleDirs = %v, want no dirs for a 404", got)
	}
}

// respondingSCM points a GithubSCM at a server that answers every request with
// one status, set of headers, and body.
func respondingSCM(t *testing.T, status int, headers map[string]string, body string) *GithubSCM {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			// Can't call t.Fatal off the test goroutine, so use Errorf.
			t.Errorf("writing response body: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return NewGithubSCM(nil, server.URL, &http.Client{})
}

// githubRequestID stands in for the X-GitHub-Request-Id GitHub serves every
// response with.
const githubRequestID = "0123:4567:89ABCD:EF0123:456789AB"

// unexpectedStatuses are responses neither REST call can use, and whether a later
// pass might be answered differently.
var unexpectedStatuses = []struct {
	name          string
	status        int
	headers       map[string]string
	body          string
	wantRetryable bool
}{
	{
		name:    "a redirect, so the repo moved",
		status:  http.StatusMovedPermanently,
		headers: map[string]string{"Location": "/api/v3/repos/someorg/renamed/git/trees/deadbeef"},
	},
	{
		// What a caching proxy in front of the host answers for a renamed repo. The
		// Retry-After reads as a rate limit, but the request never reached GitHub.
		name:    "a 403 from in front of GitHub",
		status:  http.StatusForbidden,
		headers: map[string]string{"Server": "Varnish", "Retry-After": "5"},
	},
	{
		name:    "GitHub refusing the repo",
		status:  http.StatusForbidden,
		headers: map[string]string{"X-GitHub-Request-Id": githubRequestID},
	},
	{
		name:          "GitHub's primary rate limit",
		status:        http.StatusForbidden,
		headers:       map[string]string{"X-GitHub-Request-Id": githubRequestID, "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1758000000"},
		wantRetryable: true,
	},
	{
		name:          "a secondary rate limit",
		status:        http.StatusForbidden,
		headers:       map[string]string{"X-GitHub-Request-Id": githubRequestID, "Gh-Limited-By": "search-elapsed-time", "Retry-After": "60"},
		wantRetryable: true,
	},
	{
		name:          "a secondary rate limit named only in the message",
		status:        http.StatusForbidden,
		headers:       map[string]string{"X-GitHub-Request-Id": githubRequestID},
		body:          `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
		wantRetryable: true,
	},
	{
		name:          "the host failing",
		status:        http.StatusBadGateway,
		wantRetryable: true,
	},
	{
		// Credentials are rotated outside this process, so a later pass may carry
		// working ones. Reading this as permanent would index every repo as empty.
		name:          "rejected credentials",
		status:        http.StatusUnauthorized,
		headers:       map[string]string{"X-GitHub-Request-Id": githubRequestID},
		body:          `{"message":"Bad credentials"}`,
		wantRetryable: true,
	},
	{
		name:          "the host throttling",
		status:        http.StatusTooManyRequests,
		wantRetryable: true,
	},
}

// restCalls are the calls that have to answer for a response they cannot use,
// reduced to the answer they give.
var restCalls = []struct {
	name string
	ask  func(context.Context, *GithubSCM) (retryable bool, err error)
}{
	{"ModuleDirs", func(ctx context.Context, scm *GithubSCM) (bool, error) {
		_, retryable, err := scm.ModuleDirs(ctx, "someorg/repo1", "deadbeef")
		return retryable, err
	}},
	{"GoMod", func(ctx context.Context, scm *GithubSCM) (bool, error) {
		_, _, retryable, err := scm.GoMod(ctx, "someorg/repo1", "v1.0.0", "")
		return retryable, err
	}},
}

func TestUnexpectedStatus(t *testing.T) {
	for _, call := range restCalls {
		for _, tc := range unexpectedStatuses {
			t.Run(call.name+"/"+tc.name, func(t *testing.T) {
				retryable, err := call.ask(t.Context(), respondingSCM(t, tc.status, tc.headers, tc.body))
				if err == nil {
					t.Fatalf("%s returned no error for a %d, want one", call.name, tc.status)
				}
				if retryable != tc.wantRetryable {
					t.Errorf("%s reported retryable=%v, want %v: %v", call.name, retryable, tc.wantRetryable, err)
				}
			})
		}
	}
}

// A status on its own doesn't say who answered, so the line has to carry the
// evidence the verdict was reached from.
func TestUnexpectedStatus_LogsTheResponse(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	sut := respondingSCM(t, http.StatusForbidden, map[string]string{"Server": "Varnish", "Retry-After": "5"}, "Forbidden")
	if _, _, err := sut.ModuleDirs(t.Context(), "someorg/repo1", "deadbeef"); err == nil {
		t.Fatal("ModuleDirs returned no error for a 403, want one")
	}

	got := logged.String()
	for _, want := range []string{
		"/api/v3/repos/someorg/repo1/git/trees/deadbeef?recursive=1",
		`status="403 Forbidden"`,
		"server=Varnish",
		"retryAfter=5",
		`githubRequestID=""`,
		"retryable=false",
		"it never reached GitHub",
		"body=Forbidden",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("logged %q, want it to mention %q", got, want)
		}
	}
}

type tagResponse struct {
	tag           string
	goModContent  string
	committedDate time.Time
	taggerDate    time.Time
}

func buildTagQueryResponses(t *testing.T, tags []tagResponse, endCursor githubv4.String, hasNextPage bool) tagQueryResponse {
	t.Helper()

	var edges []tagQueryEdge

	for _, tag := range tags {
		var edge tagQueryEdge
		edge.Node.Name = githubv4.String(tag.tag)
		if !tag.committedDate.IsZero() {
			edge.Node.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.committedDate})
		}
		if !tag.taggerDate.IsZero() {
			edge.Node.Target.Tag.Tagger.Date = *githubv4.NewDateTime(githubv4.DateTime{Time: tag.taggerDate})
		}
		edges = append(edges, edge)
	}

	var q tagQueryResponse
	q.Repository.Refs.Edges = edges
	q.Repository.Refs.PageInfo.EndCursor = endCursor
	q.Repository.Refs.PageInfo.HasNextPage = hasNextPage

	return q
}

// buildHeadQueryResponse stubs the default branch's HEAD commit. A zero-value
// oid stands in for an empty repo with no default branch.
func buildHeadQueryResponse(oid string, committedDate time.Time) headQueryResponse {
	var q headQueryResponse
	q.Repository.DefaultBranchRef.Target.Commit.OID = githubv4.GitObjectID(oid)
	if !committedDate.IsZero() {
		q.Repository.DefaultBranchRef.Target.Commit.CommittedDate = *githubv4.NewDateTime(githubv4.DateTime{Time: committedDate})
	}
	return q
}

func createTestGoModServer(t *testing.T, authToken string, tags []tagResponse) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("token %s", authToken) {
			http.Error(w, "wrong Authorization header", http.StatusUnauthorized)
			return
		}

		for _, tag := range tags {
			if tag.goModContent == "" {
				continue
			}
			if strings.HasSuffix(r.URL.Path, "/"+goModRequestPath(tag.tag)) {
				if _, err := w.Write([]byte(tag.goModContent)); err != nil {
					// Can't call t.Fatal off the test goroutine, so use Errorf.
					t.Errorf("writing go.mod for %q: %v", tag.tag, err)
				}
				return
			}
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	return server
}

// goModRequestPath is the "<ref>/<path>" part of the go.mod URL for a tag: the
// root go.mod at "<tag>/go.mod", or the subdir go.mod at "<tag>/<subdir>/go.mod"
// for a "<subdir>/vX.Y.Z" tag. It matches the URL GoMod builds.
func goModRequestPath(tag string) string {
	if i := strings.LastIndex(tag, "/"); i >= 0 {
		return tag + "/" + tag[:i] + "/go.mod"
	}
	return tag + "/go.mod"
}

// hostRepos is a fake GitHub Enterprise host: it answers the accounts listing over
// HTTP and the owner-repositories query over GraphQL. Its owners are the owners of
// its repos.
type hostRepos struct {
	t *testing.T
	// repos holds each owner's repos in listing order.
	repos map[string][]hostRepo
	// bots stand in for the bot accounts the listing carries alongside owners.
	bots []string
	// repoPageSize is the page size for an owner's repos; tests set it small to
	// force paging, and scmForHost defaults it to production's 100 when unset.
	repoPageSize int
	// failFor stands in for a host that refuses to answer for an owner.
	failFor map[string]bool
	queries int
	// refuseAccounts stands in for a host too slow to answer the first tries.
	refuseAccounts int
}

type hostRepo struct {
	name      string
	languages []string
	rootGoMod bool
}

// The cursor is an integer offset into the owner's repos, not an opaque token.
func (h *hostRepos) Query(_ context.Context, query any, variables map[string]any) error {
	h.queries++

	owner := string(variables["ownerLogin"].(githubv4.String))
	if h.failFor[owner] {
		return fmt.Errorf("hostRepos: no query today for %s", owner)
	}

	from := 0
	if cursor, _ := variables["reposCursor"].(*githubv4.String); cursor != nil {
		offset, err := strconv.Atoi(string(*cursor))
		if err != nil {
			return fmt.Errorf("hostRepos: cursor %q: %v", *cursor, err)
		}
		from = offset
	}

	repos := h.repos[owner]
	to := min(from+h.repoPageSize, len(repos))
	page := &query.(*ownerReposQuery).RepositoryOwner.Repositories
	for _, repo := range repos[from:to] {
		var node ownerRepoNode
		node.NameWithOwner = githubv4.String(repo.name)
		for _, language := range repo.languages {
			node.Languages.Nodes = append(node.Languages.Nodes, languageNode{githubv4.String(language)})
		}
		if repo.rootGoMod {
			node.RootGoMod = &struct {
				OID githubv4.GitObjectID `graphql:"oid"`
			}{OID: githubv4.GitObjectID(repo.name)}
		}
		page.Nodes = append(page.Nodes, node)
	}
	page.PageInfo = queryPageInfo{EndCursor: githubv4.String(strconv.Itoa(to)), HasNextPage: to < len(repos)}
	return nil
}

// accounts numbers entries from 1, so paging with since=lastID yields an empty
// final page — the termination signal Owners relies on.
func (h *hostRepos) accounts() []account {
	var accounts []account
	for _, login := range slices.Sorted(maps.Keys(h.repos)) {
		accounts = append(accounts, account{Login: login, Type: "Organization"})
	}
	for _, login := range h.bots {
		accounts = append(accounts, account{Login: login, Type: "Bot"})
	}
	for i := range accounts {
		accounts[i].ID = i + 1
	}
	return accounts
}

// handleAccounts honours since and per_page the way GitHub's listing does.
func (h *hostRepos) handleAccounts(w http.ResponseWriter, r *http.Request) {
	if h.refuseAccounts > 0 {
		h.refuseAccounts--
		http.Error(w, "hostRepos: not this time", http.StatusInternalServerError)
		return
	}

	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))

	page := []account{}
	for _, a := range h.accounts() {
		if perPage > 0 && len(page) == perPage {
			break
		}
		if a.ID > since {
			page = append(page, a)
		}
	}
	if err := json.NewEncoder(w).Encode(page); err != nil {
		h.t.Errorf("hostRepos: encoding accounts: %v", err)
	}
}

func scmForHost(t *testing.T, host *hostRepos) *GithubSCM {
	t.Helper()

	if host.repoPageSize == 0 {
		host.repoPageSize = 100
	}
	server := httptest.NewServer(http.HandlerFunc(host.handleAccounts))
	t.Cleanup(server.Close)
	scm := NewGithubSCM(host, server.URL, server.Client())
	scm.retryDelay = time.Microsecond
	return scm
}

func goRepo(name string) hostRepo {
	return hostRepo{name: name, languages: []string{"Go"}}
}

func TestOwnerGoRepos_KeepsOnlyReposListingGo(t *testing.T) {
	// Go anywhere in a repo's languages makes it a Go repo, so the repo GitHub calls
	// Java is one. So does a root go.mod on its own, which is all that marks a repo
	// whose Go is generated at build time and so goes undetected.
	got, err := scmForHost(t, &hostRepos{
		t: t,
		repos: map[string][]hostRepo{"someorg": {
			{name: "someorg/go-primary", languages: []string{"Go", "Shell"}},
			{name: "someorg/java-with-go", languages: []string{"Shell", "Java", "Go"}},
			{name: "someorg/generated-go", languages: []string{"Shell"}, rootGoMod: true},
			{name: "someorg/no-go", languages: []string{"Java", "Shell"}},
			{name: "someorg/no-languages"},
		}},
	}).OwnerGoRepos(t.Context(), "someorg")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"someorg/go-primary", "someorg/java-with-go", "someorg/generated-go"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("OwnerGoRepos() mismatch (-want +got):\n%s", diff)
	}
}

func TestOwnerGoRepos_PagesAnOwnerWithMoreReposThanOnePage(t *testing.T) {
	// An owner whose repos run past a page is read again from its cursor, so a big
	// organization can't come back truncated.
	var repos []hostRepo
	var want []string
	for i := range 5 {
		name := fmt.Sprintf("bigorg/repo%03d", i)
		repos = append(repos, goRepo(name))
		want = append(want, name)
	}
	host := &hostRepos{t: t, repos: map[string][]hostRepo{"bigorg": repos}, repoPageSize: 2}

	got, err := scmForHost(t, host).OwnerGoRepos(t.Context(), "bigorg")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("OwnerGoRepos() mismatch (-want +got):\n%s", diff)
	}
	if want := 3; host.queries != want {
		t.Errorf("listing made %d queries, want %d", host.queries, want)
	}
}

func TestOwnerGoRepos_ReturnsTheHostsError(t *testing.T) {
	// One owner the host won't answer for is that owner's problem: its work item
	// goes uncompleted and the queue hands it out again.
	_, err := scmForHost(t, &hostRepos{
		t:       t,
		repos:   map[string][]hostRepo{"someorg": {goRepo("someorg/thing")}},
		failFor: map[string]bool{"someorg": true},
	}).OwnerGoRepos(t.Context(), "someorg")
	if err == nil {
		t.Error("OwnerGoRepos() = nil error, want the host's failure")
	}
}

func TestOwners_PagesByAccountID(t *testing.T) {
	// More accounts than one listing page holds, so the listing has to follow the
	// since cursor to reach the last of them.
	var want []string
	repos := map[string][]hostRepo{}
	for i := range accountsPageSize + 1 {
		owner := fmt.Sprintf("org%03d", i)
		repos[owner] = []hostRepo{goRepo(owner + "/thing")}
		want = append(want, owner)
	}

	got, err := scmForHost(t, &hostRepos{t: t, repos: repos}).Owners(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Owners() mismatch (-want +got):\n%s", diff)
	}
}

func TestOwners_SkipsAccountsThatCannotOwnRepos(t *testing.T) {
	// Without the filter, a bot would be stored as an owner whose work item could
	// never complete.
	got, err := scmForHost(t, &hostRepos{
		t:     t,
		repos: map[string][]hostRepo{"someorg": {goRepo("someorg/thing")}},
		bots:  []string{"dependabot[bot]"},
	}).Owners(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"someorg"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Owners() mismatch (-want +got):\n%s", diff)
	}
}

func TestOwners_RetriesTheAccountsListing(t *testing.T) {
	got, err := scmForHost(t, &hostRepos{
		t:              t,
		repos:          map[string][]hostRepo{"someorg": {goRepo("someorg/thing")}},
		refuseAccounts: accountsAttempts - 1,
	}).Owners(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"someorg"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Owners() mismatch (-want +got):\n%s", diff)
	}
}
