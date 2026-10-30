package main

// 角色启动自检（B4）：每个 ALERTFLOW_ROLE 都必须在 -smoke 下完成依赖构造
// （打开数据库并迁移、读取配置、构造各角色所需对象），然后不监听端口、
// 不进入常驻循环直接返回。构造失败时角色代码会 os.Exit(1)，测试进程随之
// 失败，因此“函数正常返回”本身就是通过条件。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // db 包使用的驱动，测试里直接建库需要注册
)

func TestRoleSmoke(t *testing.T) {
	setupSmokeDatabase(t)

	// state.Save 写 os.UserConfigDir()，测试里用 XDG_CONFIG_HOME 隔离，避免污染真实 HOME。
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ALERTFLOW_CONFIG_DIR", t.TempDir())
	t.Setenv("ALERTFLOW_KAFKA_BROKERS", "127.0.0.1:9092")
	t.Setenv("ALERTFLOW_AGENT_GRPC_TARGET", "")
	t.Setenv("REDIS_URL", "")

	addr := freeAddr(t)
	roles := []struct {
		name string
		run  func()
	}{
		{"server", func() { runServer(addr, "", true) }},
		{"monitor", func() { runMonitor("", true) }},
		{"publisher", func() { runPublisher(true) }},
		{"consumer", func() { runConsumer(true) }},
		{"delivery", func() { runDelivery(true) }},
		{"retire-deliveries", func() { runRetireDeliveries(true) }},
	}
	for _, role := range roles {
		role := role
		t.Run(role.name, func(t *testing.T) {
			role.run()
		})
	}

	// server 的 smoke 分支在构造后立即返回，端口必须仍然空闲。
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("server smoke left a listener on %s", addr)
	}
}

// freeAddr 借一个当前可用的本地端口，然后立刻释放，供 smoke 检查“未监听”。
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release local port: %v", err)
	}
	return addr
}

// setupSmokeDatabase 用 ALERTFLOW_TEST_DATABASE_URL 指向的维护库建一个临时库，
// 并把 ALERTFLOW_DATABASE_URL 指过去；测试结束关闭连接并删库。
func setupSmokeDatabase(t *testing.T) {
	t.Helper()
	adminURL := os.Getenv("ALERTFLOW_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("ALERTFLOW_TEST_DATABASE_URL is required; point it at the postgres maintenance database (not the app database named alertflow)")
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
	name := "alertflow_test_smoke_" + smokeHex(t)
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("create smoke database %s: %v", name, err)
	}
	u, err := url.Parse(adminURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse admin url: %v", err)
	}
	u.Path = "/" + name
	t.Setenv("ALERTFLOW_DATABASE_URL", u.String())
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		_, _ = admin.ExecContext(dropCtx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.ExecContext(dropCtx, `DROP DATABASE IF EXISTS "`+name+`"`)
		admin.Close()
	})
}

func smokeHex(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random database suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}
