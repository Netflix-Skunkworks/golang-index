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
			if f.skip != "" {
				t.Skip(f.skip)
			}
			h := newHarness(t, f.repoList()...)

			h.indexAll(t)
			if diff := cmp.Diff(f.want, rowsOf(h.stored(t))); diff != "" {
				t.Errorf("stored rows after the first index cycle (-want +got):\n%s", diff)
			}
			firstFeed := h.feed(t, time.Time{})
			for _, r := range repeats(firstFeed) {
				t.Errorf("the first index cycle's feed carries %q twice, for %q and %q", r.moduleVersion, r.first, r.second)
			}

			f.applyUpdate(t)
			h.makeReindexDue(t)
			h.indexAll(t)

			if diff := cmp.Diff(f.wantAfterUpdate, rowsOf(h.stored(t))); diff != "" {
				t.Errorf("stored rows after the second index cycle (-want +got):\n%s", diff)
			}
			secondFeed := h.feed(t, time.Time{})
			for _, r := range repeats(secondFeed) {
				t.Errorf("the second index cycle's feed carries %q twice, for %q and %q", r.moduleVersion, r.first, r.second)
			}

			// The re-index left the feed an append-only log: a module version it
			// already carried keeps its indexed_at, and a new one gets a later one
			// than the cursor a consumer reading the first cycle's feed would hold.
			was := indexedAtByModuleVersion(firstFeed)
			cursor := latestIndexedAt(firstFeed)
			var wantNew []string
			for _, v := range secondFeed {
				moduleVersion := rowOf(v).moduleVersion()
				previously, existed := was[moduleVersion]
				if !existed {
					if !v.IndexedAt.After(cursor) {
						t.Errorf("%s: new row's indexed_at %v is not after the previous latest %v", moduleVersion, v.IndexedAt, cursor)
					}
					wantNew = append(wantNew, moduleVersion)
					continue
				}
				if !v.IndexedAt.Equal(previously) {
					t.Errorf("%s: indexed_at changed across the re-index: was %v, now %v", moduleVersion, previously, v.IndexedAt)
				}
			}

			// The feed query is inclusive of since, so poll from just past the cursor.
			gotNew := moduleVersionsOf(h.feed(t, cursor.Add(time.Microsecond)))
			slices.Sort(wantNew)
			slices.Sort(gotNew)
			if diff := cmp.Diff(wantNew, gotNew); diff != "" {
				t.Errorf("feed since the pre-re-index cursor (-want +got):\n%s", diff)
			}
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

// stored reads every row of repo_module_versions, which is what a fixture's want
// files describe. The /index feed is narrower; see [harness.feed].
func (h *harness) stored(t *testing.T) []*db.RepoModuleVersion {
	t.Helper()

	const query = "SELECT org_repo_name, version, module_path, created, indexed_at FROM repo_module_versions"
	rows, err := h.sqlDB.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("stored:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()

	var stored []*db.RepoModuleVersion
	for rows.Next() {
		var v db.RepoModuleVersion
		if err := rows.Scan(&v.OrgRepoName, &v.Version, &v.ModulePath, &v.Created, &v.IndexedAt); err != nil {
			t.Fatalf("stored: scanning a row: %v", err)
		}
		stored = append(stored, &v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("stored: iterating rows: %v", err)
	}
	return stored
}

// feed reads what the /index feed publishes from since onwards.
func (h *harness) feed(t *testing.T, since time.Time) []*db.RepoModuleVersion {
	t.Helper()

	// Higher than any fixture's row count, so the whole feed comes back.
	const limit = 10000
	got, err := h.db.FetchRepoModuleVersions(t.Context(), since, limit)
	if err != nil {
		t.Fatalf("FetchRepoModuleVersions: %v", err)
	}
	return got
}

// moduleVersionsOf lists the module versions a feed carried.
func moduleVersionsOf(feed []*db.RepoModuleVersion) []string {
	var moduleVersions []string
	for _, v := range feed {
		moduleVersions = append(moduleVersions, rowOf(v).moduleVersion())
	}
	return moduleVersions
}

// indexedAtByModuleVersion maps each module version a feed carried to its
// indexed_at.
func indexedAtByModuleVersion(feed []*db.RepoModuleVersion) map[string]time.Time {
	indexedAt := make(map[string]time.Time, len(feed))
	for _, v := range feed {
		indexedAt[rowOf(v).moduleVersion()] = v.IndexedAt
	}
	return indexedAt
}

// latestIndexedAt is the newest indexed_at in a feed, which is how far a consumer
// that read all of it would have advanced its cursor.
func latestIndexedAt(feed []*db.RepoModuleVersion) time.Time {
	var latest time.Time
	for _, v := range feed {
		if v.IndexedAt.After(latest) {
			latest = v.IndexedAt
		}
	}
	return latest
}

// repeat is a module version a feed carried twice, and the two repos it was carried
// for.
type repeat struct {
	moduleVersion string
	first, second string
}

// repeats lists the module versions a feed carried more than once.
func repeats(feed []*db.RepoModuleVersion) []repeat {
	carriedFor := make(map[string]string, len(feed))
	var repeated []repeat
	for _, v := range feed {
		moduleVersion := rowOf(v).moduleVersion()
		if first, ok := carriedFor[moduleVersion]; ok {
			repeated = append(repeated, repeat{moduleVersion, first, v.OrgRepoName})
			continue
		}
		carriedFor[moduleVersion] = v.OrgRepoName
	}
	return repeated
}
