// Package dbtest starts an ephemeral, schema-loaded Postgres container for
// integration tests via testcontainers-go, so `go test ./...` needs only a
// working Docker daemon -- no pre-existing database, no manual setup.
//
// One container is started per test binary (i.e. per package), not per
// test: StartContainer is meant to be called once from a package's
// TestMain, and Connect is called per-test to get an isolated connection
// against it. Connect truncates every table before returning, so tests
// don't see each other's data despite sharing the container.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const (
	image   = "postgres:16-alpine"
	dbName  = "aurik_test"
	dbUser  = "aurik"
	dbPass  = "aurik"
	timeout = 60 * time.Second
)

// StartContainer starts a Postgres container with db/schema.sql already
// applied and returns a connection string plus a cleanup func. Call once
// from TestMain:
//
//	func TestMain(m *testing.M) {
//		dsn, cleanup, err := dbtest.StartContainer()
//		if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
//		testDSN = dsn
//		code := m.Run()
//		cleanup()
//		os.Exit(code)
//	}
func StartContainer() (dsn string, cleanup func(), err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	schemaPath, err := schemaSQLPath()
	if err != nil {
		return "", nil, err
	}

	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPass),
		postgres.WithInitScripts(schemaPath),
		postgres.BasicWaitStrategies(), // postgres restarts itself once after initdb; wait for that too
	)
	if err != nil {
		return "", nil, fmt.Errorf("dbtest: start postgres container: %w", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = container.Terminate(context.Background())
		return "", nil, fmt.Errorf("dbtest: connection string: %w", err)
	}

	cleanup = func() {
		if err := container.Terminate(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "dbtest: terminate container: %v\n", err)
		}
	}
	return connStr, cleanup, nil
}

// Connect opens a pool against dsn (as returned by StartContainer),
// truncates every table so this test starts from a clean slate, and
// registers the pool to close on test cleanup.
func Connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dbtest: connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("dbtest: ping: %v", err)
	}

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE batch_outcomes, events, batches, machine_views`); err != nil {
		t.Fatalf("dbtest: reset tables: %v", err)
	}

	return pool
}

// schemaSQLPath locates db/schema.sql relative to this source file, so it
// works regardless of the working directory `go test` is invoked from.
func schemaSQLPath() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("dbtest: cannot determine caller file")
	}
	// this file: internal/dbtest/dbtest.go -> schema: db/schema.sql
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "db", "schema.sql")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("dbtest: schema.sql not found at %s: %w", path, err)
	}
	return path, nil
}
