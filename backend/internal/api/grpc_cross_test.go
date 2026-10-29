package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
)

func TestCrossLanguageGRPC(t *testing.T) {
	t.Run("agent off", func(t *testing.T) {
		database := db.OpenTest(t)
		t.Setenv("ALERTFLOW_AGENT_GRPC_TARGET", "")
		ev, err := event.NewCheckCompleted("svc", "API", "https://example.com/health", time.Now().Unix(), db.StatusOffline, nil, nil, "down", 1)
		if err != nil {
			t.Fatal(err)
		}
		ev.CheckID = 1
		if err := database.ApplyCheckEvent(ev); err != nil {
			t.Fatal(err)
		}
		if n := count(t, database, `SELECT COUNT(*) FROM incidents`); n != 1 {
			t.Fatalf("incidents = %d", n)
		}
		if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 1 {
			t.Fatalf("timeline = %d", n)
		}
		if n := count(t, database, `SELECT COUNT(*) FROM notification_deliveries`); n != 0 {
			t.Fatalf("tasks = %d", n)
		}
	})

	database := db.OpenTest(t)
	inc, err := database.CreateIncident("svc", "auto", time.Now().Unix(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := database.InsertCheck("svc", now, "online", nil, nil, "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO runtime_events (
			event_key, service_id, source, event_type, severity, title, summary,
			occurred_at, observed_at, recorded_at, cluster_id, namespace,
			resource_kind, resource_name, resource_uid, details
		 ) VALUES ('tl:1','svc','kubernetes','kubernetes.pod.changed','warning','api','restarted',
		           $1,$1,$1,'local','prod','Pod','api','uid-1','{"reason":"OOM"}')`, now,
	); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:      &config.Config{Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com/health"}}},
		database: database,
	}
	attachQuery(s)
	goAddr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.ServeControl(ctx, goAddr, "go-token") }()
	waitAddr(t, goAddr)

	py := os.Getenv("ALERTFLOW_PYTHON")
	if py == "" {
		py = "/home/sancho/.venvs/dev/bin/python3"
	}
	agentDir := filepath.Clean(filepath.Join("..", "..", "..", "agent"))
	taskAddr := freeAddr(t)
	cmd := exec.Command(py, "tests/cross_grpc.py", taskAddr, "task-token", goAddr, "go-token")
	cmd.Dir = agentDir
	cmd.Env = append(os.Environ(), "PYTHONPATH=src")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	read := bufio.NewReader(stdout)
	ready := readLine(t, read)
	if ready["ready"] != true {
		t.Fatalf("ready = %v", ready)
	}
	call := func(op string, extra map[string]any) map[string]any {
		t.Helper()
		body := map[string]any{"op": op}
		for key, value := range extra {
			body[key] = value
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stdin.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
		return readLine(t, read)
	}

	first := call("accept", map[string]any{"message_id": "m-1"})
	if first["accepted"] != true || first["duplicate"] == true || first["runs"] != float64(1) {
		t.Fatalf("accept = %v", first)
	}
	again := call("accept", map[string]any{"message_id": "m-1"})
	if again["duplicate"] != true || again["runs"] != float64(1) {
		t.Fatalf("duplicate = %v", again)
	}
	query := call("query", nil)
	if query["events"] != float64(1) || query["resourceUid"] != "uid-1" || query["name"] != "API" || query["checks"] != float64(1) {
		t.Fatalf("query = %v", query)
	}
	details, _ := query["details"].(map[string]any)
	if details["reason"] != "OOM" {
		t.Fatalf("details = %v", query["details"])
	}

	callback := map[string]any{
		"schema_version": "v2", "message_id": "run:abc:v1", "message_type": "run.completed",
		"payload": map[string]any{
			"run_id": "abc", "incident_id": inc.ID, "service_id": "svc",
			"event_type": "incident_start", "status": "completed",
			"report": map[string]any{"summary": "down"},
		},
	}
	stored := call("callback", map[string]any{"body": callback})
	firstAck, _ := stored["first"].(map[string]any)
	secondAck, _ := stored["second"].(map[string]any)
	if firstAck["stored"] != true || secondAck["duplicate"] != true {
		t.Fatalf("callback = %v", stored)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 1 {
		t.Fatalf("reports = %d", n)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'agent.run.completed'`); n != 1 {
		t.Fatalf("agent timeline = %d", n)
	}

	denied := call("bad_token", map[string]any{"body": map[string]any{
		"schema_version": "v2", "message_id": "run:nope", "message_type": "run.completed",
		"payload": map[string]any{"run_id": "nope", "service_id": "svc", "status": "completed"},
	}})
	if denied["error"] != "UNAUTHENTICATED" {
		t.Fatalf("bad token = %v", denied)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 1 {
		t.Fatalf("bad token wrote a row, reports = %d", n)
	}
	deadline := call("deadline", nil)
	if deadline["error"] != "DEADLINE_EXCEEDED" {
		t.Fatalf("deadline = %v", deadline)
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	down := call("db_down", map[string]any{"body": map[string]any{
		"schema_version": "v2", "message_id": "run:down", "message_type": "run.failed",
		"payload": map[string]any{"run_id": "down", "service_id": "svc", "status": "failed", "error": "x"},
	}})
	if down["error"] != "INTERNAL" && down["error"] != "UNAVAILABLE" {
		t.Fatalf("db down = %v", down)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func waitAddr(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing accepted on %s", addr)
}

func readLine(t *testing.T, read *bufio.Reader) map[string]any {
	t.Helper()
	_ = read
	line, err := read.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read harness: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatalf("harness line %s: %v", line, err)
	}
	return out
}

func count(t *testing.T, database *db.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
