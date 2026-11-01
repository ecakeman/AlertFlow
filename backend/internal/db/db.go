// Package db 打开 PostgreSQL 并执行嵌入的 goose 迁移。
package db

import (
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open 连接数据库并迁移到最新版本。
func Open(databaseURL string) (*sql.DB, error) {
	conn, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(16)
	if err := conn.Ping(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := goose.Up(conn, "migrations"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return conn, nil
}
