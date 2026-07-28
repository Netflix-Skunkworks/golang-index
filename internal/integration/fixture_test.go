package integration

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/githubfake"
	"golang.org/x/tools/txtar"
)

// testcasesDir holds the txtar fixtures, one per test case. It sits outside this
// package because it is data, not Go code.
const testcasesDir = "../testcases"

// fixture is one test case, read from a testcases/*.txtar archive: the repos the
// fake GitHub serves, the rows indexing them must produce, and optionally the
// upstream changes to apply before a second index cycle together with the rows
// expected afterwards. See ../testcases/README.md for the format.
type fixture struct {
	repos map[string]*githubfake.Repo
	want  []row
	// update holds the directive lines of the archive's cycle section, applied to
	// repos between the two index cycles.
	update []string
	// wantAfterUpdate is the want.2 file, or want again for an archive with no
	// cycle section, where nothing changes between the cycles.
	wantAfterUpdate []row
}

// row is one row of repo_module_versions in the form the want files use:
//
//	someorg/tagged go.example.com/tagged v1.0.0 2025-01-02T03:04:05Z
type row struct {
	Repo    string
	Module  string
	Version string
	Created string
}

// rowOf renders a stored module version as a row.
func rowOf(v *db.RepoModuleVersion) row {
	return row{
		Repo:    v.OrgRepoName,
		Module:  v.ModulePath,
		Version: v.Version,
		Created: formatCreated(v.Created),
	}
}

// key identifies the module version a row is about, ignoring when it was created.
func (r row) key() string { return r.Repo + " " + r.Module + "@" + r.Version }

// formatCreated renders a timestamp the one way both want files and stored rows
// are spelled, so the two are compared in the same form.
func formatCreated(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// sortRows orders rows the way both want files and query results are compared.
func sortRows(rows []row) {
	slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.key(), b.key()) })
}

// loadFixture reads and parses the archive at path.
func loadFixture(t *testing.T, path string) *fixture {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	f, err := parseFixture(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f
}

func parseFixture(archive []byte) (*fixture, error) {
	a := txtar.Parse(archive)
	f := &fixture{repos: map[string]*githubfake.Repo{}}

	setup, update, hasCycle := splitCycle(string(a.Comment))
	if err := f.applyDirectives(setup, true); err != nil {
		return nil, err
	}
	f.update = update
	if len(f.repos) == 0 {
		return nil, fmt.Errorf("archive declares no repos")
	}

	var sawWant, sawWantAfterUpdate bool
	for _, file := range a.Files {
		var err error
		switch file.Name {
		case "want":
			sawWant = true
			f.want, err = parseRows(file.Data)
		case "want.2":
			sawWantAfterUpdate = true
			f.wantAfterUpdate, err = parseRows(file.Data)
		default:
			err = f.addFile(file.Name, file.Data)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %v", file.Name, err)
		}
	}

	switch {
	case !sawWant:
		return nil, fmt.Errorf("archive has no 'want' file (write an empty one to expect no rows)")
	case hasCycle && !sawWantAfterUpdate:
		return nil, fmt.Errorf("archive has a cycle section but no 'want.2' file")
	case !hasCycle && sawWantAfterUpdate:
		return nil, fmt.Errorf("archive has a 'want.2' file but no cycle section")
	case !hasCycle:
		f.wantAfterUpdate = f.want
	}
	return f, nil
}

// splitCycle divides an archive's directive block at its "cycle" line into the
// directives that set the repos up and the directives that change them between
// the two index cycles.
func splitCycle(directives string) (setup, update []string, hasCycle bool) {
	for line := range strings.SplitSeq(directives, "\n") {
		if strings.TrimSpace(line) == "cycle" {
			hasCycle = true
			continue
		}
		if hasCycle {
			update = append(update, line)
		} else {
			setup = append(setup, line)
		}
	}
	return setup, update, hasCycle
}

// applyDirectives interprets a block of directive lines, ignoring blank lines and
// '#' comments. A "repo" line names the repo the directives after it apply to,
// declaring it when declare is set and selecting an already-declared one when not.
func (f *fixture) applyDirectives(lines []string, declare bool) error {
	var current *githubfake.Repo
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if fields[0] == "repo" {
			if len(fields) != 2 {
				return fmt.Errorf("%q: repo takes one argument", line)
			}
			repo, err := f.selectRepo(fields[1], declare)
			if err != nil {
				return err
			}
			current = repo
			continue
		}
		if current == nil {
			return fmt.Errorf("%q: no repo declared yet", line)
		}
		switch fields[0] {
		case "head":
			oid, date, err := nameAndDate(line, fields)
			if err != nil {
				return err
			}
			current.HeadOID, current.HeadDate = oid, date
		case "tag":
			name, date, err := nameAndDate(line, fields)
			if err != nil {
				return err
			}
			current.Tags = append(current.Tags, githubfake.Tag{Name: name, Date: date})
		case "untag":
			if len(fields) != 2 {
				return fmt.Errorf("%q: untag takes a tag name", line)
			}
			current.Tags = slices.DeleteFunc(current.Tags, func(tag githubfake.Tag) bool {
				return tag.Name == fields[1]
			})
		default:
			return fmt.Errorf("%q: unknown directive %q", line, fields[0])
		}
	}
	return nil
}

// nameAndDate reads the two arguments a head or tag directive takes.
func nameAndDate(line string, fields []string) (string, time.Time, error) {
	if len(fields) != 3 {
		return "", time.Time{}, fmt.Errorf("%q: %s takes a name and a date", line, fields[0])
	}
	date, err := time.Parse(time.RFC3339, fields[2])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%q: %v", line, err)
	}
	return fields[1], date, nil
}

func (f *fixture) selectRepo(name string, declare bool) (*githubfake.Repo, error) {
	repo, ok := f.repos[name]
	switch {
	case ok && declare:
		return nil, fmt.Errorf("repo %q is declared twice", name)
	case !ok && !declare:
		return nil, fmt.Errorf("repo %q was not declared before the cycle section", name)
	case !ok:
		repo = &githubfake.Repo{Name: name, Files: map[string][]byte{}}
		f.repos[name] = repo
	}
	return repo, nil
}

// addFile files an archive entry under the repo it is named for, so
// "someorg/thing/go.mod" becomes "go.mod" in the repo someorg/thing. An "@ref"
// suffix on the repo ("someorg/thing@v1.0.0/go.mod") makes it the content at that
// ref alone, which is how a fixture models a file that changed between refs.
func (f *fixture) addFile(path string, data []byte) error {
	org, rest, ok := strings.Cut(path, "/")
	repoName, rel, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 {
		return fmt.Errorf("path is not <org>/<repo>[@<ref>]/<path within the repo>")
	}
	name, ref, atRef := strings.Cut(repoName, "@")
	repo, ok := f.repos[org+"/"+name]
	if !ok {
		return fmt.Errorf("no repo %q is declared", org+"/"+name)
	}
	if !atRef {
		repo.Files[rel] = data
		return nil
	}
	if !declaresRef(repo, ref) {
		return fmt.Errorf("repo %q has no tag or head %q", repo.Name, ref)
	}
	if repo.FilesAtRef == nil {
		repo.FilesAtRef = map[string]map[string][]byte{}
	}
	if repo.FilesAtRef[ref] == nil {
		repo.FilesAtRef[ref] = map[string][]byte{}
	}
	repo.FilesAtRef[ref][rel] = data
	return nil
}

// declaresRef reports whether ref is one the repo can serve, so that a typo in an
// "@ref" path is an error rather than an override nothing reads.
func declaresRef(repo *githubfake.Repo, ref string) bool {
	return ref == repo.HeadOID || slices.ContainsFunc(repo.Tags, func(tag githubfake.Tag) bool {
		return tag.Name == ref
	})
}

// repoList returns the fixture's repos in name order.
func (f *fixture) repoList() []*githubfake.Repo {
	repos := slices.Collect(maps.Values(f.repos))
	slices.SortFunc(repos, func(a, b *githubfake.Repo) int { return strings.Compare(a.Name, b.Name) })
	return repos
}

// applyUpdate makes the fixture's upstream changes visible to the fake, which
// serves the repos by reference.
func (f *fixture) applyUpdate(t *testing.T) {
	t.Helper()
	if err := f.applyDirectives(f.update, false); err != nil {
		t.Fatalf("applying the cycle section: %v", err)
	}
}

// parseRows reads a want file: one row per line as "repo module version created",
// with blank lines and '#' comments ignored.
func parseRows(data []byte) ([]row, error) {
	var rows []row
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 4 {
			return nil, fmt.Errorf("%q: got %d fields, want 4 (repo module version created)", line, len(fields))
		}
		created, err := time.Parse(time.RFC3339, fields[3])
		if err != nil {
			return nil, fmt.Errorf("%q: %v", line, err)
		}
		rows = append(rows, row{Repo: fields[0], Module: fields[1], Version: fields[2], Created: formatCreated(created)})
	}
	sortRows(rows)
	return rows, nil
}
