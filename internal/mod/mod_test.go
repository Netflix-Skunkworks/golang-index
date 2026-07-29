package mod

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestModuleVersionFromTag(t *testing.T) {
	type result struct {
		subdir  string
		version string
		ok      bool
	}
	tests := []struct {
		name string
		tag  string
		want result
	}{
		{name: "root version", tag: "v1.0.0", want: result{version: "v1.0.0", ok: true}},
		{name: "root prerelease", tag: "v1.0.0-rc.1", want: result{version: "v1.0.0-rc.1", ok: true}},
		{name: "root major version 2", tag: "v2.3.4", want: result{version: "v2.3.4", ok: true}},
		{name: "subdir version", tag: "tracing/v0.2.2", want: result{subdir: "tracing", version: "v0.2.2", ok: true}},
		{name: "nested subdir version", tag: "cmd/tool/v0.1.0", want: result{subdir: "cmd/tool", version: "v0.1.0", ok: true}},
		{name: "major-only pointer", tag: "v1", want: result{}},
		{name: "major-only pointer v2", tag: "v2", want: result{}},
		{name: "minor-only pointer", tag: "v1.2", want: result{}},
		{name: "subdir major-only pointer", tag: "tracing/v2", want: result{}},
		{name: "build metadata", tag: "v1.0.0+incompatible", want: result{}},
		{name: "missing v prefix", tag: "1.0.0", want: result{}},
		{name: "not a version", tag: "_gheMigrationPR-435", want: result{}},
		{name: "subdir non-version", tag: "docs/latest", want: result{}},
		{name: "bare number", tag: "slides/2", want: result{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			subdir, version, ok := ModuleVersionFromTag(tc.tag)
			got := result{subdir: subdir, version: version, ok: ok}
			if diff := cmp.Diff(tc.want, got, cmp.AllowUnexported(result{})); diff != "" {
				t.Errorf("ModuleVersionFromTag(%q) mismatch (-want +got):\n%s", tc.tag, diff)
			}
		})
	}
}

func TestPseudoVersion(t *testing.T) {
	committed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const oid = "abcdef0123456789abcdef0123456789abcdef01"

	tests := []struct {
		name       string
		modulePath string
		commitOID  string
		want       string
	}{
		{
			name:       "root module is v0",
			modulePath: "go.example.com/thing",
			commitOID:  oid,
			want:       "v0.0.0-20260102030405-abcdef012345",
		},
		{
			name:       "v2 module keeps its major",
			modulePath: "go.example.com/thing/v2",
			commitOID:  oid,
			want:       "v2.0.0-20260102030405-abcdef012345",
		},
		{
			name:       "oid shorter than 12 chars is used whole",
			modulePath: "go.example.com/thing",
			commitOID:  "abcdef",
			want:       "v0.0.0-20260102030405-abcdef",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PseudoVersion(tc.modulePath, tc.commitOID, committed)
			if got != tc.want {
				t.Errorf("PseudoVersion(%q, %q, ...) = %q, want %q", tc.modulePath, tc.commitOID, got, tc.want)
			}
		})
	}
}

func TestRepoModulePath(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		orgRepoName string
		subdir      string
		want        string
	}{
		{name: "root module", host: "github.example.net", orgRepoName: "someorg/repo1", want: "github.example.net/someorg/repo1"},
		{name: "subdir module", host: "github.example.net", orgRepoName: "someorg/repo1", subdir: "tracing", want: "github.example.net/someorg/repo1/tracing"},
		{name: "nested subdir module", host: "github.example.net", orgRepoName: "someorg/repo1", subdir: "cmd/tool", want: "github.example.net/someorg/repo1/cmd/tool"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoModulePath(tc.host, tc.orgRepoName, tc.subdir); got != tc.want {
				t.Errorf("RepoModulePath(%q, %q, %q) = %q, want %q", tc.host, tc.orgRepoName, tc.subdir, got, tc.want)
			}
		})
	}
}

func TestIncompatibleVersion(t *testing.T) {
	tests := []struct {
		name    string
		subdir  string
		version string
		want    string
	}{
		{name: "v0 needs no suffix", version: "v0.3.0", want: "v0.3.0"},
		{name: "v1 needs no suffix", version: "v1.2.3", want: "v1.2.3"},
		{name: "v2 gets the suffix", version: "v2.0.0", want: "v2.0.0+incompatible"},
		{name: "v10 gets the suffix", version: "v10.1.2", want: "v10.1.2+incompatible"},
		{name: "v2 prerelease gets the suffix", version: "v2.0.0-rc.1", want: "v2.0.0-rc.1+incompatible"},
		{name: "later prerelease gets the suffix", version: "v4.1.0-rc.2", want: "v4.1.0-rc.2+incompatible"},
		{name: "subdir module can't be incompatible", subdir: "tracing", version: "v2.0.0", want: "v2.0.0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IncompatibleVersion(tc.subdir, tc.version); got != tc.want {
				t.Errorf("IncompatibleVersion(%q, %q) = %q, want %q", tc.subdir, tc.version, got, tc.want)
			}
		})
	}
}
