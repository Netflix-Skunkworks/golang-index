// Package db implements special indexing logic for an Postgres database.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	// TODO(jbarkhuysen): Consider switching to pgx instead.
	"github.com/lib/pq" // Postgres driver; also provides pq.Array.
)

// A db handle with specialised logic for indexing.
type DB struct {
	db *sql.DB
}

// Establishes a new DB.
func NewDB(username, password, host string, port uint16, dbname string) (*DB, error) {
	connStr := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", username, password, host, port, dbname)
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal(err)
	}

	// Ordinarily we'd use the context propagated from main, but here we just
	// need a timeout mechanism and this is all we can use.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("error pinging db: %v", err)
	}

	return &DB{db: db}, nil
}

// A tag for a repo.
type RepoTag struct {
	OrgRepoName string
	TagName     string
	ModulePath  string
	// Created is the git tag's own creation date.
	Created time.Time
	// IndexedAt is when this index first observed the tag. The /index feed is
	// keyed on this so it behaves like an append-only log. It is assigned by the
	// DB on insert (column DEFAULT); StoreRepoTags ignores any value set here.
	IndexedAt time.Time
}

// Fetches repo tags.
func (d *DB) FetchRepoTags(ctx context.Context, since time.Time, limit int64) ([]*RepoTag, error) {
	// Filtered and ordered by indexed_at so consumers can poll forward with
	// ?since=; see RepoTag.IndexedAt.
	query := `
SELECT org_repo_name, tag_name, module_path, created, indexed_at
FROM repo_tags
WHERE indexed_at >= $1
ORDER BY indexed_at ASC
LIMIT $2;`

	rows, err := d.db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, fmt.Errorf("FetchRepoTags:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	var repoTags []*RepoTag
	for rows.Next() {
		var rt RepoTag
		if err := rows.Scan(&rt.OrgRepoName, &rt.TagName, &rt.ModulePath, &rt.Created, &rt.IndexedAt); err != nil {
			return nil, fmt.Errorf("FetchRepoTags: %v", err)
		}
		repoTags = append(repoTags, &rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FetchRepoTags: %v", err)
	}

	return repoTags, nil
}

// Retrieves from the work queue whether it's time to re-index all repos.
func (d *DB) NextReindexAllReposWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (shouldReindex bool, _ error) {
	query := `
UPDATE repo_indexing
SET indexing_began = NOW()
WHERE indexing_began + ($1 * INTERVAL '1 SECOND') < NOW()
AND indexing_finished + ($2 * INTERVAL '1 SECOND') < NOW();`
	id, err := d.db.ExecContext(ctx, query, int64(reindexTTL.Seconds()), int64(reindexPeriod.Seconds()))
	if err != nil {
		return false, fmt.Errorf("NextReindexAllReposWork:\nquery: %s\nerror: %v", query, err)
	}
	a, err := id.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("NextReindexAllReposWork: %v", err)
	}
	return a > 0, nil
}

// Retrieves from the work queue the next repo for which to re-index tags.
// workWasFound will be false if no work was found.
func (d *DB) NextReindexRepoTagsWork(ctx context.Context, reindexTTL, reindexPeriod time.Duration) (repoToReindex string, workWasFound bool, _ error) {
	query := fmt.Sprintf(`
UPDATE repos
SET indexing_began = NOW()
WHERE org_repo_name = (
    SELECT org_repo_name
    FROM repos
    WHERE indexing_began + (%d * INTERVAL '1 SECOND') < NOW()
    AND indexing_finished + (%d * INTERVAL '1 SECOND') < NOW()
    ORDER BY indexing_finished ASC
    LIMIT 1
)
RETURNING org_repo_name;`, int64(reindexTTL.Seconds()), int64(reindexPeriod.Seconds()))

	row := d.db.QueryRowContext(ctx, query)
	if row.Err() != nil {
		return "", false, fmt.Errorf("NextReindexRepoTagsWork:\nquery: %s\nerror: %v", query, row.Err())
	}
	var r string
	if err := row.Scan(&r); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("NextReindexRepoTagsWork: %v", err)
	}
	return r, true, nil
}

// Store the given repos. Afterwards, they will be ready for repo tag indexing.
//
// TODO(jbarkhuysen): The given orgRepoNames should be treated as authoratative.
// Any repos in GitHub not in this list should be deleted (and their repo tags).
func (d *DB) StoreRepos(ctx context.Context, orgRepoNames []string) error {
	if len(orgRepoNames) == 0 {
		return fmt.Errorf("StoreRepos called with 0 repos")
	}

	var valueStrings []string
	var valueArgs []any
	for i, orn := range orgRepoNames {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d)", i+1))
		valueArgs = append(valueArgs, orn)
	}

	query := fmt.Sprintf(`
INSERT INTO repos (org_repo_name)
VALUES %s
ON CONFLICT (org_repo_name) DO NOTHING;`, strings.Join(valueStrings, ",\n\t"))

	if _, err := d.db.ExecContext(ctx, query, valueArgs...); err != nil {
		return fmt.Errorf("StoreRepos:\nquery: %s\nerror: %v", query, err)
	}

	return nil
}

// Store the given repo tags. It's permissable to give this function repo tags
// for different repos.
//
// WARNING: Timezones aren't retained. Always pass UTC timezones.
//
// WARNING: The given repo tags are treated as authoratative: for each repo that
// tags are given, any stored tags not in the given list will be deleted. This
// function SHOULD NOT be provided partial updates.
func (d *DB) StoreRepoTags(ctx context.Context, repoTags []*RepoTag) error {
	if len(repoTags) == 0 {
		return fmt.Errorf("StoreRepoTags called with 0 repo tags")
	}

	// Number of fields in the INSERT below, used to number its placeholders.
	const fieldCount = 4

	var valueStrings []string
	var valueArgs []any
	orgRepoNames := make(map[string]bool)
	// keepRepos/keepModulePaths/keepTags zip the authoritative incoming
	// (org_repo_name, module_path, tag_name) rows into parallel arrays for the
	// delete-stale anti-join below.
	keepRepos := make([]string, len(repoTags))
	keepModulePaths := make([]string, len(repoTags))
	keepTags := make([]string, len(repoTags))
	for i, rt := range repoTags {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", fieldCount*i+1, fieldCount*i+2, fieldCount*i+3, fieldCount*i+4))
		valueArgs = append(valueArgs, rt.OrgRepoName)
		valueArgs = append(valueArgs, rt.TagName)
		valueArgs = append(valueArgs, rt.ModulePath)
		valueArgs = append(valueArgs, rt.Created.Format(time.RFC3339))
		orgRepoNames[rt.OrgRepoName] = true
		keepRepos[i] = rt.OrgRepoName
		keepModulePaths[i] = rt.ModulePath
		keepTags[i] = rt.TagName
	}
	repoList := make([]string, 0, len(orgRepoNames))
	for orgRepoName := range orgRepoNames {
		repoList = append(repoList, orgRepoName)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("StoreRepoTags: %v", err)
	}
	// Defer a rollback in case anything fails.
	defer tx.Rollback()

	// Delete only STALE tags: those stored for these repos but absent from the
	// authoritative incoming set. Surviving rows are kept so their first-seen
	// indexed_at persists across re-indexing; new tags get a fresh indexed_at
	// from the column DEFAULT below.
	query := `
DELETE FROM repo_tags rt
WHERE rt.org_repo_name = ANY($1)
AND NOT EXISTS (
    SELECT 1
    FROM unnest($2::text[], $3::text[], $4::text[]) AS keep(org_repo_name, module_path, tag_name)
    WHERE keep.org_repo_name = rt.org_repo_name
    AND keep.module_path = rt.module_path
    AND keep.tag_name = rt.tag_name
);`
	if _, err := tx.ExecContext(ctx, query, pq.Array(repoList), pq.Array(keepRepos), pq.Array(keepModulePaths), pq.Array(keepTags)); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	query = fmt.Sprintf(`
INSERT INTO repo_tags (org_repo_name, tag_name, module_path, created)
VALUES %s
ON CONFLICT (org_repo_name, module_path, tag_name) DO UPDATE
SET created = EXCLUDED.created;`, strings.Join(valueStrings, ",\n"))
	if _, err := tx.ExecContext(ctx, query, valueArgs...); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	query = `
UPDATE repos
SET indexing_finished = NOW()
WHERE org_repo_name = ANY($1);`
	if _, err := tx.ExecContext(ctx, query, pq.Array(repoList)); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("StoreRepoTags: %v", err)
	}

	return nil
}
