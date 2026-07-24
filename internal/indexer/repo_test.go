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

func fakeFromTags(specs []tagSpec) *fakeSCM {
	f := &fakeSCM{goMods: map[string]string{}}
	for _, s := range specs {
		f.tags = append(f.tags, github.Tag{Name: s.tag, Date: s.date})
		if s.goModContent != "" {
			f.goMods[s.tag] = s.goModContent
		}
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
	scm := fakeFromTags([]tagSpec{
		{tag: "v1.0.0", date: date, goModContent: "module stash.someorg.company.com/someorg/repo1\n"},
		{tag: "v1.1.0", date: date},
		{tag: "v1.2.0", date: date},
		{tag: "v1.3.0", date: date},
		{tag: "v1.4.0", date: date, goModContent: "module invalid/module/path"},
		{tag: "v0.9.0", date: date},
	})

	want := []*mod.ModuleVersion{
		{Version: "v1.0.0", Created: date, ModulePath: "stash.someorg.company.com/someorg/repo1"},
		{Version: "v1.1.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v1.2.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		{Version: "v1.3.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
		// v1.4.0's go.mod has a bad module path, so it's skipped.
		{Version: "v0.9.0", Created: date, ModulePath: testModuleHost + "/someorg/repo1"},
	}

	if diff := cmp.Diff(want, versionsForRepo(t, scm)); diff != "" {
		t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
	}
}

func TestModuleVersionsForRepo_SubdirectoryAndNonModuleTags(t *testing.T) {
	date := time.Date(2025, 1, 2, 3, 4, 5, 6, time.UTC)
	scm := fakeFromTags([]tagSpec{
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
	scm := fakeFromTags([]tagSpec{
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
	scm := fakeFromTags([]tagSpec{
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

func TestModuleVersionsForRepo_SynthesizesPseudoVersion(t *testing.T) {
	commitDate := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"

	tests := []struct {
		name         string
		oid          string
		goModContent string
		want         []*mod.ModuleVersion
	}{
		{
			name:         "root module",
			oid:          oid,
			goModContent: "module go.example.com/thing\n",
			want:         []*mod.ModuleVersion{{Version: "v0.0.0-20260102030405-abcdef012345", Created: commitDate, ModulePath: "go.example.com/thing"}},
		},
		{
			name:         "v2 module",
			oid:          oid,
			goModContent: "module go.example.com/thing/v2\n",
			want:         []*mod.ModuleVersion{{Version: "v2.0.0-20260102030405-abcdef012345", Created: commitDate, ModulePath: "go.example.com/thing/v2"}},
		},
		{
			name: "no go.mod at HEAD",
			oid:  oid,
			want: nil,
		},
		{
			name: "empty repo has no HEAD",
			oid:  "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// HEAD's go.mod is fetched at the commit oid, so key the fake on it.
			scm := &fakeSCM{headOID: tc.oid, headAt: commitDate, goMods: map[string]string{}}
			if tc.goModContent != "" {
				scm.goMods[tc.oid] = tc.goModContent
			}

			if diff := cmp.Diff(tc.want, versionsForRepo(t, scm)); diff != "" {
				t.Errorf("moduleVersionsForRepo mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
