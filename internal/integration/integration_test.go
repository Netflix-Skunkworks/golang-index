package integration

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/github"
	"github.com/Netflix-Skunkworks/golang-index/internal/githubfake"
	"github.com/Netflix-Skunkworks/golang-index/internal/indexer"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/google/go-cmp/cmp"

	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

// TestIndexing runs every fixture in internal/testcases as a subtest, indexing the
// repos it describes twice and comparing the stored module versions with its want
// files. See ../testcases/README.md for the fixture format.
func TestIndexing(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(testcasesDir, "*.txtar"))
	if err != nil {
		t.Fatalf("globbing fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures found in %s", testcasesDir)
	}

	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".txtar"), func(t *testing.T) {
			f := loadFixture(t, path)
			h := newHarness(t, f.repoList()...)

			h.indexAll(t)
			first := h.stored(t, time.Time{})
			assertRows(t, "first index cycle", f.want, first)

			f.applyUpdate(t)
			h.makeReindexDue(t)
			h.indexAll(t)

			second := h.stored(t, time.Time{})
			assertRows(t, "second index cycle", f.wantAfterUpdate, second)
			h.assertFeed(t, first, second)
		})
	}
}

// harness wires the real db, GitHub SCM, and indexers against a real Postgres and
// a fake GitHub serving repos.
type harness struct {
	db       *db.DB
	sqlDB    *sql.DB
	allRepos *indexer.AllReposIndexer
	repoTags *indexer.RepoTagsIndexer
}

func newHarness(t *testing.T, repos ...*githubfake.Repo) *harness {
	t.Helper()

	sutDB, sqlDB := freshDB(t)

	srv := githubfake.NewServer(repos...)
	scm := github.NewEnterpriseSCM(githubfake.BaseURL, githubfake.Host, srv.Client())

	return &harness{
		db:    sutDB,
		sqlDB: sqlDB,
		allRepos: &indexer.AllReposIndexer{
			DB: sutDB, Lister: scm,
			WorkCheckPeriod: time.Minute, ReindexTTL: time.Minute, ReindexPeriod: time.Hour,
		},
		repoTags: &indexer.RepoTagsIndexer{
			DB: sutDB, SCM: scm, DefaultModuleHost: githubfake.Host,
			WorkCheckPeriod: time.Minute, ReindexTTL: time.Minute, ReindexPeriod: time.Hour,
		},
	}
}

// freshDB creates a freshly-migrated database on the Postgres server named by the
// POSTGRES_* env vars (the same server the db package's tests use) and returns
// handles to it. Each test gets its own database so the suite stays isolated from
// those tests, which reset the shared database, when both run under `go test ./...`.
func freshDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	pg := pgEnv(t)

	// Derive a valid, unquoted Postgres identifier from the test name: lowercase,
	// with every other character (spaces, dots, slashes from subtests) mapped to _.
	name := "gi_integ_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, strings.ToLower(t.Name()))
	dropDatabase(t, pg, name) // a stale copy, from a run that was interrupted
	admin, err := sql.Open("postgres", pg.dsn(pg.adminDB))
	if err != nil {
		t.Fatalf("freshDB: opening admin connection: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("freshDB: creating %q: %v", name, err)
	}
	if err := admin.Close(); err != nil {
		t.Fatalf("freshDB: closing admin connection: %v", err)
	}
	t.Cleanup(func() { dropDatabase(t, pg, name) })

	sqlDB, err := sql.Open("postgres", pg.dsn(name))
	if err != nil {
		t.Fatalf("freshDB: opening %q: %v", name, err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("freshDB: closing %q: %v", name, err)
		}
	})

	migrateUp(t, sqlDB)

	sutDB, err := db.NewDB(pg.user, pg.pass, pg.host, pg.port, name)
	if err != nil {
		t.Fatalf("freshDB: db.NewDB: %v", err)
	}
	return sutDB, sqlDB
}

// pgConfig is a Postgres server to create test databases on.
type pgConfig struct {
	user, pass, host string
	port             uint16
	// adminDB is an existing database to connect to in order to create and drop
	// the others.
	adminDB string
}

func (pg pgConfig) dsn(dbName string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", pg.user, pg.pass, pg.host, pg.port, dbName)
}

// pgEnv reads the Postgres connection settings, which are the same POSTGRES_* env
// vars the db package's tests require.
func pgEnv(t *testing.T) pgConfig {
	t.Helper()
	get := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			t.Fatalf("%s is not set (POSTGRES_USERNAME/PASSWORD/HOST/PORT/DB are required)", key)
		}
		return v
	}
	port, err := strconv.ParseUint(get("POSTGRES_PORT"), 10, 16)
	if err != nil {
		t.Fatalf("POSTGRES_PORT is invalid: %v", err)
	}
	return pgConfig{
		user:    get("POSTGRES_USERNAME"),
		pass:    get("POSTGRES_PASSWORD"),
		host:    get("POSTGRES_HOST"),
		port:    uint16(port),
		adminDB: get("POSTGRES_DB"),
	}
}

// dropDatabase removes a database created by [freshDB]. WITH (FORCE) closes the
// lingering pool [db.NewDB] left open, which has no Close method.
func dropDatabase(t *testing.T, pg pgConfig, name string) {
	t.Helper()
	admin, err := sql.Open("postgres", pg.dsn(pg.adminDB))
	if err != nil {
		t.Errorf("dropDatabase: opening admin connection: %v", err)
		return
	}
	defer func() {
		if err := admin.Close(); err != nil {
			t.Errorf("dropDatabase: closing admin connection: %v", err)
		}
	}()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
		t.Errorf("dropDatabase: dropping %q: %v", name, err)
	}
}

// migrateUp applies the repo's migrations to sqlDB.
func migrateUp(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	driver, err := postgres.WithInstance(sqlDB, &postgres.Config{})
	if err != nil {
		t.Fatalf("migrateUp: driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://../../migrations", "postgres", driver)
	if err != nil {
		t.Fatalf("migrateUp: migrator: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("migrateUp: %v", err)
	}
}

// makeReindexDue rewinds every work-queue timestamp into the past so the next
// indexing cycle re-indexes each repo exactly once (rather than the default,
// where a just-finished repo isn't due again for ReindexPeriod).
func (h *harness) makeReindexDue(t *testing.T) {
	t.Helper()

	const rewind = "SET indexing_began = NOW() - INTERVAL '48 hours', indexing_finished = NOW() - INTERVAL '48 hours'"
	for _, table := range []string{"repos", "repo_indexing"} {
		if _, err := h.sqlDB.ExecContext(t.Context(), "UPDATE "+table+" "+rewind); err != nil {
			t.Fatalf("makeReindexDue %s: %v", table, err)
		}
	}
}

// indexAll runs one full indexing cycle: refresh the repo list, then drain the
// repo-tags work queue.
func (h *harness) indexAll(t *testing.T) {
	t.Helper()

	if _, err := h.allRepos.IndexAllReposOnce(t.Context()); err != nil {
		t.Fatalf("IndexAllReposOnce: %v", err)
	}
	for {
		gotWork, _, err := h.repoTags.IndexNextRepoOnce(t.Context())
		if err != nil {
			t.Fatalf("IndexNextRepoOnce: %v", err)
		}
		if !gotWork {
			break
		}
	}
}

// stored reads what the /index feed reports from since onwards.
func (h *harness) stored(t *testing.T, since time.Time) []*db.RepoModuleVersion {
	t.Helper()

	// Higher than any fixture's row count, so the whole feed comes back.
	const limit = 10000
	got, err := h.db.FetchRepoModuleVersions(t.Context(), since, limit)
	if err != nil {
		t.Fatalf("FetchRepoModuleVersions: %v", err)
	}
	return got
}

// assertRows fails if the stored module versions do not match want.
func assertRows(t *testing.T, when string, want []row, stored []*db.RepoModuleVersion) {
	t.Helper()

	var got []row
	for _, v := range stored {
		got = append(got, rowOf(v))
	}
	sortRows(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("stored rows after the %s (-want +got):\n%s", when, diff)
	}
}

// assertFeed checks that a re-index left the /index feed an append-only log: rows
// that survived keep the indexed_at they already had, rows that are new get a
// later one, and a consumer polling from the pre-re-index cursor sees exactly the
// new rows.
func (h *harness) assertFeed(t *testing.T, before, after []*db.RepoModuleVersion) {
	t.Helper()

	// The cursor stands in for how far a feed consumer had read before the
	// re-index: the latest indexed_at it could have seen.
	was := make(map[string]time.Time, len(before))
	var cursor time.Time
	for _, v := range before {
		was[rowOf(v).key()] = v.IndexedAt
		if v.IndexedAt.After(cursor) {
			cursor = v.IndexedAt
		}
	}

	var wantNew []string
	for _, v := range after {
		key := rowOf(v).key()
		previously, existed := was[key]
		if !existed {
			if !v.IndexedAt.After(cursor) {
				t.Errorf("%s: new row's indexed_at %v is not after the previous latest %v", key, v.IndexedAt, cursor)
			}
			wantNew = append(wantNew, key)
			continue
		}
		if !v.IndexedAt.Equal(previously) {
			t.Errorf("%s: indexed_at changed across the re-index: was %v, now %v", key, previously, v.IndexedAt)
		}
	}

	// The feed query is inclusive of since, so poll from just past the cursor.
	var gotNew []string
	for _, v := range h.stored(t, cursor.Add(time.Microsecond)) {
		gotNew = append(gotNew, rowOf(v).key())
	}
	slices.Sort(wantNew)
	slices.Sort(gotNew)
	if diff := cmp.Diff(wantNew, gotNew); diff != "" {
		t.Errorf("feed since the pre-re-index cursor (-want +got):\n%s", diff)
	}
}
