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

func TestFetchRepoModuleVersions(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	// UTC so the ?since= bound below and the stored indexed_at (a
	// timestamp-without-tz column holding UTC) compare correctly regardless of the
	// machine's local timezone.
	now := time.Now().UTC()
	// Ordered by IndexedAt ASC, as FetchRepoModuleVersions returns. The out-of-order Created
	// dates ensure the ordering can only come from IndexedAt.
	allTags := []*db.RepoModuleVersion{
		{OrgRepoName: "foo/bar", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/bar", Created: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), IndexedAt: now},
		{OrgRepoName: "foo/bar", Version: "v0.0.2", ModulePath: "github.somecompany.net/foo/bar", Created: time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC), IndexedAt: now.Add(time.Second)},
		{OrgRepoName: "foo/gaz", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/gaz", Created: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC), IndexedAt: now.Add(time.Minute)},
	}
	populateRepoModuleVersions(t, sqlDB, allTags)

	// Get all.
	gotTags, err := sutDB.FetchRepoModuleVersions(t.Context(), now.Add(-1*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags, gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoModuleVersions: -want,+got: %s", diff)
	}

	// Get with limit.
	gotTags, err = sutDB.FetchRepoModuleVersions(t.Context(), now.Add(-1*time.Hour), 2)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags[:2], gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoModuleVersions: -want,+got: %s", diff)
	}

	// Get with since: only the third tag was indexed after now+2s.
	gotTags, err = sutDB.FetchRepoModuleVersions(t.Context(), now.Add(2*time.Second), 1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(allTags[2:], gotTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoModuleVersions: -want,+got: %s", diff)
	}
}

func TestFetchRepoModuleVersions_SharedModulePath(t *testing.T) {
	// Where several repos claim one module version, the feed publishes it once, at
	// the earliest indexed_at; polling past that does not resurface the rest.
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	// UTC and whole seconds, the precision populateRepoModuleVersions stores
	// indexed_at with, so now.Add(time.Microsecond) below really does exclude now.
	now := time.Now().UTC().Truncate(time.Second)
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	firstSeen := &db.RepoModuleVersion{OrgRepoName: "someorg/shared-a", Version: "v1.0.0", ModulePath: "go.example.com/shared", Created: created, IndexedAt: now}
	seenLater := &db.RepoModuleVersion{OrgRepoName: "someorg/shared-b", Version: "v1.0.0", ModulePath: "go.example.com/shared", Created: created, IndexedAt: now.Add(2 * time.Second)}
	// Two repos seen at the same instant: org_repo_name breaks the tie, so tie-a wins.
	tieWinner := &db.RepoModuleVersion{OrgRepoName: "someorg/tie-a", Version: "v2.0.0", ModulePath: "go.example.com/tied", Created: created, IndexedAt: now.Add(4 * time.Second)}
	tieLoser := &db.RepoModuleVersion{OrgRepoName: "someorg/tie-b", Version: "v2.0.0", ModulePath: "go.example.com/tied", Created: created, IndexedAt: now.Add(4 * time.Second)}
	unshared := &db.RepoModuleVersion{OrgRepoName: "someorg/solo", Version: "v1.0.0", ModulePath: "go.example.com/solo", Created: created, IndexedAt: now.Add(6 * time.Second)}

	// tieLoser is stored before tieWinner, so a winner picked by insertion order
	// rather than by org_repo_name would fail below.
	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{firstSeen, seenLater, tieLoser, tieWinner, unshared})

	sinceBeforeAny := now.Add(-1 * time.Hour)
	got, err := sutDB.FetchRepoModuleVersions(t.Context(), sinceBeforeAny, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := []*db.RepoModuleVersion{firstSeen, tieWinner, unshared}
	if diff := cmp.Diff(want, got, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoModuleVersions(since=%v): -want,+got:\n%s", sinceBeforeAny, diff)
	}

	// Polling past the first sighting must not resurface the rows it stood in for.
	sincePastFirstSeen := now.Add(time.Microsecond)
	got, err = sutDB.FetchRepoModuleVersions(t.Context(), sincePastFirstSeen, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want = []*db.RepoModuleVersion{tieWinner, unshared}
	if diff := cmp.Diff(want, got, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("FetchRepoModuleVersions(since=%v): -want,+got:\n%s", sincePastFirstSeen, diff)
	}
}

func TestStoreRepos(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar", "gaz/urk"}); err != nil {
		t.Fatal(err)
	}

	gotRepos := slices.Sorted(maps.Keys(repoModuleVersions(t, sqlDB)))
	wantRepos := []string{"foo/bar", "gaz/urk"}
	if diff := cmp.Diff(wantRepos, gotRepos); diff != "" {
		t.Errorf("StoreRepos: -want,+got: %s", diff)
	}

	// Repeated storing same repo has no effect.
	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar"}); err != nil {
		t.Fatal(err)
	}
	gotRepos = slices.Sorted(maps.Keys(repoModuleVersions(t, sqlDB)))
	if diff := cmp.Diff(wantRepos, gotRepos); diff != "" {
		t.Errorf("StoreRepos: -want,+got: %s", diff)
	}
}

func TestStoreRepoModuleVersions(t *testing.T) {
	// Whenever we store module versions for a repo, all pre-existing ones are
	// removed; only the new set remains.
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar", "foo/gaz"}); err != nil {
		t.Fatal(err)
	}
	preExistingTag1 := db.RepoModuleVersion{OrgRepoName: "foo/gaz", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/gaz", Created: time.Now().UTC()}
	preExistingTag2 := db.RepoModuleVersion{OrgRepoName: "foo/gaz", Version: "v0.0.2", ModulePath: "github.somecompany.net/foo/gaz", Created: time.Now().UTC()}
	newTag := db.RepoModuleVersion{OrgRepoName: "foo/gaz", Version: "v0.0.3", ModulePath: "github.somecompany.net/foo/gaz", Created: time.Now().UTC()}
	preExistingTag3 := db.RepoModuleVersion{OrgRepoName: "foo/bar", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/bar", Created: time.Now().UTC()}

	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{&preExistingTag1, &preExistingTag2, &preExistingTag3})

	// Each repo is stored on its own, since a call is authoritative for one repo.
	// newTag is new. preExistingTag2 is not included, so it goes.
	if err := sutDB.StoreRepoModuleVersions(t.Context(), "foo/gaz", []*db.RepoModuleVersion{&preExistingTag1, &newTag}); err != nil {
		t.Fatal(err)
	}
	if err := sutDB.StoreRepoModuleVersions(t.Context(), "foo/bar", []*db.RepoModuleVersion{&preExistingTag3}); err != nil {
		t.Fatal(err)
	}

	want := map[string][]*db.RepoModuleVersion{
		"foo/gaz": {&preExistingTag1, &newTag},
		"foo/bar": {&preExistingTag3},
	}
	gotRepoTags := repoModuleVersions(t, sqlDB)
	if diff := cmp.Diff(want, gotRepoTags, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("StoreRepoModuleVersions: -want,+got: %s", diff)
	}
}

func TestStoreRepoModuleVersions_Colliding(t *testing.T) {
	// A repo with no semver tags gets one HEAD pseudo-version per module. Because a
	// pseudo-version is derived from the commit, not the module path, sibling v0
	// modules share an identical version string. They must coexist, keyed by
	// module_path; a single-batch insert keyed only on (repo, tag) crashed here
	// with "ON CONFLICT DO UPDATE command cannot affect row a second time".
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/multi"}); err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const pseudo = "v0.0.0-20260102030405-abcdef012345"
	tags := []*db.RepoModuleVersion{
		{OrgRepoName: "foo/multi", Version: pseudo, ModulePath: "github.somecompany.net/foo/multi", Created: created},
		{OrgRepoName: "foo/multi", Version: pseudo, ModulePath: "github.somecompany.net/foo/multi/cmd/tool", Created: created},
	}
	if err := sutDB.StoreRepoModuleVersions(t.Context(), "foo/multi", tags); err != nil {
		t.Fatalf("StoreRepoModuleVersions with colliding pseudo-versions: %v", err)
	}

	byModulePath := cmpopts.SortSlices(func(a, b *db.RepoModuleVersion) bool { return a.ModulePath < b.ModulePath })
	want := map[string][]*db.RepoModuleVersion{"foo/multi": tags}
	if diff := cmp.Diff(want, repoModuleVersions(t, sqlDB), byModulePath, cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("StoreRepoModuleVersions: -want,+got: %s", diff)
	}
}

func TestStoreRepoModuleVersions_NoModuleVersions(t *testing.T) {
	// A repo that yields no module versions is stored too: every row it has is stale,
	// so they all go, and its work item is completed so it waits out the re-index
	// period instead of being handed out again every re-indexing TTL.
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)

	gone := &db.RepoModuleVersion{OrgRepoName: "foo/gone", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/gone", Created: time.Now().UTC()}
	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{gone})

	if err := sutDB.StoreRepoModuleVersions(t.Context(), "foo/gone", nil); err != nil {
		t.Fatal(err)
	}

	// foo/gone keeps its repos row, so it still shows up, now with no module versions.
	want := map[string][]*db.RepoModuleVersion{"foo/gone": nil}
	if diff := cmp.Diff(want, repoModuleVersions(t, sqlDB), cmpopts.EquateApproxTime(time.Second)); diff != "" {
		t.Errorf("StoreRepoModuleVersions: -want,+got: %s", diff)
	}

	repoToReindex, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), 0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if gotWork {
		t.Errorf("NextReindexRepoTagsWork handed out %q, want no work: foo/gone finished within the reindex period", repoToReindex)
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
			setAllReposIndexing(t, sqlDB, tc.lastIndexingBegan, tc.lastIndexingFinished)

			shouldReindex, err := sutDB.NextReindexAllReposWork(t.Context(), tc.reindexTTL, tc.reindexPeriod)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := shouldReindex, tc.expectReindex; got != want {
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

func TestNextReindexAllReposWork_Roundtrip(t *testing.T) {
	// Storing repos completes the all-repos work item, so the re-index period holds
	// the next pass off. The TTL below is zero, which opens the "another worker is
	// still going" gate and leaves the period as the only thing that can.
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)
	setAllReposIndexing(t, sqlDB, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour))

	const reindexPeriod = time.Hour
	shouldReindex, err := sutDB.NextReindexAllReposWork(t.Context(), 5*time.Minute, reindexPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shouldReindex, true; got != want {
		t.Fatalf("expected shouldReindex=%v, got %v: a pass 24h ago is due again", want, got)
	}

	if err := sutDB.StoreRepos(t.Context(), []string{"foo/bar"}); err != nil {
		t.Fatal(err)
	}

	shouldReindex, err = sutDB.NextReindexAllReposWork(t.Context(), 0, reindexPeriod)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shouldReindex, false; got != want {
		t.Errorf("expected shouldReindex=%v, got %v: the pass finished within the reindex period", want, got)
	}
}

func TestNextReindexRepoTagsWork_SingleRepo(t *testing.T) {
	sutDB, sqlDB := setupDB(t)

	for _, tc := range reindexWorkerTestCases {
		t.Run(tc.name, func(t *testing.T) {
			resetTables(t, sqlDB)
			populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{{OrgRepoName: "foo/bar", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/bar", Created: time.Now().Add(-1000 * time.Hour)}})
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
	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{{OrgRepoName: "foo/bar", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/bar", Created: time.Now().Add(-1000 * time.Hour)}})
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

	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{
		{OrgRepoName: "foo/bar", Version: "v0.0.1", ModulePath: "github.somecompany.net/foo/bar", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "gaz/urk", Version: "v0.0.1", ModulePath: "github.somecompany.net/gaz/urk", Created: time.Now().Add(-1000 * time.Hour)},
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

	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{
		{OrgRepoName: "foo/bar", Version: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "bee/doh", Version: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
		{OrgRepoName: "gaz/urk", Version: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)},
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

func TestNextReindexRepoTags_Roundtrip(t *testing.T) {
	sutDB, sqlDB := setupDB(t)
	resetTables(t, sqlDB)
	populateRepoModuleVersions(t, sqlDB, []*db.RepoModuleVersion{{OrgRepoName: "foo/bar", Version: "v0.0.1", Created: time.Now().Add(-1000 * time.Hour)}})

	// First, get some work.
	gotRepoToReindex, gotWork, err := sutDB.NextReindexRepoTagsWork(t.Context(), time.Hour, time.Hour) // Re-index TTL & period are unused here.
	if err != nil {
		t.Fatal(err)
	}
	if !gotWork {
		t.Fatalf("NextReindexRepoTagsWork: expected work but got none")
	}
	if gotRepoToReindex != "foo/bar" {
		t.Errorf("NextReindexRepoTagsWork: expected foo/bar but got %s", gotRepoToReindex)
	}

	// Re-index and store the result.
	newTags := []*db.RepoModuleVersion{{OrgRepoName: "foo/bar", Version: "v0.0.1", Created: time.Now().Add(time.Minute)}}
	if err := sutDB.StoreRepoModuleVersions(t.Context(), "foo/bar", newTags); err != nil {
		t.Fatal(err)
	}

	// We should not be able to get work, since we just finished (StoreRepoModuleVersions)
	// work within the last 1h.
	_, gotWork, err = sutDB.NextReindexRepoTagsWork(t.Context(), time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if gotWork {
		t.Fatalf("NextReindexRepoTagsWork: expected no work but got some")
	}

	// We should be able to get work again once we're past our (artificially
	// low) reindex period.
	// Note: We're only operating at the second granularity, so let's sleep 1s
	// first.
	time.Sleep(time.Second)
	_, gotWork, err = sutDB.NextReindexRepoTagsWork(t.Context(), time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !gotWork {
		t.Fatalf("NextReindexRepoTagsWork: expected work but got none")
	}
}
