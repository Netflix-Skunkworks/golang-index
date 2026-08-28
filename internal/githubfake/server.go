package githubfake

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Host is the GitHub Enterprise hostname the fake presents. Its repos are named
// for it.
const Host = "github.fake.test"

// BaseURL is the base URL to give the GithubSCM under test. It is never dialed,
// since [Server.Client]'s transport serves requests in memory, but its path routes
// to the fake's handlers and its host is [Host].
const BaseURL = "https://" + Host

// Server is an in-memory fake GitHub Enterprise. Build a real *github.GithubSCM
// with [BaseURL] and [Server.Client]; it serves the GraphQL, accounts-listing,
// raw-content, and git-trees surfaces the indexer uses, with no TCP socket.
type Server struct {
	handler http.Handler
	repos   map[string]*Repo
}

// NewServer builds a fake serving the given repos. The repos are served by
// reference: mutating a *Repo (e.g. its Tags) after construction changes what
// later requests observe, which tests use to simulate upstream changes between
// index cycles.
func NewServer(repos ...*Repo) *Server {
	s := &Server{repos: make(map[string]*Repo, len(repos))}
	for _, r := range repos {
		s.repos[r.Name] = r
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/graphql", s.handleGraphQL)
	mux.HandleFunc("GET /raw/{org}/{repo}/{tail...}", s.handleRaw)
	mux.HandleFunc("GET /api/v3/repos/{org}/{repo}/git/trees/{ref...}", s.handleTrees)
	mux.HandleFunc("GET /api/v3/users", s.handleAccounts)
	s.handler = mux
	return s
}

// Client returns an *http.Client that serves requests in memory by invoking the
// fake's handlers directly, with no TCP socket (mirroring shurcooL/githubv4's own
// test transport).
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: roundTripper{s.handler}}
}

type roundTripper struct{ handler http.Handler }

func (rt roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// handleGraphQL answers the three queries the indexer issues. It routes on a
// token unique to each generated query string; only the selected fields are
// returned, since the client rejects any field it did not ask for.
//
// Paging is not modelled: every response is a single page (hasNextPage false),
// so fixtures must stay under GitHub's 100-per-page limit.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if strings.Contains(body.Query, "repositoryOwner(login:") {
		s.respondOwnerRepos(w, body.Variables)
		return
	}

	// The remaining queries are both about one repo, named by the variables.
	name, err := orgRepo(body.Variables)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case strings.Contains(body.Query, "defaultBranchRef"):
		s.respondHead(w, name)
	case strings.Contains(body.Query, "refs(refPrefix"):
		s.respondTags(w, name)
	default:
		http.Error(w, "githubfake: unrecognized graphql query", http.StatusBadRequest)
	}
}

// Owners are derived from repo keys because fixtures have no owner directive.
func (s *Server) owners() []string {
	owners := map[string]bool{}
	for name := range s.repos {
		owner, _, _ := strings.Cut(name, "/")
		owners[owner] = true
	}
	return slices.Sorted(maps.Keys(owners))
}

// handleAccounts pages by the id of the last account handed over, the way GitHub's
// listing does. Owners are numbered from 1, so the listing ends with an empty page.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	// An absent parameter reads as its zero value, which is what GitHub does with
	// both of these.
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))

	accounts := []any{}
	for i, owner := range s.owners() {
		if perPage > 0 && len(accounts) == perPage {
			break
		}
		if id := i + 1; id > since {
			accounts = append(accounts, map[string]any{"id": id, "login": owner, "type": "Organization"})
		}
	}
	writeJSON(w, accounts)
}

// Every repo lists Go, since which repos a sweep picks out is not what the fixtures
// exercise. Paging is not modelled, so an owner's repos must fit one page.
func (s *Server) respondOwnerRepos(w http.ResponseWriter, vars map[string]json.RawMessage) {
	var ownerLogin string
	if err := json.Unmarshal(vars["ownerLogin"], &ownerLogin); err != nil {
		http.Error(w, fmt.Sprintf("githubfake: decoding ownerLogin variable: %v", err), http.StatusBadRequest)
		return
	}

	repos := []any{}
	for _, name := range slices.Sorted(maps.Keys(s.repos)) {
		if owner, _, _ := strings.Cut(name, "/"); owner != ownerLogin {
			continue
		}
		repos = append(repos, map[string]any{
			"nameWithOwner": name,
			"languages":     map[string]any{"nodes": []any{map[string]any{"name": "Go"}}},
		})
	}
	writeData(w, map[string]any{"repositoryOwner": map[string]any{
		"repositories": map[string]any{"nodes": repos, "pageInfo": onePage()},
	}})
}

func (s *Server) respondHead(w http.ResponseWriter, name string) {
	repo, ok := s.repos[name]
	if !ok {
		writeData(w, map[string]any{"repository": nil})
		return
	}
	if repo.HeadOID == "" {
		writeData(w, map[string]any{"repository": map[string]any{"defaultBranchRef": nil}})
		return
	}
	writeData(w, map[string]any{"repository": map[string]any{
		"defaultBranchRef": map[string]any{"target": map[string]any{
			"oid":           repo.HeadOID,
			"committedDate": repo.HeadDate.Format(time.RFC3339),
		}},
	}})
}

func (s *Server) respondTags(w http.ResponseWriter, name string) {
	repo, ok := s.repos[name]
	if !ok {
		writeData(w, map[string]any{"repository": nil})
		return
	}
	var edges []any
	for _, tag := range repo.Tags {
		edges = append(edges, map[string]any{"node": map[string]any{
			"name":   tag.Name,
			"target": map[string]any{"committedDate": tag.Date.Format(time.RFC3339)},
		}})
	}
	writeData(w, map[string]any{"repository": map[string]any{
		"refs": map[string]any{"edges": edges, "pageInfo": onePage()},
	}})
}

// handleRaw serves a repo file. The URL is /raw/{org}/{repo}/{ref}/{path}; since
// the tree is ref-independent, the file is identified by matching a known path
// against the {ref}/{path} tail (see [Repo.fileFromTail]), and the ref is ignored.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, ok := repo.fileFromTail(r.PathValue("tail"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := w.Write(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleTrees serves the recursive git tree at the ref in the URL. Every file is
// reported as a blob, which is all ModuleDirs inspects.
func (s *Server) handleTrees(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	var tree []any
	for _, filePath := range repo.pathsAt(r.PathValue("ref")) {
		tree = append(tree, map[string]any{"path": filePath, "type": "blob"})
	}
	writeJSON(w, map[string]any{"tree": tree, "truncated": false})
}

// repo looks up the repo named by a request's {org} and {repo} path wildcards.
func (s *Server) repo(r *http.Request) (*Repo, bool) {
	repo, ok := s.repos[r.PathValue("org")+"/"+r.PathValue("repo")]
	return repo, ok
}

// orgRepo reads "org/name" from a query's repoOrg/repoName variables.
func orgRepo(vars map[string]json.RawMessage) (string, error) {
	var org, name string
	if err := json.Unmarshal(vars["repoOrg"], &org); err != nil {
		return "", fmt.Errorf("decoding repoOrg variable: %v", err)
	}
	if err := json.Unmarshal(vars["repoName"], &name); err != nil {
		return "", fmt.Errorf("decoding repoName variable: %v", err)
	}
	return org + "/" + name, nil
}

// onePage is a pageInfo for a single, final page of results.
func onePage() map[string]any {
	return map[string]any{"endCursor": "", "hasNextPage": false}
}

func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, map[string]any{"data": data})
}

// writeJSON marshals v and writes it as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, fmt.Sprintf("githubfake: marshaling response: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		http.Error(w, fmt.Sprintf("githubfake: writing response: %v", err), http.StatusInternalServerError)
	}
}
