package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"

	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type handles struct {
	sut   *db.DB
	sqlDB *sql.DB
}

// The tests run one at a time and each calls resetTables, so one pair of pools
// serves them all; a pair per test runs the server out of connections, since
// neither handle is closed before the process exits.
var openDB = sync.OnceValues(func() (handles, error) {
	var missing []string
	env := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			missing = append(missing, key)
		}
		return v
	}
	username, password, host := env("POSTGRES_USERNAME"), env("POSTGRES_PASSWORD"), env("POSTGRES_HOST")
	portStr, dbname := env("POSTGRES_PORT"), env("POSTGRES_DB")
	if len(missing) > 0 {
		return handles{}, fmt.Errorf("%s not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB", strings.Join(missing, ", "))
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return handles{}, fmt.Errorf("POSTGRES_PORT is invalid: %v", err)
	}

	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", username, password, host, port, dbname)
	sqlDB, err := sql.Open("postgres", connStr)
	if err != nil {
		return handles{}, fmt.Errorf("error opening db %s: %v", connStr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return handles{}, fmt.Errorf("error pinging db %s: %v", connStr, err)
	}

	sut, err := db.NewDB(username, password, host, uint16(port), dbname)
	if err != nil {
		return handles{}, fmt.Errorf("error creating new DB: %v", err)
	}
	return handles{sut, sqlDB}, nil
})

func setupDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()

	h, err := openDB()
	if err != nil {
		t.Fatalf("setupDB: %v", err)
	}
	return h.sut, h.sqlDB
}

// Drops tables and re-runs migrations.
func resetTables(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repo_module_versions;"); err != nil {
		t.Fatalf("resetTables: error dropping repo_module_versions table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repos;"); err != nil {
		t.Fatalf("resetTables: error dropping repos table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS owners;"); err != nil {
		t.Fatalf("resetTables: error dropping owners table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS owner_indexing;"); err != nil {
		t.Fatalf("resetTables: error dropping owner_indexing table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repo_indexing;"); err != nil {
		t.Fatalf("resetTables: error dropping repo_indexing table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS schema_migrations;"); err != nil {
		t.Fatalf("resetTables: error dropping schema_migrations table: %v", err)
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		t.Fatalf("resetTables: error creating postgres driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://../../migrations", "postgres", driver)
	if err != nil {
		t.Fatalf("resetTables: error creating database migrator: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("resetTables: error running migrations: %v", err)
	}
}

// Returns a map of orgRepoName to RepoModuleVersion. Includes repos which have no tags.
func repoModuleVersions(t *testing.T, sdb *sql.DB) map[string][]*db.RepoModuleVersion {
	t.Helper()

	query := `
SELECT org_repo_name
FROM repos`
	rows, err := sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("repoModuleVersions: error fetching repos:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	repoModuleVersions := make(map[string][]*db.RepoModuleVersion)
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("repoModuleVersions: %v", err)
		}
		repoModuleVersions[r] = nil
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("repoModuleVersions: %v", err)
	}

	query = `
SELECT org_repo_name, version, module_path, created
FROM repo_module_versions
ORDER BY created DESC`
	rows, err = sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("repoModuleVersions: error fetching repo tags:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rt db.RepoModuleVersion
		if err := rows.Scan(&rt.OrgRepoName, &rt.Version, &rt.ModulePath, &rt.Created); err != nil {
			t.Fatalf("repoModuleVersions: %v", err)
		}
		repoModuleVersions[rt.OrgRepoName] = append(repoModuleVersions[rt.OrgRepoName], &rt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("repoModuleVersions: %v", err)
	}

	return repoModuleVersions
}

func populateRepoModuleVersions(t *testing.T, db *sql.DB, repoModuleVersions []*db.RepoModuleVersion) {
	t.Helper()

	for _, rt := range repoModuleVersions {
		query := fmt.Sprintf(`
INSERT INTO repos (org_repo_name)
VALUES ('%s')
ON CONFLICT (org_repo_name) DO NOTHING;`, rt.OrgRepoName)
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("populateRepoModuleVersions: error inserting into repos table:\nquery: %s\nerror: %v", query, err)
		}

		query = fmt.Sprintf(`
INSERT INTO repo_module_versions (org_repo_name, version, module_path, created, indexed_at)
VALUES ('%s', '%s', '%s', TIMESTAMP WITH TIME ZONE '%s', TIMESTAMP WITH TIME ZONE '%s')
ON CONFLICT (org_repo_name, module_path, version) DO UPDATE
SET created = EXCLUDED.created, indexed_at = EXCLUDED.indexed_at;`,
			rt.OrgRepoName, rt.Version, rt.ModulePath, rt.Created.Format(time.RFC3339), rt.IndexedAt.Format(time.RFC3339))
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("populateRepoModuleVersions: error inserting into repo_module_versions table:\nquery: %s\nerror:%v", query, err)
		}
	}
}

func setAllOwnersIndexing(t *testing.T, db *sql.DB, indexingBegan, indexingFinished time.Time) {
	t.Helper()

	query := fmt.Sprintf(`
UPDATE owner_indexing
SET indexing_began = TIMESTAMP WITH TIME ZONE '%s', indexing_finished = TIMESTAMP WITH TIME ZONE '%s'`,
		indexingBegan.Format(time.RFC3339), indexingFinished.Format(time.RFC3339))

	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("setAllOwnersIndexing: error updating owner_indexing table:\nquery: %s\nerror: %v", query, err)
	}
}

// Upserts an owner row with the given indexing timestamps. Unlike
// setSingleRepoIndexing, this inserts the row if it does not already exist.
func setSingleOwnerIndexing(t *testing.T, db *sql.DB, ownerLogin string, indexingBegan, indexingFinished time.Time) {
	t.Helper()

	query := fmt.Sprintf(`
INSERT INTO owners (owner_login, indexing_began, indexing_finished)
VALUES ('%s', TIMESTAMP WITH TIME ZONE '%s', TIMESTAMP WITH TIME ZONE '%s')
ON CONFLICT (owner_login) DO UPDATE
SET indexing_began = EXCLUDED.indexing_began, indexing_finished = EXCLUDED.indexing_finished;`,
		ownerLogin, indexingBegan.Format(time.RFC3339), indexingFinished.Format(time.RFC3339))

	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("setSingleOwnerIndexing: error updating owners table:\nquery: %s\nerror: %v", query, err)
	}
}

func owners(t *testing.T, sdb *sql.DB) []string {
	t.Helper()

	query := `
SELECT owner_login
FROM owners
ORDER BY owner_login`
	rows, err := sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("owners: error fetching owners:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	var logins []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			t.Fatalf("owners: %v", err)
		}
		logins = append(logins, login)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("owners: %v", err)
	}
	return logins
}

func setSingleRepoIndexing(t *testing.T, db *sql.DB, orgRepoName string, indexingBegan, indexingFinished time.Time) {
	t.Helper()

	query := fmt.Sprintf(`
UPDATE repos
SET indexing_began = TIMESTAMP WITH TIME ZONE '%s', indexing_finished = TIMESTAMP WITH TIME ZONE '%s'
WHERE org_repo_name = '%s'`,
		indexingBegan.Format(time.RFC3339), indexingFinished.Format(time.RFC3339), orgRepoName)

	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("setSingleRepoIndexing: error updating repos table:\nquery: %s\nerror: %v", query, err)
	}
}
