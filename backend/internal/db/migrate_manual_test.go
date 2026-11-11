//go:build manualmigration

package db

// 手工迁移冒烟测试：go test -tags manualmigration ./backend/internal/db -run TestManualMigrationDownUp -v
// 该测试会把共享测试库回滚到 0 再升级，因此默认不参与 go test ./...。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestManualMigrationDownUp(t *testing.T) {
	conn, err := sql.Open("pgx", "postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	// 清空数据，避免 Down 重建唯一约束时撞到软删除留下的同名行。
	if _, err := conn.ExecContext(ctx, `TRUNCATE
		system.episode_input_versions, system.episode_artifact_refs, system.outbox_events,
		system.agent_tasks, system.episode_app_events, system.application_event_receipts,
		system.episodes, system.service_monitor_points, system.service_source_states,
		system.service_monitor_target_states, system.service_monitor_targets, system.services,
		cognitive.fault_documents, cognitive.episode_artifacts, cognitive.agent_task_receipts
		CASCADE`); err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(conn, "migrations", 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var tables int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema IN ('system','cognitive')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("tables after down = %d", tables)
	}
	if err := goose.Up(conn, "migrations"); err != nil {
		t.Fatalf("up: %v", err)
	}
	var cols int
	if err := conn.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema='system' AND column_name='deleted_at'
		  AND table_name IN ('services','service_monitor_targets')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 2 {
		t.Fatalf("deleted_at columns = %d", cols)
	}
	var indexes int
	if err := conn.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname='system'
		  AND indexname IN ('uniq_services_service_key_active','uniq_targets_service_key_active')`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 2 {
		t.Fatalf("partial unique indexes = %d", indexes)
	}
	var def string
	if err := conn.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='targets_type_chk'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "KUBERNETES") || !strings.Contains(def, "HTTP") || !strings.Contains(def, "TCP") {
		t.Fatalf("targets_type_chk = %s", def)
	}
}
