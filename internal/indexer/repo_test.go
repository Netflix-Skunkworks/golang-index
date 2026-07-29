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

func TestModuleVersionsForRepo_NoVersionsWhenRepoHasNoCommits(t *testing.T) {
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
		// Root v2+ but the go.mod path is v0/v1, so skip it.
		{tag: "v2.1.0", date: date, goModContent: "module vanity.example.com/thing\n"},
		// Subdir v3 tag but the go.mod is /v2, so skip it.
		{tag: "tracing/v3.0.0", date: date, goModContent: "module go.example.com/monorepo/tracing/v2\n"},
		// A subdir v2 tag with no go.mod can't be +incompatible, which is only for
		// a module in the repo root, so skip it too.
		{tag: "tracing/v2.0.0", date: date},
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

func TestModuleVersionsForRepo_MajorVersionSubdirectory(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	// tracing/ holds the v0/v1 module and tracing/v2/ holds its v2, so the tag
	// prefix is "tracing" for both and only the version says which dir to read.
	scm := &fakeSCM{
		tags: []github.Tag{
			{Name: "tracing/v0.5.0", Date: date},
			{Name: "tracing/v2.0.0", Date: date},
			{Name: "v3.0.0", Date: date},
			{Name: "auth/v2.0.0", Date: date},
		},
		goMods: map[goModKey]string{
			{ref: "tracing/v0.5.0", subdir: "tracing"}:    "module go.example.com/monorepo/tracing\n",
			{ref: "tracing/v2.0.0", subdir: "tracing"}:    "module go.example.com/monorepo/tracing\n",
			{ref: "tracing/v2.0.0", subdir: "tracing/v2"}: "module go.example.com/monorepo/tracing/v2\n",
			// The same layout at the repo root, where the major dir is just "v3".
			{ref: "v3.0.0", subdir: ""}:   "module go.example.com/monorepo\n",
			{ref: "v3.0.0", subdir: "v3"}: "module go.example.com/monorepo/v3\n",
			// A major dir whose go.mod forgot the suffix reads as if it weren't
			// there, leaving the subdir's own go.mod to account for the version.
			{ref: "auth/v2.0.0", subdir: "auth"}:    "module go.example.com/monorepo/auth/v2\n",
			{ref: "auth/v2.0.0", subdir: "auth/v2"}: "module go.example.com/monorepo/auth\n",
		},
	}

	want := []*mod.ModuleVersion{
		{Version: "v0.5.0", Created: date, ModulePath: "go.example.com/monorepo/tracing"},
		{Version: "v2.0.0", Created: date, ModulePath: "go.example.com/monorepo/tracing/v2"},
		{Version: "v3.0.0", Created: date, ModulePath: "go.example.com/monorepo/v3"},
		{Version: "v2.0.0", Created: date, ModulePath: "go.example.com/monorepo/auth/v2"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_IncompatibleWhenRepoHasNoGoMod(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	// A pre-modules repo that kept releasing past v1.
	scm := fakeFromTags(t, []tagSpec{
		{tag: "v1.0.0", date: date},
		{tag: "v2.0.0", date: date},
		{tag: "v3.1.0-rc.1", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v1.0.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v2.0.0+incompatible", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v3.1.0-rc.1+incompatible", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_PseudoVersionAtRepoURLWhenNoGoMod(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"
	// No module dirs, so there's no go.mod to read a path from.
	scm := &fakeSCM{headOID: oid, headAt: commitDate}

	want := []*mod.ModuleVersion{{
		Version:    "v0.0.0-20260102030405-abcdef012345",
		Created:    commitDate,
		ModulePath: testModuleHost + "/someorg/repo1",
	}}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_NoPseudoVersionWhenGoModDeclaresNoModule(t *testing.T) {
	const oid = "abcdef0123456789abcdef0123456789abcdef01"
	scm := &fakeSCM{
		headOID:    oid,
		headAt:     time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		moduleDirs: []string{""},
		goMods:     map[goModKey]string{{ref: oid, subdir: ""}: "go 1.22\n"},
	}

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
