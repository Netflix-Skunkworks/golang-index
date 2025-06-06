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
	_ "github.com/lib/pq" // Postgres driver.
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
	Created     time.Time
}

// Fetches repo tags.
func (d *DB) FetchRepoTags(ctx context.Context, since time.Time, limit int64) ([]*RepoTag, error) {
	query := `
SELECT org_repo_name, tag_name, module_path, created
FROM repo_tags
WHERE created >= $1
ORDER BY created DESC
LIMIT $2;`

	rows, err := d.db.QueryContext(ctx, query, since, limit)
	if err != nil {
		return nil, fmt.Errorf("FetchRepoTags:\nquery: %s\nerror: %v", query, err)
	}
	defer rows.Close()
	var repoTags []*RepoTag
	for rows.Next() {
		var rt RepoTag
		if err := rows.Scan(&rt.OrgRepoName, &rt.TagName, &rt.ModulePath, &rt.Created); err != nil {
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

	var valueStrings []string
	var valueArgs []any
	var conditionalStrings []string
	var conditionalArgs []any

	// number of fields in the SQL query used to correctly number query
	// placeholders
	const fieldCount = 4

	orgRepoNames := make(map[string]bool)
	for i, rt := range repoTags {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", fieldCount*i+1, fieldCount*i+2, fieldCount*i+3, fieldCount*i+4))
		valueArgs = append(valueArgs, rt.OrgRepoName)
		valueArgs = append(valueArgs, rt.TagName)
		valueArgs = append(valueArgs, rt.ModulePath)
		valueArgs = append(valueArgs, rt.Created.Format(time.RFC3339))
		orgRepoNames[rt.OrgRepoName] = true
	}
	i := 1
	for orgRepoName := range orgRepoNames {
		if len(conditionalStrings) == 0 {
			conditionalStrings = append(conditionalStrings, fmt.Sprintf("WHERE org_repo_name = $%d", i))
		} else {
			conditionalStrings = append(conditionalStrings, fmt.Sprintf("OR org_repo_name = $%d", i))
		}
		conditionalArgs = append(conditionalArgs, orgRepoName)
		i++
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("StoreRepoTags: %v", err)
	}
	// Defer a rollback in case anything fails.
	defer tx.Rollback()

	query := "DELETE FROM repo_tags " + strings.Join(conditionalStrings, "\n")
	if _, err := tx.ExecContext(ctx, query, conditionalArgs...); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	query = fmt.Sprintf(`
INSERT INTO repo_tags (org_repo_name, tag_name, module_path, created)
VALUES %s
ON CONFLICT (org_repo_name, tag_name) DO UPDATE
SET created = EXCLUDED.created;`, strings.Join(valueStrings, ",\n"))
	if _, err := tx.ExecContext(ctx, query, valueArgs...); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	query = `UPDATE repos
SET indexing_finished = NOW()` + "\n" + strings.Join(conditionalStrings, "\n")
	if _, err := tx.ExecContext(ctx, query, conditionalArgs...); err != nil {
		return fmt.Errorf("StoreRepoTags:\nquery: %s\nerror: %v", query, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("StoreRepoTags: %v", err)
	}

	return nil
}
