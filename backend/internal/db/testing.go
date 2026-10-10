package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// OpenTest creates a throwaway PostgreSQL database for one test and drops it
// afterwards. ALERTFLOW_TEST_DATABASE_URL must point at a maintenance database
// (usually named postgres). The helper never drops the application database.
func OpenTest(t *testing.T) *DB {
	t.Helper()
	adminURL := os.Getenv("ALERTFLOW_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("ALERTFLOW_TEST_DATABASE_URL is required; point it at the postgres maintenance database (not the app database named alertflow)")
	}
	name := "alertflow_test_" + randomHex(t)
	if !strings.HasPrefix(name, "alertflow_test_") || name == "alertflow" {
		t.Fatalf("refusing to use database %q", name)
	}

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Fatalf("ping admin db: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name)); err != nil {
		admin.Close()
		t.Fatalf("create test database %s: %v", name, err)
	}

	testURL, err := withDatabase(adminURL, name)
	if err != nil {
		dropTestDB(admin, name)
		admin.Close()
		t.Fatalf("build test url: %v", err)
	}
	database, err := Open(testURL)
	if err != nil {
		dropTestDB(admin, name)
		admin.Close()
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() {
		database.Close()
		dropTestDB(admin, name)
		admin.Close()
	})
	return database
}

func dropTestDB(admin *sql.DB, name string) {
	if !strings.HasPrefix(name, "alertflow_test_") || name == "alertflow" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = admin.ExecContext(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
		name)
	_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(name))
}

func withDatabase(rawURL, name string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func randomHex(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	return hex.EncodeToString(b[:])
}
