package indexer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/cenkalti/backoff/v5"
	"github.com/google/go-cmp/cmp"
)

// smallBackoff pauses for at most a millisecond, so retry paths don't slow the
// tests down.
func smallBackoff() *backoff.ExponentialBackOff {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = time.Millisecond
	b.MaxInterval = time.Millisecond
	return b
}

func TestAllReposIndexer_StoresRepos(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored [][]string
	store := &fakeAllReposStore{
		nextReindexAllReposWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			return true, nil
		},
		storeRepos: func(_ context.Context, orgRepoNames []string) error {
			stored = append(stored, orgRepoNames)
			cancel()
			return nil
		},
	}
	lister := &fakeRepoLister{
		goRepos: func(context.Context) ([]string, error) {
			return []string{"org/a", "org/b"}, nil
		},
	}
	ix := &AllReposIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
		ReindexTTL:      time.Minute,
		ReindexPeriod:   time.Minute,
	}

	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if diff := cmp.Diff([][]string{{"org/a", "org/b"}}, stored); diff != "" {
		t.Errorf("stored repos mismatch (-want +got):\n%s", diff)
	}
}

func TestAllReposIndexer_NoWorkStoresNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := &fakeAllReposStore{
		nextReindexAllReposWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			cancel()
			return false, nil
		},
		storeRepos: func(context.Context, []string) error {
			t.Errorf("StoreRepos called, want no call when there's no work")
			return nil
		},
	}
	lister := &fakeRepoLister{
		goRepos: func(context.Context) ([]string, error) {
			t.Errorf("GoRepos called, want no call when there's no work")
			return nil, nil
		},
	}
	ix := &AllReposIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
	}

	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestAllReposIndexer_RetriesAfterGitHubError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored [][]string
	store := &fakeAllReposStore{
		nextReindexAllReposWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			return true, nil
		},
		storeRepos: func(_ context.Context, orgRepoNames []string) error {
			stored = append(stored, orgRepoNames)
			cancel()
			return nil
		},
	}
	goReposCalls := 0
	lister := &fakeRepoLister{
		goRepos: func(context.Context) ([]string, error) {
			goReposCalls++
			if goReposCalls == 1 {
				return nil, errors.New("github boom")
			}
			return []string{"org/a"}, nil
		},
	}
	ix := &AllReposIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
	}

	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if goReposCalls != 2 {
		t.Errorf("GoRepos called %d times, want 2 (one failure, one retry)", goReposCalls)
	}
	if diff := cmp.Diff([][]string{{"org/a"}}, stored); diff != "" {
		t.Errorf("stored repos mismatch (-want +got):\n%s", diff)
	}
}

func newRepoTagsIndexer(store *fakeRepoTagsStore, scm scm) *RepoTagsIndexer {
	return &RepoTagsIndexer{
		DB:                store,
		SCM:               scm,
		DefaultModuleHost: testModuleHost,
		bo:                smallBackoff(),
		WorkCheckPeriod:   time.Hour,
		ReindexTTL:        time.Minute,
		ReindexPeriod:     time.Minute,
	}
}

func TestRepoTagsIndexer_StoresTags(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	date := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var stored [][]*db.RepoModuleVersion
	handedOut := false
	store := &fakeRepoTagsStore{
		nextReindexRepoTagsWork: func(context.Context, time.Duration, time.Duration) (string, bool, error) {
			if handedOut {
				cancel()
				return "", false, nil
			}
			handedOut = true
			return "someorg/repo1", true, nil
		},
		storeRepoTags: func(_ context.Context, repoModuleVersions []*db.RepoModuleVersion) error {
			stored = append(stored, repoModuleVersions)
			return nil
		},
	}
	scm := fakeFromTags([]tagSpec{{tag: "v1.0.0", date: date}})

	if err := newRepoTagsIndexer(store, scm).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}

	want := [][]*db.RepoModuleVersion{{
		{OrgRepoName: "someorg/repo1", Version: "v1.0.0", ModulePath: testModuleHost + "/someorg/repo1", Created: date},
	}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
	}
}

func TestRepoTagsIndexer_SkipsStoreWhenNoModuleVersions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	handedOut := false
	store := &fakeRepoTagsStore{
		nextReindexRepoTagsWork: func(context.Context, time.Duration, time.Duration) (string, bool, error) {
			if handedOut {
				cancel()
				return "", false, nil
			}
			handedOut = true
			return "someorg/repo1", true, nil
		},
		// No tags and no HEAD commit means no module versions, so StoreRepoModuleVersions
		// must not be called (it errors on an empty slice).
		storeRepoTags: func(context.Context, []*db.RepoModuleVersion) error {
			t.Errorf("StoreRepoModuleVersions called, want no call when there are no module versions")
			return nil
		},
	}

	if err := newRepoTagsIndexer(store, &fakeSCM{}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestRepoTagsIndexer_RetriesAfterGitHubError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	date := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var stored [][]*db.RepoModuleVersion
	workCalls := 0
	store := &fakeRepoTagsStore{
		nextReindexRepoTagsWork: func(context.Context, time.Duration, time.Duration) (string, bool, error) {
			workCalls++
			if workCalls > 2 {
				cancel()
				return "", false, nil
			}
			return "someorg/repo1", true, nil
		},
		storeRepoTags: func(_ context.Context, repoModuleVersions []*db.RepoModuleVersion) error {
			stored = append(stored, repoModuleVersions)
			return nil
		},
	}
	scm := fakeFromTags([]tagSpec{{tag: "v1.0.0", date: date}})
	scm.repoTagsFails = 1

	if err := newRepoTagsIndexer(store, scm).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if scm.repoTagsCalls != 2 {
		t.Errorf("RepoTags called %d times, want 2 (one failure, one retry)", scm.repoTagsCalls)
	}

	want := [][]*db.RepoModuleVersion{{
		{OrgRepoName: "someorg/repo1", Version: "v1.0.0", ModulePath: testModuleHost + "/someorg/repo1", Created: date},
	}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
	}
}
