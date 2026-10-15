// Package db wraps the PostgreSQL database that stores users, sessions and
// monitoring history. Service definitions live in config.yaml, not here.
// Schema changes are managed by goose migrations in migrations/*.sql.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// DB is a thin wrapper around *sql.DB with domain queries.
type DB struct {
	*sql.DB
}

//go:embed migrations/*.sql
var embedMigrations embed.FS

// migrateMu serialises migrations inside one process. It cannot help across
// processes, which is why migrate also takes a database advisory lock.
var migrateMu sync.Mutex

// migrationLockKey is an arbitrary but fixed key for the advisory lock that
// serialises migrations across processes.
const migrationLockKey int64 = 0x414C4657 // "ALFW"

// Open connects to the PostgreSQL database at databaseURL, checks it, and runs
// migrations. databaseURL is a postgres:// or postgresql:// DSN; credentials
// must come from the environment, not from source.
func Open(databaseURL string) (*DB, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("database url is empty (set ALERTFLOW_DATABASE_URL)")
	}
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(envInt("ALERTFLOW_DB_MAX_OPEN", 10))
	sqlDB.SetMaxIdleConns(envInt("ALERTFLOW_DB_MAX_IDLE", 5))
	sqlDB.SetConnMaxLifetime(envDuration("ALERTFLOW_DB_CONN_MAX_LIFETIME", 30*time.Minute))
	sqlDB.SetConnMaxIdleTime(envDuration("ALERTFLOW_DB_CONN_MAX_IDLE_TIME", 5*time.Minute))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := migrate(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{sqlDB}, nil
}

// migrate runs the embedded goose migrations against PostgreSQL.
//
// Four roles (server, publisher, consumer, delivery) start at the same time and
// each runs this on a fresh database. The in-process mutex only orders them
// within one process, so a session-level advisory lock does the rest — without
// it, two concurrent runs collide while goose is creating its version table
// ("goose_db_version does not exist" / "duplicate key ... pg_type_typname").
// The lock is released automatically if the process dies.
func migrate(sqlDB *sql.DB) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	ctx := context.Background()
	locker, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = locker.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	goose.SetBaseFS(embedMigrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migrations dialect: %w", err)
	}
	if err := goose.Up(sqlDB, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

// resetIdentity moves a BY DEFAULT identity sequence past the current max id
// so a later insert does not collide with ids loaded by an archive import.
// table must be a known identifier, never request input.
func resetIdentity(tx *sql.Tx, table string) error {
	q := fmt.Sprintf(`
		SELECT setval(
			pg_get_serial_sequence('%s', 'id'),
			GREATEST(COALESCE((SELECT MAX(id) FROM %s), 1), 1),
			EXISTS (SELECT 1 FROM %s)
		)`, table, table, table)
	_, err := tx.Exec(q)
	return err
}

// isUniqueViolation reports a PostgreSQL unique-constraint failure (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}
