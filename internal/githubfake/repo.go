// Package githubfake is a test double for the GitHub Enterprise surfaces the
// indexer reads: the GraphQL API, raw file content, and the git-trees REST API.
// Repos are plain [Repo] values, and [NewServer] serves them over HTTP so a real
// *github.GithubSCM can be pointed at it. The integration tests build their repos
// from txtar fixtures; see internal/testcases/README.md.
package githubfake

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// Repo is a fake repository. Every ref (the HEAD oid or any tag) sees the same
// Files, except where FilesAtRef says otherwise.
type Repo struct {
	// Name is the "org/name" path, e.g. "someorg/thing".
	Name string
	// HeadOID is the default branch's HEAD commit oid; "" means no commits.
	HeadOID  string
	HeadDate time.Time
	Tags     []Tag
	// Files maps a repo-relative path ("go.mod", "tracing/go.mod") to its content.
	Files map[string][]byte
	// Renamed marks the repo as one that has been renamed since it was indexed.
	// GraphQL still answers for its old name, so only its git tree changes; see
	// [Server].
	Renamed bool
	// FilesAtRef overlays Files at one ref, keyed by ref and then by path: it can
	// replace a file's content there or add one that no other ref has. Use it for
	// content that differs between refs — a root go.mod whose module path gained a
	// /vN suffix after the tags that predate the bump, say. SetFileAtTail fills it.
	FilesAtRef map[string]map[string][]byte
}

// Tag is a git tag and its creation date.
type Tag struct {
	Name string
	Date time.Time
}

// fileFromTail resolves the URL remainder "{ref}/{path}" to a repo file. A ref
// may contain slashes (e.g. the tag "tracing/v2.0.0"), so rather than guess the
// boundary it strips each known ref (the HEAD oid and tag names, longest first)
// and looks the remainder up as an exact path.
func (r *Repo) fileFromTail(tail string) ([]byte, bool) {
	for _, ref := range r.refs() {
		path, ok := strings.CutPrefix(tail, ref+"/")
		if !ok {
			continue
		}
		if data, ok := r.FilesAtRef[ref][path]; ok {
			return data, true
		}
		if data, ok := r.Files[path]; ok {
			return data, true
		}
	}
	return nil, false
}

// SetFileAtTail files data as the content at a single ref, splitting the
// "{ref}/{path}" tail the way [Repo.fileFromTail] resolves it so that the two
// agree on where a slashed ref ends. It reports false if no known ref matches.
func (r *Repo) SetFileAtTail(tail string, data []byte) bool {
	for _, ref := range r.refs() {
		path, ok := strings.CutPrefix(tail, ref+"/")
		if !ok {
			continue
		}
		if r.FilesAtRef == nil {
			r.FilesAtRef = map[string]map[string][]byte{}
		}
		if r.FilesAtRef[ref] == nil {
			r.FilesAtRef[ref] = map[string][]byte{}
		}
		r.FilesAtRef[ref][path] = data
		return true
	}
	return false
}

// pathsAt lists the repo's tree at a ref, in path order.
func (r *Repo) pathsAt(ref string) []string {
	paths := slices.Collect(maps.Keys(r.Files))
	for path := range r.FilesAtRef[ref] {
		if _, ok := r.Files[path]; !ok {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths
}

// refs lists the repo's servable refs (HEAD oid and tag names), longest first so
// a ref that is a string prefix of another can't shadow it.
func (r *Repo) refs() []string {
	refs := make([]string, 0, len(r.Tags)+1)
	if r.HeadOID != "" {
		refs = append(refs, r.HeadOID)
	}
	for _, t := range r.Tags {
		refs = append(refs, t.Name)
	}
	slices.SortFunc(refs, func(a, b string) int { return len(b) - len(a) })
	return refs
}
