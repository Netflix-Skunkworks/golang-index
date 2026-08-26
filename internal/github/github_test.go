package github

import (
	"context"
	"encoding/json"
	"fmt"
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
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
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
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
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
	got, err := sut.RepoTags(t.Context(), "someorg/repo1")
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

			gotOID, gotCommitted, err := sut.HeadCommit(t.Context(), "someorg/repo1")
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
		content, found, err := sut.GoMod(t.Context(), "someorg/repo1", "v1.0.0", "")
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
		_, found, err := sut.GoMod(t.Context(), "someorg/repo1", "v9.9.9", "")
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
	got, err := sut.ModuleDirs(t.Context(), "someorg/repo1", oid)
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	sut := NewGithubSCM(nil, server.URL, &http.Client{})
	got, err := sut.ModuleDirs(t.Context(), "someorg/repo1", "deadbeef")
	if err != nil {
		t.Fatalf("ModuleDirs returned error for a 404, want none: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ModuleDirs = %v, want no dirs for a 404", got)
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

// hostRepos is a fake GitHub Enterprise host for a repo sweep: it answers the
// accounts listing over HTTP and the owner-repositories query over GraphQL,
// paging both the way GitHub does. Its owners are the owners of its repos.
type hostRepos struct {
	t *testing.T
	// repos holds each owner's repos in listing order.
	repos map[string][]hostRepo
	// failFor stands in for a host that refuses part of a sweep.
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

func (h *hostRepos) owners() []string { return slices.Sorted(maps.Keys(h.repos)) }

// The cursor is an offset applied to every owner in the batch, since one query
// carries one cursor.
func (h *hostRepos) Query(_ context.Context, query any, variables map[string]any) error {
	h.queries++

	q := query.(*ownerReposQuery)
	pageSize := int(variables["repoPageSize"].(githubv4.Int))
	ownerIDs := variables["ownerIDs"].([]githubv4.ID)

	from := 0
	if cursor, _ := variables["reposCursor"].(*githubv4.String); cursor != nil {
		offset, err := strconv.Atoi(string(*cursor))
		if err != nil {
			return fmt.Errorf("hostRepos: cursor %q: %v", *cursor, err)
		}
		from = offset
	}

	for _, id := range ownerIDs {
		owner := id.(string)
		if h.failFor[owner] {
			return fmt.Errorf("hostRepos: no query today for %s", owner)
		}
	}

	for _, id := range ownerIDs {
		owner := id.(string)
		repos := h.repos[owner]
		to := min(from+pageSize, len(repos))

		var node ownerNode
		node.Owner.ID = githubv4.ID(owner)
		for _, repo := range repos[from:to] {
			var repoNode ownerRepoNode
			repoNode.NameWithOwner = githubv4.String(repo.name)
			for _, language := range repo.languages {
				repoNode.Languages.Nodes = append(repoNode.Languages.Nodes, languageNode{githubv4.String(language)})
			}
			if repo.rootGoMod {
				repoNode.RootGoMod = &struct {
					OID githubv4.GitObjectID `graphql:"oid"`
				}{OID: githubv4.GitObjectID(repo.name)}
			}
			node.Owner.Repositories.Nodes = append(node.Owner.Repositories.Nodes, repoNode)
		}
		node.Owner.Repositories.PageInfo = queryPageInfo{
			EndCursor:   githubv4.String(strconv.Itoa(to)),
			HasNextPage: to < len(repos),
		}
		q.Nodes = append(q.Nodes, node)
	}
	return nil
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

	accounts := []account{}
	for i, login := range h.owners() {
		if perPage > 0 && len(accounts) == perPage {
			break
		}
		// Ids run from 1, so the listing ends with an empty page.
		if id := i + 1; id > since {
			accounts = append(accounts, account{ID: id, NodeID: login})
		}
	}
	if err := json.NewEncoder(w).Encode(accounts); err != nil {
		h.t.Errorf("hostRepos: encoding accounts: %v", err)
	}
}

func scmForHost(t *testing.T, host *hostRepos) *GithubSCM {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(host.handleAccounts))
	t.Cleanup(server.Close)
	scm := NewGithubSCM(host, server.URL, server.Client())
	scm.retryDelay = time.Microsecond
	return scm
}

func goRepo(name string) hostRepo {
	return hostRepo{name: name, languages: []string{"Go"}}
}

func TestGoRepos_KeepsOnlyReposListingGo(t *testing.T) {
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
	}).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"someorg/generated-go", "someorg/go-primary", "someorg/java-with-go"}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
}

func TestGoRepos_PagesAnOwnerWithMoreReposThanOnePage(t *testing.T) {
	// An owner whose repos run past a page is read again from its cursor, so a big
	// organization can't come back truncated.
	var repos []hostRepo
	var want []string
	for i := range 2*ownerRepoPageSize + 1 {
		name := fmt.Sprintf("bigorg/repo%03d", i)
		repos = append(repos, goRepo(name))
		want = append(want, name)
	}
	host := &hostRepos{t: t, repos: map[string][]hostRepo{"bigorg": repos}}

	got, err := scmForHost(t, host).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
	// The batched page, then the two owner pages the rest needs.
	if want := 3; host.queries != want {
		t.Errorf("sweep made %d queries, want %d", host.queries, want)
	}
}

func TestGoRepos_AccountsListingPagesByAccountID(t *testing.T) {
	// More accounts than one listing page holds, so the sweep has to follow the
	// since cursor to reach the last of them.
	var want []string
	repos := map[string][]hostRepo{}
	for i := range accountsPageSize + 1 {
		owner := fmt.Sprintf("org%03d", i)
		repos[owner] = []hostRepo{goRepo(owner + "/thing")}
		want = append(want, owner+"/thing")
	}

	got, err := scmForHost(t, &hostRepos{t: t, repos: repos}).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
}

func TestGoRepos_KeepsTheReposOfOwnersAQueryDidReach(t *testing.T) {
	// One owner GitHub won't answer for must not cost every other owner's repos:
	// storing the list never removes a repo, so the reachable ones still move the
	// index forward and the rest wait for the next sweep.
	var want []string
	// Enough owners to fill more than one batch, so the failure lands in a batch of
	// its own rather than taking the whole sweep with it.
	fixture := map[string][]hostRepo{}
	for i := range ownerBatchSize + 1 {
		owner := fmt.Sprintf("org%03d", i)
		fixture[owner] = []hostRepo{goRepo(owner + "/thing")}
		want = append(want, owner+"/thing")
	}
	broken := slices.Sorted(maps.Keys(fixture))[ownerBatchSize]
	want = slices.DeleteFunc(want, func(name string) bool { return name == broken+"/thing" })

	got, err := scmForHost(t, &hostRepos{t: t, repos: fixture, failFor: map[string]bool{broken: true}}).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
}

func TestGoRepos_FailsWhenEveryQueryFails(t *testing.T) {
	// An empty sweep would read as a host holding no Go at all, so a sweep that read
	// nothing has to fail instead.
	_, err := scmForHost(t, &hostRepos{
		t:       t,
		repos:   map[string][]hostRepo{"someorg": {goRepo("someorg/thing")}},
		failFor: map[string]bool{"someorg": true},
	}).GoRepos(t.Context())
	if err == nil {
		t.Error("GoRepos() = nil error, want a failure when every query failed")
	}
}

func TestGoRepos_RetriesTheAccountsListing(t *testing.T) {
	// The accounts listing pages by the last id seen, so a page lost to a slow host
	// would take every account after it and quietly shorten the sweep.
	got, err := scmForHost(t, &hostRepos{
		t:              t,
		repos:          map[string][]hostRepo{"someorg": {goRepo("someorg/thing")}},
		refuseAccounts: sweepQueryAttempts - 1,
	}).GoRepos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"someorg/thing"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
}
