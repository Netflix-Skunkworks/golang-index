package db_test

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	"github.com/Netflix-Skunkworks/golang-index/internal/pgtest"
)

// TestMain runs one embedded Postgres for the whole package; each test gets its
// own freshly-migrated database via [pgtest.FreshDB].
func TestMain(m *testing.M) { os.Exit(pgtest.RunMain(m)) }

func setupDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	return pgtest.FreshDB(t)
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
SELECT org_repo_name, tag_name, module_path, created
FROM repo_tags
ORDER BY created DESC`
	rows, err = sdb.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("repoTags: error fetching repo tags:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rt db.RepoTag
		if err := rows.Scan(&rt.OrgRepoName, &rt.TagName, &rt.ModulePath, &rt.Created); err != nil {
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
INSERT INTO repo_tags (org_repo_name, tag_name, module_path, created, indexed_at)
VALUES ('%s', '%s', '%s', TIMESTAMP WITH TIME ZONE '%s', TIMESTAMP WITH TIME ZONE '%s')
ON CONFLICT (org_repo_name, module_path, tag_name) DO UPDATE
SET created = EXCLUDED.created, indexed_at = EXCLUDED.indexed_at;`,
			rt.OrgRepoName, rt.TagName, rt.ModulePath, rt.Created.Format(time.RFC3339), rt.IndexedAt.Format(time.RFC3339))
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
