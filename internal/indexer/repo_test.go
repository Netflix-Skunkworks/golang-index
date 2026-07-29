package indexer

import (
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/mod"
	"github.com/google/go-cmp/cmp"
)

const testModuleHost = "github.somecompany.net"

type tagSpec struct {
	tag          string
	goModContent string
	date         time.Time
}

func fakeFromTags(t *testing.T, specs []tagSpec) *fakeSCM {
	t.Helper()

	f := &fakeSCM{goMods: map[goModKey]string{}}
	for _, s := range specs {
		f.tags = append(f.tags, github.Tag{Name: s.tag, Date: s.date})
		if s.goModContent == "" {
			continue
		}
		// A subdir tag's go.mod is read at (tag, subdir), matching how
		// moduleVersionsForRepo calls GoMod.
		subdir, _, ok := mod.ModuleVersionFromTag(s.tag)
		if !ok {
			t.Fatalf("tag %q sets goModContent but isn't a module version tag, so nothing reads it", s.tag)
		}
		f.goMods[goModKey{ref: s.tag, subdir: subdir}] = s.goModContent
	}
	return f
}

func versionsForRepo(t *testing.T, scm scm) []*mod.ModuleVersion {
	t.Helper()

	got, err := moduleVersionsForRepo(t.Context(), scm, testModuleHost, "someorg/repo1")
	if err != nil {
		t.Fatalf("moduleVersionsForRepo(%q) returned error: %v", "someorg/repo1", err)
	}
	return got
}

func TestModuleVersionsForRepo_Empty(t *testing.T) {
	got := versionsForRepo(t, &fakeSCM{})
	if len(got) != 0 {
		t.Errorf("moduleVersionsForRepo() = %d versions, want 0", len(got))
	}
}

func TestModuleVersionsForRepo_Tags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	scm := fakeFromTags(t, []tagSpec{
		{tag: "v1.0.0", date: date, goModContent: "module stash.someorg.company.com/someorg/repo1\n"},
		{tag: "v1.1.0", date: date},
		{tag: "v1.2.0", date: date},
		{tag: "v1.3.0", date: date},
		// A bad module path, then a go.mod with no module line: skip both rather
		// than fall back to the repo URL.
		{tag: "v1.4.0", date: date, goModContent: "module invalid/module/path"},
		{tag: "v1.5.0", date: date, goModContent: "go 1.22\n"},
		{tag: "v0.9.0", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v1.0.0", Created: date, ModulePath: "stash.someorg.company.com/someorg/repo1"},
		{Version: "v1.1.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v1.2.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v1.3.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v0.9.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_SubdirectoryAndNonModuleTags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	scm := fakeFromTags(t, []tagSpec{
		{tag: "v1.0.0", date: date},
		{tag: "tracing/v0.2.2", date: date},
		{tag: "cmd/tool/v0.1.0", date: date},
		// Valid semver but not canonical (vN, vN.N pointers, build metadata): skipped.
		{tag: "v1", date: date},
		{tag: "v2", date: date},
		{tag: "v1.2", date: date},
		{tag: "tracing/v2", date: date},
		{tag: "v1.0.0+incompatible", date: date},
		// Not semver at all: skipped.
		{tag: "_gheMigrationPR-435", date: date},
		{tag: "docs/latest", date: date},
		{tag: "slides/2", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v1.0.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v0.2.2", Created: date, ModulePath: testModuleHost + "/someorg/repo1/tracing"},
		{Version: "v0.1.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1/cmd/tool"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_SubdirectoryModuleGoMod(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	scm := fakeFromTags(t, []tagSpec{
		// Subdir module at v2+: keep the /v2 from its go.mod.
		{tag: "tracing/v2.0.0", date: date, goModContent: "module go.example.com/monorepo/tracing/v2\n"},
		// go.mod declares a vanity/moved path: use it as-is.
		{tag: "auth/v1.4.0", date: date, goModContent: "module vanity.example.com/auth\n"},
		// No go.mod: fall back to the path from the repo URL.
		{tag: "metrics/v0.5.0", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v2.0.0", Created: date, ModulePath: "go.example.com/monorepo/tracing/v2"},
		{Version: "v1.4.0", Created: date, ModulePath: "vanity.example.com/auth"},
		{Version: "v0.5.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1/metrics"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_SkipsMajorVersionMismatch(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	scm := fakeFromTags(t, []tagSpec{
		// Root v2+ with no go.mod: path has no /v2 suffix, so skip it.
		{tag: "v2.0.0", date: date},
		// Root v2+ but the go.mod path is v0/v1, so skip it.
		{tag: "v2.1.0", date: date, goModContent: "module vanity.example.com/thing\n"},
		// Subdir v3 tag but the go.mod is /v2, so skip it.
		{tag: "tracing/v3.0.0", date: date, goModContent: "module go.example.com/monorepo/tracing/v2\n"},
		// A plain v1 tag still comes through.
		{tag: "v1.5.0", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v1.5.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_NoPseudoVersionWhenHeadHasNoGoMod(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// HEAD exists but the repo has no modules, so there's nothing to synthesize.
	scm := &fakeSCM{headOID: "abcdef0123456789abcdef0123456789abcdef01", headAt: commitDate}

	if got := versionsForRepo(t, scm); len(got) != 0 {
		t.Errorf("moduleVersionsForRepo() = %d versions, want 0", len(got))
	}
}

func TestModuleVersionsForRepo_SynthesizesPseudoVersions(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"

	// A repo with no semver tags: a root module plus two submodules. Every module
	// should get a HEAD pseudo-version, not just the root.
	scm := &fakeSCM{
		headOID:    oid,
		headAt:     commitDate,
		moduleDirs: []string{"", "tracing", "cmd/tool"},
		goMods: map[goModKey]string{
			{ref: oid, subdir: ""}:         "module go.example.com/thing\n",
			{ref: oid, subdir: "tracing"}:  "module go.example.com/thing/tracing/v2\n",
			{ref: oid, subdir: "cmd/tool"}: "module go.example.com/thing/cmd/tool\n",
		},
	}

	want := []*mod.ModuleVersion{
		{Version: "v0.0.0-20260102030405-abcdef012345", Created: commitDate, ModulePath: "go.example.com/thing"},
		{Version: "v2.0.0-20260102030405-abcdef012345", Created: commitDate, ModulePath: "go.example.com/thing/tracing/v2"},
		{Version: "v0.0.0-20260102030405-abcdef012345", Created: commitDate, ModulePath: "go.example.com/thing/cmd/tool"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}
