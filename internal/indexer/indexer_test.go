package indexer

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
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

func TestAllOwnersIndexer_StoresOwners(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored [][]string
	store := &fakeAllOwnersStore{
		nextReindexAllOwnersWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			return true, nil
		},
		storeOwners: func(_ context.Context, ownerLogins []string) error {
			stored = append(stored, ownerLogins)
			cancel()
			return nil
		},
	}
	lister := &fakeOwnerLister{
		owners: func(context.Context) ([]string, error) {
			return []string{"orga", "orgb"}, nil
		},
	}
	ix := &AllOwnersIndexer{
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
	if diff := cmp.Diff([][]string{{"orga", "orgb"}}, stored); diff != "" {
		t.Errorf("stored owners mismatch (-want +got):\n%s", diff)
	}
}

func TestAllOwnersIndexer_NoWorkStoresNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := &fakeAllOwnersStore{
		nextReindexAllOwnersWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			cancel()
			return false, nil
		},
		storeOwners: func(context.Context, []string) error {
			t.Errorf("StoreOwners called, want no call when there's no work")
			return nil
		},
	}
	lister := &fakeOwnerLister{
		owners: func(context.Context) ([]string, error) {
			t.Errorf("Owners called, want no call when there's no work")
			return nil, nil
		},
	}
	ix := &AllOwnersIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
	}

	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

func TestAllOwnersIndexer_RetriesAfterGitHubError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored [][]string
	store := &fakeAllOwnersStore{
		nextReindexAllOwnersWork: func(context.Context, time.Duration, time.Duration) (bool, error) {
			return true, nil
		},
		storeOwners: func(_ context.Context, ownerLogins []string) error {
			stored = append(stored, ownerLogins)
			cancel()
			return nil
		},
	}
	ownersCalls := 0
	lister := &fakeOwnerLister{
		owners: func(context.Context) ([]string, error) {
			ownersCalls++
			if ownersCalls == 1 {
				return nil, errors.New("github boom")
			}
			return []string{"orga"}, nil
		},
	}
	ix := &AllOwnersIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
	}

	if err := ix.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if ownersCalls != 2 {
		t.Errorf("Owners called %d times, want 2 (one failure, one retry)", ownersCalls)
	}
	if diff := cmp.Diff([][]string{{"orga"}}, stored); diff != "" {
		t.Errorf("stored owners mismatch (-want +got):\n%s", diff)
	}
}

func newOwnerReposIndexer(store *fakeOwnerReposStore, lister *fakeOwnerRepoLister) *OwnerReposIndexer {
	return &OwnerReposIndexer{
		DB:              store,
		Lister:          lister,
		bo:              smallBackoff(),
		WorkCheckPeriod: time.Hour,
		ReindexTTL:      time.Minute,
		ReindexPeriod:   time.Minute,
	}
}

// handOutOnce yields ownerLogin once, then cancels ctx so Run exits.
func handOutOnce(ownerLogin string, cancel context.CancelFunc) func(context.Context, time.Duration, time.Duration) (string, bool, error) {
	handedOut := false
	return func(context.Context, time.Duration, time.Duration) (string, bool, error) {
		if handedOut {
			cancel()
			return "", false, nil
		}
		handedOut = true
		return ownerLogin, true, nil
	}
}

func TestOwnerReposIndexer_StoresRepos(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored []ownerStoreCall
	store := &fakeOwnerReposStore{
		nextReindexOwnerReposWork: handOutOnce("someorg", cancel),
		storeOwnerRepos: func(_ context.Context, ownerLogin string, orgRepoNames []string) error {
			stored = append(stored, ownerStoreCall{ownerLogin, orgRepoNames})
			return nil
		},
	}
	lister := &fakeOwnerRepoLister{
		ownerGoRepos: func(_ context.Context, ownerLogin string) ([]string, error) {
			return []string{ownerLogin + "/a", ownerLogin + "/b"}, nil
		},
	}

	if err := newOwnerReposIndexer(store, lister).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}

	want := []ownerStoreCall{{"someorg", []string{"someorg/a", "someorg/b"}}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored owner repos mismatch (-want +got):\n%s", diff)
	}
}

func TestOwnerReposIndexer_StoresWhenNoRepos(t *testing.T) {
	// An owner with no Go repos is stored too: that completes its work item, so it
	// waits out ReindexPeriod instead of being handed out again every ReindexTTL.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored []ownerStoreCall
	store := &fakeOwnerReposStore{
		nextReindexOwnerReposWork: handOutOnce("someorg", cancel),
		storeOwnerRepos: func(_ context.Context, ownerLogin string, orgRepoNames []string) error {
			stored = append(stored, ownerStoreCall{ownerLogin, orgRepoNames})
			return nil
		},
	}
	lister := &fakeOwnerRepoLister{
		ownerGoRepos: func(context.Context, string) ([]string, error) { return nil, nil },
	}

	if err := newOwnerReposIndexer(store, lister).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}

	want := []ownerStoreCall{{"someorg", nil}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored owner repos mismatch (-want +got):\n%s", diff)
	}
}

func TestOwnerReposIndexer_RetriesAfterGitHubError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored []ownerStoreCall
	workCalls := 0
	store := &fakeOwnerReposStore{
		nextReindexOwnerReposWork: func(context.Context, time.Duration, time.Duration) (string, bool, error) {
			workCalls++
			if workCalls > 2 {
				cancel()
				return "", false, nil
			}
			return "someorg", true, nil
		},
		storeOwnerRepos: func(_ context.Context, ownerLogin string, orgRepoNames []string) error {
			stored = append(stored, ownerStoreCall{ownerLogin, orgRepoNames})
			return nil
		},
	}
	goReposCalls := 0
	lister := &fakeOwnerRepoLister{
		ownerGoRepos: func(_ context.Context, ownerLogin string) ([]string, error) {
			goReposCalls++
			if goReposCalls == 1 {
				return nil, errors.New("github boom")
			}
			return []string{ownerLogin + "/a"}, nil
		},
	}

	if err := newOwnerReposIndexer(store, lister).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if goReposCalls != 2 {
		t.Errorf("OwnerGoRepos called %d times, want 2 (one failure, one retry)", goReposCalls)
	}

	want := []ownerStoreCall{{"someorg", []string{"someorg/a"}}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored owner repos mismatch (-want +got):\n%s", diff)
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
	var stored []storeCall
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
		storeRepoModuleVersions: func(_ context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error {
			stored = append(stored, storeCall{orgRepoName, repoModuleVersions})
			return nil
		},
	}
	scm := fakeFromTags(t, []tagSpec{{tag: "v1.0.0", date: date}})

	if err := newRepoTagsIndexer(store, scm).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}

	want := []storeCall{{"someorg/repo1", []*db.RepoModuleVersion{
		{Version: "v1.0.0", ModulePath: testModuleHost + "/someorg/repo1", Created: date},
	}}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
	}
}

func TestRepoTagsIndexer_StoresWhenNoModuleVersions(t *testing.T) {
	// A repo with no tags and no HEAD commit has no module versions, and is still
	// stored: that completes its work item, so it waits out ReindexPeriod instead of
	// being handed out again every ReindexTTL.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stored []storeCall
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
		storeRepoModuleVersions: func(_ context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error {
			stored = append(stored, storeCall{orgRepoName, repoModuleVersions})
			return nil
		},
	}

	if err := newRepoTagsIndexer(store, &fakeSCM{}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}

	want := []storeCall{{"someorg/repo1", []*db.RepoModuleVersion{}}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
	}
}

func TestRepoTagsIndexer_StoresWhenErrorIsNotRetryable(t *testing.T) {
	tests := map[string]*fakeSCM{
		"tree refused": {
			headOID:       "abcdef0123456789abcdef0123456789abcdef01",
			moduleDirsErr: errors.New("access denied"),
		},
		"go.mod refused": {
			tags:     []github.Tag{{Name: "v1.0.0", Date: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}},
			goModErr: errors.New("access denied"),
		},
	}

	for name, scm := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var stored []storeCall
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
				storeRepoModuleVersions: func(_ context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error {
					stored = append(stored, storeCall{orgRepoName, repoModuleVersions})
					return nil
				},
			}

			if err := newRepoTagsIndexer(store, scm).Run(ctx); !errors.Is(err, context.Canceled) {
				t.Errorf("Run returned %v, want context.Canceled", err)
			}

			want := []storeCall{{"someorg/repo1", nil}}
			if diff := cmp.Diff(want, stored); diff != "" {
				t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRepoTagsIndexer_RetriesAfterGitHubError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	date := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	var stored []storeCall
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
		storeRepoModuleVersions: func(_ context.Context, orgRepoName string, repoModuleVersions []*db.RepoModuleVersion) error {
			stored = append(stored, storeCall{orgRepoName, repoModuleVersions})
			return nil
		},
	}
	scm := fakeFromTags(t, []tagSpec{{tag: "v1.0.0", date: date}})
	scm.repoTagsFails = 1

	if err := newRepoTagsIndexer(store, scm).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if scm.repoTagsCalls != 2 {
		t.Errorf("RepoTags called %d times, want 2 (one failure, one retry)", scm.repoTagsCalls)
	}

	want := []storeCall{{"someorg/repo1", []*db.RepoModuleVersion{
		{Version: "v1.0.0", ModulePath: testModuleHost + "/someorg/repo1", Created: date},
	}}}
	if diff := cmp.Diff(want, stored); diff != "" {
		t.Errorf("stored repo tags mismatch (-want +got):\n%s", diff)
	}
}

func TestRunQueue_ResetsBackoffAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = time.Millisecond
	bo.RandomizationFactor = 0

	passes := 0
	once := func(context.Context) (gotWork, retryable bool, err error) {
		passes++
		if passes == 1 {
			return true, true, errors.New("github boom")
		}
		cancel()
		return true, false, nil
	}

	if err := runQueue(ctx, slog.Default(), bo, time.Hour, once); !errors.Is(err, context.Canceled) {
		t.Errorf("runQueue returned %v, want context.Canceled", err)
	}
	if passes != 2 {
		t.Errorf("runQueue made %d passes, want 2 (one failure, one success)", passes)
	}
	if got := bo.NextBackOff(); got != bo.InitialInterval {
		t.Errorf("backoff after a successful pass = %v, want the initial %v", got, bo.InitialInterval)
	}
}
