// Package pgtest runs a real Postgres for tests with no Docker and no
// externally-provisioned database: it starts an embedded Postgres server, runs
// the repo's migrations, and hands each test its own freshly-migrated database.
//
// A package under test wires it up in TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(pgtest.RunMain(m)) }
//
// and each test calls [FreshDB] for an isolated, migrated [*db.DB].
package pgtest

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/Netflix-Skunkworks/golang-index/internal/db"
	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

const (
	adminUser = "postgres"
	adminPass = "postgres"
	adminDB   = "postgres"
	host      = "localhost"
)

// pgVersion is pinned to a release with universal binaries so Postgres runs
// natively on Apple Silicon and linux/amd64 (older majors need Rosetta on Apple
// Silicon).
const pgVersion = embeddedpostgres.V18

// server is the single embedded Postgres shared by a test package, set up in
// [RunMain]. Each test gets its own database on it via [FreshDB].
var server struct {
	port  uint16
	admin *sql.DB
}

var dbCounter atomic.Int64

// RunMain starts one embedded Postgres, runs m, then tears it down, returning
// the exit code for os.Exit. Each package gets its own ephemeral port so
// parallel packages don't collide.
func RunMain(m *testing.M) int {
	port, err := freePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: finding free port: %v\n", err)
		return 1
	}

	dataDir, err := os.MkdirTemp("", "pgtest-data-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: temp data dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dataDir)

	// BinariesPath is a stable shared cache so the ~130MB Postgres tree is
	// extracted once, not per run; DataPath and RuntimePath are per-process so
	// concurrent test packages don't clobber each other's data or socket.
	binariesPath := sharedBinariesPath()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(pgVersion).
		Username(adminUser).
		Password(adminPass).
		Database(adminDB).
		Port(uint32(port)).
		BinariesPath(binariesPath).
		DataPath(filepath.Join(dataDir, "data")).
		RuntimePath(filepath.Join(dataDir, "runtime")).
		Logger(io.Discard))

	if err := startOnce(binariesPath, pg.Start); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: starting embedded postgres: %v\n", err)
		return 1
	}
	// Stop releases the child process; skipping it blocks the caller.
	defer func() {
		if err := pg.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: stopping embedded postgres: %v\n", err)
		}
	}()

	admin, err := sql.Open("postgres", dsn(port, adminDB))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: opening admin connection: %v\n", err)
		return 1
	}
	defer admin.Close()

	server.port = port
	server.admin = admin

	return m.Run()
}

// FreshDB creates a uniquely-named, freshly-migrated database on the shared
// server and returns handles to it. The database is dropped when the test ends.
func FreshDB(t *testing.T) (*db.DB, *sql.DB) {
	t.Helper()
	if server.admin == nil {
		t.Fatal("pgtest.FreshDB: no server; the test package must call pgtest.RunMain from TestMain")
	}

	name := fmt.Sprintf("test_%d", dbCounter.Add(1))
	if _, err := server.admin.ExecContext(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("pgtest.FreshDB: creating database %q: %v", name, err)
	}
	// Production Postgres runs in UTC, which the schema and app rely on (e.g.
	// TIMESTAMP columns fed tz-aware values); match it rather than the host zone.
	if _, err := server.admin.ExecContext(t.Context(), "ALTER DATABASE "+name+" SET timezone TO 'UTC'"); err != nil {
		t.Fatalf("pgtest.FreshDB: setting UTC on database %q: %v", name, err)
	}
	t.Cleanup(func() {
		// t.Context is already cancelled during cleanup, so use a fresh one.
		if _, err := server.admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
			t.Errorf("pgtest.FreshDB: dropping database %q: %v", name, err)
		}
	})

	sqlDB, err := sql.Open("postgres", dsn(server.port, name))
	if err != nil {
		t.Fatalf("pgtest.FreshDB: opening %q: %v", name, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	migrateUp(t, sqlDB)

	sutDB, err := db.NewDB(adminUser, adminPass, host, server.port, name)
	if err != nil {
		t.Fatalf("pgtest.FreshDB: db.NewDB: %v", err)
	}
	return sutDB, sqlDB
}

func migrateUp(t *testing.T, sqlDB *sql.DB) {
	t.Helper()

	driver, err := migratepostgres.WithInstance(sqlDB, &migratepostgres.Config{})
	if err != nil {
		t.Fatalf("pgtest: creating migrate driver: %v", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationsDir(), "postgres", driver)
	if err != nil {
		t.Fatalf("pgtest: creating migrator: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("pgtest: running migrations: %v", err)
	}
}

// migrationsDir returns the absolute path to the repo's migrations directory,
// located relative to this source file so it works from any test package.
func migrationsDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func dsn(port uint16, dbname string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", adminUser, adminPass, host, port, dbname)
}

// sharedBinariesPath is a stable, per-version location for the extracted
// Postgres binaries, shared across test packages and runs.
func sharedBinariesPath() string {
	root, err := os.UserCacheDir()
	if err != nil {
		root = os.TempDir()
	}
	return filepath.Join(root, "golang-index-pgtest", "postgres-"+string(pgVersion))
}

// startOnce serializes the first, extracting Start across concurrent test
// packages. embedded-postgres guards extraction with only an in-process lock,
// but `go test ./...` runs each package in its own process, so without this two
// could extract into the shared binariesPath at once. Once the binaries exist
// Start reuses them and the lock is skipped entirely.
func startOnce(binariesPath string, start func() error) error {
	marker := filepath.Join(binariesPath, "bin", "pg_ctl")
	if _, err := os.Stat(marker); err == nil {
		return start()
	}
	if err := os.MkdirAll(filepath.Dir(binariesPath), 0o755); err != nil {
		return fmt.Errorf("preparing binaries cache: %v", err)
	}

	unlock, err := lockFile(binariesPath + ".lock")
	if err != nil {
		return fmt.Errorf("locking binaries cache: %v", err)
	}
	defer unlock()
	return start()
}

// freePort asks the kernel for an unused TCP port. There's a small window
// before Postgres binds it, which is acceptable for tests.
func freePort() (uint16, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port), nil
}
