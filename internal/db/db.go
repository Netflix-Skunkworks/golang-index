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

// RepoModuleVersion is one version of one module in a repo: a module path at a
// version, where the version is a git tag or a synthesized pseudo-version.
type RepoModuleVersion struct {
	OrgRepoName string
	Version     string
	ModulePath  string
	// Created is when the version was created: the git tag's date, or the commit
	// date for a pseudo-version.
	Created time.Time
	// IndexedAt is when this index first observed the version. The /index feed is
	// keyed on this so it behaves like an append-only log. It is assigned by the
	// DB on insert (column DEFAULT); StoreRepoModuleVersions ignores any value set here.
	IndexedAt time.Time
}

// Fetches the module versions the /index feed publishes, from since onwards. A
// module version is published once, at the earliest indexed_at any repo was seen
// to claim it, even where several repos claim it.
func (d *DB) FetchRepoModuleVersions(ctx context.Context, since time.Time, limit int64) ([]*RepoModuleVersion, error) {
	// Filtered and ordered by indexed_at so consumers can poll forward with
	// ?since=; see RepoModuleVersion.IndexedAt.
	//
	// Several repos can declare the same module path — an un-renamed fork, a
	// vendored copy — and each keeps its own row, but the feed carries no
	// org_repo_name to tell those rows apart, so repeating a module version would
	// both stop the feed mirroring index.golang.org and break a consumer that
	// upserts a whole page in one statement. org_repo_name breaks ties in
	// indexed_at, so exactly one row survives.
	query := `
SELECT org_repo_name, version, module_path, created, indexed_at
FROM repo_module_versions rmv
WHERE indexed_at >= $1
AND NOT EXISTS (
    SELECT 1
    FROM repo_module_versions earlier
    WHERE earlier.module_path = rmv.module_path
    AND earlier.version = rmv.version
    AND (earlier.indexed_at, earlier.org_repo_name) < (rmv.indexed_at, rmv.org_repo_name)
)
ORDER BY indexed_at ASC
LIMIT $2;`

	rows, err := d.db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, fmt.Errorf("FetchRepoModuleVersions:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	var repoModuleVersions []*RepoModuleVersion
	for rows.Next() {
		var rt RepoModuleVersion
		if err := rows.Scan(&rt.OrgRepoName, &rt.Version, &rt.ModulePath, &rt.Created, &rt.IndexedAt); err != nil {
			return nil, fmt.Errorf("FetchRepoModuleVersions: %v", err)
		}
		repoModuleVersions = append(repoModuleVersions, &rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FetchRepoModuleVersions: %v", err)
	}

	return repoModuleVersions, nil
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
// Completes the all-repos work item in the same transaction, which is what holds
// the next pass off until the re-index period has passed.
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

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("StoreRepos: %v", err)
	}
	// Defer a rollback in case anything fails.
	defer tx.Rollback()

	query := fmt.Sprintf(`
INSERT INTO repos (org_repo_name)
VALUES %s
ON CONFLICT (org_repo_name) DO NOTHING;`, strings.Join(valueStrings, ",\n\t"))
	if _, err := tx.ExecContext(ctx, query, valueArgs...); err != nil {
		return fmt.Errorf("StoreRepos:\nquery: %s\nerror: %v", query, err)
	}

	// repo_indexing holds a single row, so this needs no WHERE; see the table's
	// definition in the initial migration.
	query = `
UPDATE repo_indexing
SET indexing_finished = NOW();`
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("StoreRepos:\nquery: %s\nerror: %v", query, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("StoreRepos: %v", err)
	}

	return nil
}

// Store orgRepoName's module versions, and complete its work item in the same
// transaction, which is what holds the repo's next re-index off until the
// re-index period has passed.
//
// An empty repoModuleVersions is a repo with no module versions to index, not an
// error: its stored rows are all stale, so they all go.
//
// WARNING: Timezones aren't retained. Always pass UTC timezones.
//
// WARNING: The given module versions are treated as authoratative: any of the
// repo's stored rows not in the given list will be deleted. This function SHOULD
// NOT be provided partial updates.
func (d *DB) StoreRepoModuleVersions(ctx context.Context, orgRepoName string, repoModuleVersions []*RepoModuleVersion) error {
	// The incoming rows, zipped into parallel arrays so both statements below can
	// take them as three parameters rather than three per row.
	modulePaths := make([]string, len(repoModuleVersions))
	versions := make([]string, len(repoModuleVersions))
	createds := make([]string, len(repoModuleVersions))
	for i, rt := range repoModuleVersions {
		modulePaths[i] = rt.ModulePath
		versions[i] = rt.Version
		createds[i] = rt.Created.Format(time.RFC3339)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("StoreRepoModuleVersions: %v", err)
	}
	// Defer a rollback in case anything fails.
	defer tx.Rollback()

	// Delete only STALE rows: those stored for this repo but absent from the
	// authoritative incoming set. Surviving rows are kept so their first-seen
	// indexed_at persists across re-indexing; new rows get a fresh indexed_at
	// from the column DEFAULT below.
	query := `
DELETE FROM repo_module_versions rt
WHERE rt.org_repo_name = $1
AND NOT EXISTS (
    SELECT 1
    FROM unnest($2::text[], $3::text[]) AS keep(module_path, version)
    WHERE keep.module_path = rt.module_path
    AND keep.version = rt.version
);`
	if _, err := tx.ExecContext(ctx, query, orgRepoName, pq.Array(modulePaths), pq.Array(versions)); err != nil {
		return fmt.Errorf("StoreRepoModuleVersions:\nquery: %s\nerror: %v", query, err)
	}

	// Empty arrays select no rows, so a repo with no module versions inserts nothing.
	query = `
INSERT INTO repo_module_versions (org_repo_name, module_path, version, created)
SELECT $1, * FROM unnest($2::text[], $3::text[], $4::timestamp[])
ON CONFLICT (org_repo_name, module_path, version) DO UPDATE
SET created = EXCLUDED.created;`
	if _, err := tx.ExecContext(ctx, query, orgRepoName, pq.Array(modulePaths), pq.Array(versions), pq.Array(createds)); err != nil {
		return fmt.Errorf("StoreRepoModuleVersions:\nquery: %s\nerror: %v", query, err)
	}

	query = `
UPDATE repos
SET indexing_finished = NOW()
WHERE org_repo_name = $1;`
	if _, err := tx.ExecContext(ctx, query, orgRepoName); err != nil {
		return fmt.Errorf("StoreRepoModuleVersions:\nquery: %s\nerror: %v", query, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("StoreRepoModuleVersions: %v", err)
	}

	return nil
}
