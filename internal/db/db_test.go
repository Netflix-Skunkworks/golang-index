package db_test

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestFetchRepoTags(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	allTags := []*db.RepoTag{
		// Ordered by Created DESC, which is how we expect it returned.
		{OrgRepoName: "foo/gaz", TagName: "v0.0.1", Created: time.Now().Add(time.Minute)},
		{OrgRepoName: "foo/bar", TagName: "v0.0.2", Created: time.Now().Add(time.Second)},
		{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now()},
	}
	populateRepoTags(t, sqlDB, allTags)

	// Get all.
	gotTags, err := sutDB.FetchRepoTags(t.Context(), time.Now().Add(-1*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags, gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoTags: -want,+got: %s", diff)
	}

	// Get with limit.
	gotTags, err = sutDB.FetchRepoTags(t.Context(), time.Now().Add(-1*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags[:2], gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoTags: -want,+got: %s", diff)
	}

	// Get with since.
	gotTags, err = sutDB.FetchRepoTags(t.Context(), time.Now().Add(2*time.Second), 1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags[:1], gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoTags: -want,+got: %s", diff)
	}
}

func TestStoreRepos(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar", "gaz/urk"}); err != nil {
		t.Fatal(err)
	}

	gotRepos := slices.Sorted(maps.Keys(repoTags(t, sqlDB)))
	wantRepos := []string{"foo/bar", "gaz/urk"}
	if diff := cmp.Diff(wantRepos, gotRepos); diff != "" {
		t.Errorf("StoreRepos: -want,+got: %s", diff)
	}

	// Repeated storing same repo has no effect.
	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar"}); err != nil {
		t.Fatal(err)
	}
	gotRepos = slices.Sorted(maps.Keys(repoTags(t, sqlDB)))
	if diff := cmp.Diff(wantRepos, gotRepos); diff != "" {
		t.Errorf("StoreRepos: -want,+got: %s", diff)
	}
}

func TestStoreRepoTags(t *testing.T) {
	// Whenever we store tags for a repo, all pre-existing tags are removed.
	// Only new tags remain.
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar", "foo/gaz"}); err != nil {
		t.Fatal(err)
	}
	preExistingTag1 := db.RepoTag{OrgRepoName: "foo/gaz", TagName: "v0.0.1", Created: time.Now()}
	preExistingTag2 := db.RepoTag{OrgRepoName: "foo/gaz", TagName: "v0.0.2", Created: time.Now()}
	newTag := db.RepoTag{OrgRepoName: "foo/gaz", TagName: "v0.0.3", Created: time.Now()}
	preExistingTag3 := db.RepoTag{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now()}

	populateRepoTags(t, sqlDB, []*db.RepoTag{&preExistingTag1, &preExistingTag2, &preExistingTag3})

	// newTag is new. preExistingTag2 is not included.
	if err := sutDB.StoreRepoTags(t.Context(), []*db.RepoTag{&preExistingTag1, &newTag, &preExistingTag3}); err != nil {
		t.Fatal(err)
	}

	want := map[string][]*db.RepoTag{
		"foo/gaz": {&preExistingTag1, &newTag},
		"foo/bar": {&preExistingTag3},
	}
	gotRepoTags := repoTags(t, sqlDB)
	if diff := cmp.Diff(want, gotRepoTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("StoreRepoTags: -want,+got: %s", diff)
	}
}

// Both the "All repos" and "Tags for one repo" reindexing work queues work the
// same way. So, we can share a single set of test cases for both.
type reindexWorkerTestCase struct {
	name                 string
	lastIndexingBegan    time.Time
	lastIndexingFinished time.Time
	reindexTTL           time.Duration
	reindexPeriod        time.Duration // We should reindex after this period of time.
	expectReindex        bool
}

var reindexWorkerTestCases = []*reindexWorkerTestCase{
	{
		// We re-indexed long ago: we should do so again.
		name:                 "beyond reindex period",
		lastIndexingBegan:    time.Now().Add(-24 * time.Hour),
		lastIndexingFinished: time.Now().Add(-24 * time.Hour),
		reindexTTL:           time.Minute,
		reindexPeriod:        time.Hour,
		expectReindex:        true,
	},
	{
		// We re-indexed long ago, but another worker is busy re-indexing: don't re-index.
		name:                 "beyond reindex period but another worker busy",
		lastIndexingBegan:    time.Now().Add(-1 * time.Minute), // The other worker only started 1m ago, and has 5m: give it more time.
		lastIndexingFinished: time.Now().Add(-24 * time.Hour),
		reindexTTL:           5 * time.Minute,
		reindexPeriod:        time.Hour,
		expectReindex:        false,
	},
	{
		// We re-indexed long ago, but another worker is busy re-indexing: don't re-index.
		name:                 "beyond reindex period and another worker stalled",
		lastIndexingBegan:    time.Now().Add(-6 * time.Minute), // The other worker only started 6m ago, and has 5m: it's stalled, so take over.
		lastIndexingFinished: time.Now().Add(-24 * time.Hour),
		reindexTTL:           5 * time.Minute,
		reindexPeriod:        time.Hour,
		expectReindex:        true,
	},
	{
		// We've re-indexed recently: no point doing so again.
		name:                 "within reindex period",
		lastIndexingBegan:    time.Now().Add(-10 * time.Minute),
		lastIndexingFinished: time.Now().Add(-10 * time.Minute),
		reindexTTL:           time.Minute,
		reindexPeriod:        time.Hour,
		expectReindex:        false,
	},
	{
		// We're beyond the re-indexing TTL. But, since we're still within the re-indexing period, no need to re-index.
		name:                 "within reindex period despite recent start",
		lastIndexingBegan:    time.Now().Add(-10 * time.Minute),
		lastIndexingFinished: time.Now().Add(-10 * time.Minute),
		reindexTTL:           time.Second, // The last re-indexing worker had 1s to finish, and it's far beyond that TTL.
		reindexPeriod:        time.Hour,
		expectReindex:        false,
	},
}

func TestNextReindexAllReposWork_Basic(t *testing.T) {
	sutDB, sqlDB := setupDB(t)

	for _, tc := range reindexWorkerTestCases {
		t.Run(tc.name, func(t *testing.T) {
			resetTables(t, sqlDB)
			setAllReposIndexing(t, sqlDB, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))
			shouldReindex, err := sutDB.NextReindexAllReposWork(t.Context(), 5*time.Minute, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := shouldReindex, true; got != want {
				t.Errorf("expected shouldReindex=%v, got %v", want, got)
			}
		})
	}

}

func TestNextReindexAllReposWork_QuickSuccession(t *testing.T) {
	// The first call should return work, second should not, since asking for
	// the first time should return & update it.

	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)
	setAllReposIndexing(t, sqlDB, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))

	// Take work for the first time: should return true.
	shouldReindex, err := sutDB.NextReindexAllReposWork(t.Context(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shouldReindex, true; got != want {
		t.Errorf("expected shouldReindex=%v, got %v", want, got)
	}

	// Try to take work the second time: should return false.
	shouldReindex, err = sutDB.NextReindexAllReposWork(t.Context(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shouldReindex, false; got != want {
		t.Errorf("expected shouldReindex=%v, got %v", want, got)
	}
}

func TestNextReindexRepoTagsWork_SingleRepo(t *testing.T) {
	sutDB, sqlDB := setupDB(t)

	for _, tc := range reindexWorkerTestCases {
		t.Run(tc.name, func(t *testing.T) {
			resetTables(t, sqlDB)
			populateRepoTags(t, sqlDB, []*db.RepoTag{{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)}})
			setSingleRepoIndexing(t, sqlDB, "foo/bar", tc.lastIndexingBegan, tc.lastIndexingFinished)

			gotRepoToReindex, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), tc.reindexTTL, tc.reindexPeriod)
			if err != nil {
				t.Fatal(err)
			}

			if tc.expectReindex {
				if !gotWork {
					t.Fatalf("NextReindexRepoTagsWork: expected work but got none")
				}
				if gotRepoToReindex != "foo/bar" {
					t.Errorf("NextReindexRepoTagsWork: expected foo/bar but got %s", gotRepoToReindex)
				}
			} else {
				if gotWork {
					t.Errorf("NextReindexRepoTagsWork: expected no work, but got some: %s", gotRepoToReindex)
				}
			}
		})
	}
}

func TestNextReindexRepoTagsWork_NoRepos(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)
	_, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotWork, false; got != want {
		t.Errorf("expected gotWork=%v, got %v", want, got)
	}
}

func TestNextReindexRepoTagsWork_QuickSuccession(t *testing.T) {
	// The first call should return work, second should not, since asking for
	// the first time should return & update it.

	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)
	populateRepoTags(t, sqlDB, []*db.RepoTag{{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)}})
	setSingleRepoIndexing(t, sqlDB, "foo/bar", time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))

	// Take work for the first time: should return true.
	_, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotWork, true; got != want {
		t.Errorf("expected gotWork=%v, got %v", want, got)
	}

	// Try to take work the second time: should return false.
	_, gotWork, err = sutDB.NextReindexRepoTagsWork(t.Context(), 5*time.Minute, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotWork, false; got != want {
		t.Errorf("expected gotWork=%v, got %v", want, got)
	}
}

func TestNextReindexRepoTagsWork_MultipleRepo_TakeReindexNeeded(t *testing.T) {
	// When one repo needs re-indexing and another doesn't, take the one that does.

	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	populateRepoTags(t, sqlDB, []*db.RepoTag{
		{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "gaz/urk", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
	})

	// Does not need re-indexing (based on reindex period specified a bit below).
	setSingleRepoIndexing(t, sqlDB, "foo/bar", time.Now().Add(-1*time.Minute), time.Now().Add(-1*time.Minute))
	// Needs re-indexing (based on reindex period specified a bit below).
	setSingleRepoIndexing(t, sqlDB, "gaz/urk", time.Now().Add(-1*time.Hour), time.Now().Add(-1*time.Hour))

	gotRepoToReindex, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), 10*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !gotWork {
		t.Fatalf("NextReindexRepoTagsWork: expected work but got none")
	}
	if gotRepoToReindex != "gaz/urk" {
		t.Errorf("NextReindexRepoTagsWork: expected gaz/urk but got %s", gotRepoToReindex)
	}
}

func TestNextReindexRepoTagsWork_MultipleRepo_TakeOldestNeedingReindexing(t *testing.T) {
	// When multiple repos need re-indexing, take the oldest.

	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	populateRepoTags(t, sqlDB, []*db.RepoTag{
		{OrgRepoName: "foo/bar", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "bee/doh", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "gaz/urk", TagName: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
	})

	// All need re-indexing (based on reindex period specified a bit below).
	// But, the second needs it more since it's been longer.
	setSingleRepoIndexing(t, sqlDB, "foo/bar", time.Now().Add(-50*time.Minute), time.Now().Add(-50*time.Minute))
	setSingleRepoIndexing(t, sqlDB, "bee/doh", time.Now().Add(-70*time.Minute), time.Now().Add(-70*time.Minute))
	setSingleRepoIndexing(t, sqlDB, "gaz/urk", time.Now().Add(-60*time.Minute), time.Now().Add(-60*time.Minute))

	gotRepoToReindex, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), 10*time.Minute, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !gotWork {
		t.Fatalf("NextReindexRepoTagsWork: expected work but got none")
	}
	if gotRepoToReindex != "bee/doh" {
		t.Errorf("NextReindexRepoTagsWork: expected bee/doh but got %s", gotRepoToReindex)
	}
}
