package api

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMain 串行化共享测试库：api 与 system 两个测试包共用同一 PostgreSQL，
// 用 advisory lock 避免并发迁移以及相互 TRUNCATE 造成的干扰。
func TestMain(m *testing.M) {
	conn, err := sql.Open("pgx", "postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")
	if err != nil {
		panic(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(4815162342)"); err != nil {
		panic(err)
	}
	code := m.Run()
	_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(4815162342)")
	_ = conn.Close()
	os.Exit(code)
}
