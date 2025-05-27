package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"

	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

func setupDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()

	username := os.Getenv("POSTGRES_USERNAME")
	if username == "" {
		t.Fatal("POSTGRES_USERNAME is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB.")
	}
	password := os.Getenv("POSTGRES_PASSWORD")
	if password == "" {
		t.Fatal("POSTGRES_PASSWORD is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB.")
	}
	host := os.Getenv("POSTGRES_HOST")
	if host == "" {
		t.Fatal("POSTGRES_HOST is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB.")
	}
	portStr := os.Getenv("POSTGRES_PORT")
	if portStr == "" {
		t.Fatal("POSTGRES_PORT is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB.")
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		t.Fatalf("POSTGRES_PORT is invalid: %v", err)
	}
	dbname := os.Getenv("POSTGRES_DB")
	if dbname == "" {
		t.Fatal("POSTGRES_DB is not set. Must set POSTGRES_USERNAME, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, and POSTGRES_DB.")
	}

	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", username, password, host, port, dbname)
	sqlDB, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("setupDB: error opening db %s: %v", connStr, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		t.Fatalf("setupDB: error pinging db %s: %v", connStr, err)
	}

	sutDB, err := db.NewDB(username, password, host, uint16(port), dbname)
	if err != nil {
		t.Fatalf("setupDB: error creating new DB: %v", err)
	}

	return sutDB, sqlDB
}

// Drops tables and re-runs migrations.
func resetTables(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repo_tags;"); err != nil {
		t.Fatalf("resetTables: error dropping repo_tags table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repos;"); err != nil {
		t.Fatalf("resetTables: error dropping repos table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS repo_indexing;"); err != nil {
		t.Fatalf("resetTables: error dropping repo_indexing table: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TABLE IF EXISTS schema_migrations;"); err != nil {
		t.Fatalf("resetTables: error dropping repo_indexing table: %v", err)
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

// Returns a map of orgRepoName to RepoTag. Includes repos which have no tags.
func repoTags(t *testing.T, sdb *sql.DB) map[string][]*db.RepoTag {
	t.Helper()

	query := `
SELECT org_repo_name
FROM repos`
	rows, err := sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("repoTags: error fetching repos:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	repoTags := make(map[string][]*db.RepoTag)
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("repoTags: %v", err)
		}
		repoTags[r] = nil
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("repoTags: %v", err)
	}

	query = `
SELECT org_repo_name, tag_name, created
FROM repo_tags
ORDER BY created DESC`
	rows, err = sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("repoTags: error fetching repo tags:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rt db.RepoTag
		if err := rows.Scan(&rt.OrgRepoName, &rt.TagName, &rt.Created); err != nil {
			t.Fatalf("repoTags: %v", err)
		}
		repoTags[rt.OrgRepoName] = append(repoTags[rt.OrgRepoName], &rt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("repoTags: %v", err)
	}

	return repoTags
}

func populateRepoTags(t *testing.T, db *sql.DB, repoTags []*db.RepoTag) {
	t.Helper()

	for _, rt := range repoTags {
		query := fmt.Sprintf(`
INSERT INTO repos (org_repo_name)
VALUES ('%s')
ON CONFLICT (org_repo_name) DO NOTHING;`, rt.OrgRepoName)
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("populateRepoTags: error inserting into repos table:\nquery: %s\nerror: %v", query, err)
		}

		query = fmt.Sprintf(`
INSERT INTO repo_tags (org_repo_name, tag_name, created)
VALUES ('%s', '%s', TIMESTAMP WITH TIME ZONE '%s')
ON CONFLICT (org_repo_name, tag_name) DO UPDATE
SET created = EXCLUDED.created;`, rt.OrgRepoName, rt.TagName, rt.Created.Format(time.RFC3339))
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("populateRepoTags: error inserting into repo_tags table:\nquery: %s\nerror:%v", query, err)
		}
	}
}

func setAllReposIndexing(t *testing.T, db *sql.DB, indexingBegan, indexingFinished time.Time) {
	t.Helper()

	query := fmt.Sprintf(`
UPDATE repo_indexing
SET indexing_began = TIMESTAMP WITH TIME ZONE '%s', indexing_finished = TIMESTAMP WITH TIME ZONE '%s'`,
		indexingBegan.Format(time.RFC3339), indexingFinished.Format(time.RFC3339))

	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("setAllReposIndexing: error updating repo_indexing table:\nquery: %s\nerror: %v", query, err)
	}
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
