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
		base       string
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
			name:       "release base bumps the patch",
			modulePath: "go.example.com/thing",
			base:       "v1.2.3",
			commitOID:  oid,
			want:       "v1.2.4-0.20260102030405-abcdef012345",
		},
		{
			name:       "prerelease base is extended",
			modulePath: "go.example.com/thing",
			base:       "v1.2.3-rc.1",
			commitOID:  oid,
			want:       "v1.2.3-rc.1.0.20260102030405-abcdef012345",
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
			got := PseudoVersion(tc.modulePath, tc.base, committed, tc.commitOID)
			if got != tc.want {
				t.Errorf("PseudoVersion(%q, %q, committed, %q) = %q, want %q", tc.modulePath, tc.base, tc.commitOID, got, tc.want)
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

func TestMajorSubdir(t *testing.T) {
	tests := []struct {
		name    string
		subdir  string
		version string
		want    string
	}{
		{name: "v0 has none", version: "v0.5.0", want: ""},
		{name: "v1 has none", version: "v1.2.3", want: ""},
		{name: "v2 at the root", version: "v2.0.0", want: "v2"},
		{name: "v2 under a subdir", subdir: "tracing", version: "v2.0.0", want: "tracing/v2"},
		{name: "v3 under a nested subdir", subdir: "client/pkg", version: "v3.5.0", want: "client/pkg/v3"},
		{name: "prerelease still counts", subdir: "kafka", version: "v4.0.1-rc.1", want: "kafka/v4"},
		{name: "not a version has none", subdir: "tracing", version: "nope", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := MajorSubdir(tc.subdir, tc.version); got != tc.want {
				t.Errorf("MajorSubdir(%q, %q) = %q, want %q", tc.subdir, tc.version, got, tc.want)
			}
		})
	}
}

func TestPseudoVersionBaseFromTags(t *testing.T) {
	tests := []struct {
		name       string
		modulePath string
		subdir     string
		tags       []string
		want       string
	}{
		{name: "no tags", modulePath: "go.example.com/thing"},
		{
			name:       "canonical tag bases a version",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1.2.3"},
			want:       "v1.2.3",
		},
		{
			name:       "prerelease tag bases a version",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1.2.3-rc.1+meta"},
			want:       "v1.2.3-rc.1",
		},
		{
			name:       "pseudo-version tag bases nothing",
			modulePath: "go.example.com/thing",
			tags:       []string{"v0.0.0-20240101000000-abcdef123456"},
		},
		{
			name:       "build metadata tag bases a version",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1.2.3+meta"},
			want:       "v1.2.3",
		},
		{
			name:       "highest tag wins",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1.2.3+meta", "v1.9.0+meta", "v1.4.0+meta"},
			want:       "v1.9.0",
		},
		{
			name:       "prerelease sorts below its release",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1.9.0+meta", "v1.9.0-rc.1+meta"},
			want:       "v1.9.0",
		},
		{
			name:       "shorthand and non-semver tags don't base anything",
			modulePath: "go.example.com/thing",
			tags:       []string{"v1", "v1.2", "0.0.24", "_gheMigrationPR-1", "latest"},
		},
		{
			name:       "major version has to match the path",
			modulePath: "go.example.com/thing",
			tags:       []string{"v2.0.0+meta"},
		},
		{
			name:       "major version matching the path counts",
			modulePath: "go.example.com/thing/v2",
			subdir:     "",
			tags:       []string{"v2.3.0+meta"},
			want:       "v2.3.0",
		},
		{
			name:       "only tags for this subdir count",
			modulePath: "go.example.com/thing/tracing",
			subdir:     "tracing",
			tags:       []string{"v1.9.0+meta", "tracing/v0.4.0+meta", "auth/v3.0.0+meta"},
			want:       "v0.4.0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PseudoVersionBaseFromTags(tc.modulePath, tc.subdir, tc.tags); got != tc.want {
				t.Errorf("PseudoVersionBaseFromTags(%q, %q, %v) = %q, want %q", tc.modulePath, tc.subdir, tc.tags, got, tc.want)
			}
		})
	}
}

func TestTagPrefix(t *testing.T) {
	tests := []struct {
		name       string
		subdir     string
		modulePath string
		want       string
	}{
		{name: "root module", modulePath: "go.example.com/thing", want: ""},
		{name: "subdir module", subdir: "tracing", modulePath: "go.example.com/thing/tracing", want: "tracing"},
		{name: "major subdir drops its version element", subdir: "client/v3", modulePath: "go.example.com/thing/client/v3", want: "client"},
		{name: "major version at the root", subdir: "v2", modulePath: "go.example.com/thing/v2", want: ""},
		{name: "major in the path but not the dir", subdir: "kafka", modulePath: "go.example.com/thing/kafka/v4", want: "kafka"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TagPrefix(tc.subdir, tc.modulePath); got != tc.want {
				t.Errorf("TagPrefix(%q, %q) = %q, want %q", tc.subdir, tc.modulePath, got, tc.want)
			}
		})
	}
}
