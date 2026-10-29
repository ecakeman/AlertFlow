package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentv1 "github.com/ecakeman/alertflow/gen/alertflow/agent/v1"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/deliver"
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
	if err := database.ApplyKubernetesObject(db.KubernetesObject{
		ServiceID: "svc", ClusterID: "local", Namespace: "prod", Kind: "Pod", Name: "api",
		UID: "uid-1", Fingerprint: "fp-1", ResourceVersion: "9", Active: true, Live: true,
		ObservedAt: now, Snapshot: json.RawMessage(`{"status":{"phase":"Running","readyReplicas":1}}`),
	}, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	memory := false
	s := &Server{
		cfg: &config.Config{
			Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com/health"}},
			Settings: config.Settings{Agent: config.AgentSettings{
				MemoryEnabled: &memory, ContextBudgetTokens: 42,
				LLM: config.ModelEndpoint{Model: "cross-model"},
			}},
		},
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
		var err error
		py, err = exec.LookPath("python3")
		if err != nil {
			t.Fatal(err)
		}
	}
	agentDir := filepath.Clean(filepath.Join("..", "..", "..", "agent"))
	taskAddr := freeAddr(t)
	agentDB := agentDatabaseURL(t)
	cmd := exec.Command(py, "tests/cross_grpc.py", taskAddr, "task-token", goAddr, "go-token", agentDB)
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
		out := readLine(t, read)
		if out["error"] != nil {
			t.Fatalf("%s: %v", op, out)
		}
		return out
	}

	deniedPy := call("bad_token", nil)
	if deniedPy["code"] != "UNAUTHENTICATED" || deniedPy["persisted"] != float64(0) || deniedPy["before"] != float64(0) {
		t.Fatalf("python auth = %v", deniedPy)
	}
	ev, err := event.NewCheckCompleted("svc", "API", "https://example.com/health", now, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = 2
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	var incID int64
	if err := database.QueryRow(`SELECT id FROM incidents WHERE service_id = 'svc'`).Scan(&incID); err != nil {
		t.Fatal(err)
	}
	tasks, err := deliver.DialTasks(ctx, taskAddr, "task-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tasks.Close() })
	tasks.Q = s.queries
	clock := time.Now().Unix()
	worker := &deliver.Worker{DB: database, Owner: "cross", Now: func() int64 { return clock }, Tasks: tasks}
	if n, err := worker.ProcessDue(ctx); err != nil || n != 1 {
		t.Fatalf("investigation delivery n=%d err=%v", n, err)
	}
	if status := deliveryStatus(t, database, ev.EventID); status != "retry_wait" {
		t.Fatalf("dropped investigation ack = %s", status)
	}
	firstRuns := call("runs", nil)
	if firstRuns["persisted"] != float64(1) {
		t.Fatalf("stored before retry = %v", firstRuns)
	}
	clock += 5
	if n, err := worker.ProcessDue(ctx); err != nil || n != 1 {
		t.Fatalf("investigation retry n=%d err=%v", n, err)
	}
	if status := deliveryStatus(t, database, ev.EventID); status != "succeeded" {
		t.Fatalf("investigation delivery = %s", status)
	}
	stored := call("runs", nil)
	keys, _ := stored["keys"].([]any)
	if stored["persisted"] != float64(1) || len(keys) != 1 || keys[0] != "incident_start:"+strconv.FormatInt(incID, 10) {
		t.Fatalf("investigation runs = %v", stored)
	}

	conn, err := grpc.NewClient(goAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	badCtx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer wrong")
	_, err = agentv1.NewAlertFlowControlServiceClient(conn).SubmitAgentCallback(badCtx, &agentv1.AgentCallback{
		MessageId: "run:nope", MessageType: "run.completed", ServiceId: "svc", Status: "completed",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("go auth = %v", err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 0 {
		t.Fatalf("bad token wrote a row, reports = %d", n)
	}

	query := call("query", map[string]any{"incident_id": incID})
	cfg, _ := query["config"].(map[string]any)
	llm, _ := cfg["llm"].(map[string]any)
	if cfg["context_budget_tokens"] != float64(42) || llm["model"] != "cross-model" || cfg["memory_enabled"] != false {
		t.Fatalf("config = %v", cfg)
	}
	incidents, _ := query["incidents"].(map[string]any)
	if incidents["total"] != float64(1) {
		t.Fatalf("incidents = %v", incidents)
	}
	one, _ := query["incident"].(map[string]any)
	if one["id"] != float64(incID) || one["serviceId"] != "svc" {
		t.Fatalf("incident = %v", one)
	}
	metrics, _ := query["metrics"].(map[string]any)
	if metrics["name"] != "API" {
		t.Fatalf("metrics = %v", metrics)
	}
	checks, _ := query["checks"].(map[string]any)
	rows, _ := checks["checks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("checks = %v", checks)
	}
	timeline, _ := query["timeline"].(map[string]any)
	if timeline["resourceUid"] != "uid-1" || timeline["resourceKind"] != "Pod" || timeline["namespace"] != "prod" {
		t.Fatalf("timeline = %v", timeline)
	}
	details, _ := timeline["details"].(map[string]any)
	if details["reason"] != "OOM" {
		t.Fatalf("details = %v", timeline["details"])
	}
	sources, _ := query["sources"].(map[string]any)
	if sources["retentionDays"] == float64(0) || !sourceNamed(sources["sources"], "kubernetes") {
		t.Fatalf("coverage = %v", sources)
	}
	k8s, _ := query["kubernetes"].(map[string]any)
	objects, _ := k8s["objects"].([]any)
	if len(objects) != 1 {
		t.Fatalf("kubernetes = %v", k8s)
	}
	obj, _ := objects[0].(map[string]any)
	summary, _ := obj["summary"].(string)
	if obj["resourceVersion"] != "9" || obj["fingerprint"] != "fp-1" || !strings.Contains(summary, "Running") {
		t.Fatalf("object = %v", obj)
	}

	callbacks := call("callbacks", nil)
	if callbacks["disabled"] != true || callbacks["sent"] != float64(2) || callbacks["waiting"] != float64(1) || callbacks["retried"] != float64(1) {
		t.Fatalf("callbacks = %v", callbacks)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 3 {
		t.Fatalf("reports = %d", n)
	}
	for _, kind := range []string{"agent.run.started", "agent.run.completed", "agent.run.failed"} {
		if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = $1`, kind); n != 1 {
			t.Fatalf("%s = %d", kind, n)
		}
	}
	graph := call("graph", map[string]any{"incident_id": incID})
	if graph["status"] != "completed" || graph["sent"] != float64(2) {
		t.Fatalf("graph = %v", graph)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 5 {
		t.Fatalf("graph reports = %d", n)
	}
	for _, kind := range []string{"agent.run.started", "agent.run.completed"} {
		if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = $1`, kind); n != 2 {
			t.Fatalf("%s = %d", kind, n)
		}
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'agent.run.failed'`); n != 1 {
		t.Fatalf("failed timeline = %d", n)
	}

	payload, err := json.Marshal(db.DeliverySnapshot{
		Event: db.EventChatRequested, EventID: "chat-cross", IncidentID: &incID,
		ServiceID: "svc", ServiceName: "API", OccurredAt: now,
		Chat: &db.ChatSnapshot{RequestID: "chat-cross", Message: "why?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.CreateChatRequest(db.ChatRequestRow{
		RequestID: "chat-cross", IncidentID: incID, Message: "why?",
	}, payload, now); err != nil {
		t.Fatal(err)
	}
	if n, err := worker.ProcessDue(ctx); err != nil || n != 1 {
		t.Fatalf("chat delivery n=%d err=%v", n, err)
	}
	if status := deliveryStatus(t, database, "chat-cross"); status != "succeeded" {
		t.Fatalf("chat delivery = %s", status)
	}
	answered := call("chat_answer", nil)
	if answered["answered"] != float64(1) || answered["sent"] != float64(1) {
		t.Fatalf("chat = %v", answered)
	}
	var replyText string
	if err := database.QueryRow(`SELECT reply FROM agent_chat_requests WHERE request_id = 'chat-cross'`).Scan(&replyText); err != nil {
		t.Fatal(err)
	}
	if replyText != "cross-language answer" {
		t.Fatalf("reply = %q", replyText)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'agent.chat.completed'`); n != 1 {
		t.Fatalf("chat timeline = %d", n)
	}

	slowAddr := freeAddr(t)
	slowCtx, slowCancel := context.WithCancel(context.Background())
	t.Cleanup(slowCancel)
	go serveSlowTimeline(t, slowCtx, slowAddr)
	waitAddr(t, slowAddr)
	deadline := call("deadline", map[string]any{"target": slowAddr})
	if deadline["code"] != "DEADLINE_EXCEEDED" {
		t.Fatalf("deadline = %v", deadline)
	}

	if closed := call("break_store", nil); closed["closed"] != true {
		t.Fatalf("break = %v", closed)
	}
	downPayload, err := json.Marshal(db.DeliverySnapshot{
		Event: db.EventChatRequested, EventID: "chat-down", IncidentID: &incID,
		ServiceID: "svc", ServiceName: "API", OccurredAt: now,
		Chat: &db.ChatSnapshot{RequestID: "chat-down", Message: "still?"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := database.CreateChatRequest(db.ChatRequestRow{
		RequestID: "chat-down", IncidentID: incID, Message: "still?",
	}, downPayload, now); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ProcessDue(ctx); err != nil {
		t.Fatal(err)
	}
	if status := deliveryStatus(t, database, "chat-down"); status != "retry_wait" {
		t.Fatalf("failed persistence = %s", status)
	}
}

func TestGetSourceCoverageHonorsCanceledContext(t *testing.T) {
	database := db.OpenTest(t)
	s := &Server{
		cfg:      &config.Config{Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com/health"}}},
		database: database,
	}
	attachQuery(s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&controlServer{s: s}).GetSourceCoverage(ctx, &agentv1.GetSourceCoverageRequest{ServiceId: "svc"})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("coverage = %v", err)
	}
}

type slowTimeline struct {
	agentv1.UnimplementedAlertFlowControlServiceServer
}

func (slowTimeline) GetTimeline(ctx context.Context, _ *agentv1.GetTimelineRequest) (*agentv1.TimelinePage, error) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return &agentv1.TimelinePage{}, nil
	}
}

func serveSlowTimeline(t *testing.T, ctx context.Context, addr string) {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	gs := grpc.NewServer()
	agentv1.RegisterAlertFlowControlServiceServer(gs, slowTimeline{})
	go func() {
		<-ctx.Done()
		gs.Stop()
	}()
	_ = gs.Serve(lis)
}

func agentDatabaseURL(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv("ALERTFLOW_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Fatal("ALERTFLOW_TEST_DATABASE_URL is required")
	}
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	name := "alertflow_test_agent_" + hex.EncodeToString(buf[:])
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func deliveryStatus(t *testing.T, database *db.DB, eventID string) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM notification_deliveries WHERE event_id = $1`, eventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func sourceNamed(raw any, name string) bool {
	rows, _ := raw.([]any)
	for _, row := range rows {
		item, _ := row.(map[string]any)
		if item["source"] == name {
			return true
		}
	}
	return false
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
