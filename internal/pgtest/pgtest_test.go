package pgtest_test

import (
	"os"
	"testing"

	"github.com/Netflix-Skunkworks/golang-index/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.RunMain(m)) }

// TestFreshDB is a smoke test: it proves the embedded server starts, migrations
// apply, and the schema is present.
func TestFreshDB(t *testing.T) {
	_, sqlDB := pgtest.FreshDB(t)

	var n int
	err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('repos', 'repo_tags', 'repo_indexing')`).
		Scan(&n)
	if err != nil {
		t.Fatalf("querying schema: %v", err)
	}
	if n != 3 {
		t.Errorf("found %d of the 3 expected tables, want 3", n)
	}
}

// TestFreshDB_Isolated confirms two FreshDB calls get independent databases.
func TestFreshDB_Isolated(t *testing.T) {
	_, a := pgtest.FreshDB(t)
	_, b := pgtest.FreshDB(t)

	if _, err := a.ExecContext(t.Context(), `INSERT INTO repos (org_repo_name) VALUES ('foo/bar')`); err != nil {
		t.Fatalf("inserting into first db: %v", err)
	}

	var n int
	if err := b.QueryRowContext(t.Context(), `SELECT count(*) FROM repos`).Scan(&n); err != nil {
		t.Fatalf("counting second db: %v", err)
	}
	if n != 0 {
		t.Errorf("second db saw %d repos, want 0 (databases not isolated)", n)
	}
}
