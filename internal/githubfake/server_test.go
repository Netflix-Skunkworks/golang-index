package githubfake_test

import (
	"slices"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/githubfake"
	"github.com/google/go-cmp/cmp"
)

const sampleHeadOID = "0123456789abcdef0123456789abcdef01234567"

// sampleRepo is a two-module repo with a root tag and a subdirectory tag.
func sampleRepo() *githubfake.Repo {
	return &githubfake.Repo{
		Name:     "someorg/thing",
		HeadOID:  sampleHeadOID,
		HeadDate: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Tags: []githubfake.Tag{
			{Name: "v1.0.0", Date: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
			{Name: "tracing/v2.0.0", Date: time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)},
		},
		Files: map[string][]byte{
			"go.mod":         []byte("module go.example.com/thing\n"),
			"tracing/go.mod": []byte("module go.example.com/thing/tracing/v2\n"),
		},
	}
}

// newSCM points a real GithubSCM (and real githubv4 client) at a fake serving
// repos, exercising the actual request/response parsing in package github.
func newSCM(t *testing.T, repos ...*githubfake.Repo) *github.GithubSCM {
	t.Helper()
	srv := githubfake.NewServer(repos...)
	return github.NewEnterpriseSCM(githubfake.BaseURL, srv.Client())
}

func TestServerGoRepos(t *testing.T) {
	scm := newSCM(t, sampleRepo(), &githubfake.Repo{Name: "someorg/other"})

	got, err := scm.GoRepos(t.Context())
	if err != nil {
		t.Fatalf("GoRepos: %v", err)
	}
	// A sweep returns repos in owner order, so sort before comparing.
	want := []string{"someorg/other", "someorg/thing"}
	if diff := cmp.Diff(want, slices.Sorted(slices.Values(got))); diff != "" {
		t.Errorf("GoRepos() mismatch (-want +got):\n%s", diff)
	}
}

func TestServerRepoTags(t *testing.T) {
	scm := newSCM(t, sampleRepo())

	got, err := scm.RepoTags(t.Context(), "someorg/thing")
	if err != nil {
		t.Fatalf("RepoTags: %v", err)
	}
	want := []github.Tag{
		{Name: "v1.0.0", Date: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
		{Name: "tracing/v2.0.0", Date: time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoTags() mismatch (-want +got):\n%s", diff)
	}
}

func TestServerHeadCommit(t *testing.T) {
	scm := newSCM(t, sampleRepo())

	oid, committed, err := scm.HeadCommit(t.Context(), "someorg/thing")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if want := sampleHeadOID; oid != want {
		t.Errorf("HeadCommit() oid = %q, want %q", oid, want)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !committed.Equal(want) {
		t.Errorf("HeadCommit() committed = %v, want %v", committed, want)
	}
}

func TestServerHeadCommit_NoCommits(t *testing.T) {
	scm := newSCM(t, &githubfake.Repo{Name: "someorg/empty"})

	oid, _, err := scm.HeadCommit(t.Context(), "someorg/empty")
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if oid != "" {
		t.Errorf("HeadCommit() oid = %q, want empty", oid)
	}
}

func TestServerGoMod(t *testing.T) {
	scm := newSCM(t, sampleRepo())

	for _, tc := range []struct {
		name   string
		ref    string
		subdir string
		want   string
		found  bool
	}{
		{name: "root", ref: "v1.0.0", subdir: "", want: "module go.example.com/thing\n", found: true},
		{name: "subdir with slashed tag", ref: "tracing/v2.0.0", subdir: "tracing", want: "module go.example.com/thing/tracing/v2\n", found: true},
		{name: "missing", ref: "v1.0.0", subdir: "cmd", found: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content, found, err := scm.GoMod(t.Context(), "someorg/thing", tc.ref, tc.subdir)
			if err != nil {
				t.Fatalf("GoMod: %v", err)
			}
			if found != tc.found {
				t.Fatalf("GoMod() found = %v, want %v", found, tc.found)
			}
			if found && string(content) != tc.want {
				t.Errorf("GoMod() = %q, want %q", content, tc.want)
			}
		})
	}
}

// TestServerRefOverrides covers [githubfake.Repo.FilesAtRef]: content that
// differs at a single ref, and a file that exists only there.
func TestServerRefOverrides(t *testing.T) {
	repo := sampleRepo()
	repo.FilesAtRef = map[string]map[string][]byte{
		"v1.0.0":      {"go.mod": []byte("module go.example.com/thing/v1\n")},
		sampleHeadOID: {"cmd/tool/go.mod": []byte("module go.example.com/thing/cmd/tool\n")},
	}
	scm := newSCM(t, repo)

	for _, tc := range []struct {
		name string
		ref  string
		want string
	}{
		{name: "at the overridden ref", ref: "v1.0.0", want: "module go.example.com/thing/v1\n"},
		{name: "at any other ref", ref: "tracing/v2.0.0", want: "module go.example.com/thing\n"},
	} {
		t.Run("root go.mod "+tc.name, func(t *testing.T) {
			content, found, err := scm.GoMod(t.Context(), "someorg/thing", tc.ref, "")
			if err != nil {
				t.Fatalf("GoMod: %v", err)
			}
			if !found {
				t.Fatal("GoMod() found = false, want true")
			}
			if string(content) != tc.want {
				t.Errorf("GoMod() = %q, want %q", content, tc.want)
			}
		})
	}

	t.Run("tree includes a file only that ref has", func(t *testing.T) {
		got, err := scm.ModuleDirs(t.Context(), "someorg/thing", sampleHeadOID)
		if err != nil {
			t.Fatalf("ModuleDirs: %v", err)
		}
		want := []string{"cmd/tool", "", "tracing"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("ModuleDirs() mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestServerModuleDirs(t *testing.T) {
	scm := newSCM(t, sampleRepo())

	got, err := scm.ModuleDirs(t.Context(), "someorg/thing", sampleHeadOID)
	if err != nil {
		t.Fatalf("ModuleDirs: %v", err)
	}
	want := []string{"", "tracing"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ModuleDirs() mismatch (-want +got):\n%s", diff)
	}
}
