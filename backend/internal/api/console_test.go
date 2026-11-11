package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	"github.com/google/uuid"
)

func createServiceForAPI(t *testing.T, h http.Handler) system.ServiceView {
	t.Helper()
	body := fmt.Sprintf(`{"service_key":"svc-%s","name":"orders","authority_producer_id":"order-api",
		"targets":[{"target_key":"api","target_name":"API","target_type":"HTTP",
		"address":"http://orders/health","interval_ms":1000,"timeout_ms":200,
		"failure_threshold":2,"recovery_threshold":2}]}`, uuid.NewString()[:8])
	rec := do(t, h, http.MethodPost, "/api/v1/services", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create service %d %s", rec.Code, rec.Body.String())
	}
	var view system.ServiceView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

func TestReadyz(t *testing.T) {
	h := testAPI(t).Handler()
	rec := do(t, h, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["status"] != "ok" {
		t.Fatalf("readyz body = %s %v", rec.Body.String(), err)
	}
}

func TestDashboardEndpoint(t *testing.T) {
	h := testAPI(t).Handler()
	createServiceForAPI(t, h)
	rec := do(t, h, http.MethodGet, "/api/v1/dashboard/summary", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rec.Code, rec.Body.String())
	}
	var summary system.DashboardSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Services.Total != 1 || summary.Targets.Total != 1 || summary.Episodes.Active != 0 {
		t.Fatalf("dashboard = %+v", summary)
	}
	if summary.MonitorTrend == nil || len(summary.MonitorTrend) != 0 {
		t.Fatalf("empty trend = %+v", summary.MonitorTrend)
	}
	if summary.RecentEpisodes == nil {
		t.Fatal("recent_episodes must be an array")
	}
}

func TestServiceListFiltersAndDelete(t *testing.T) {
	h := testAPI(t).Handler()
	first := createServiceForAPI(t, h)
	createServiceForAPI(t, h)
	rec := do(t, h, http.MethodGet, "/api/v1/services?limit=50&offset=0", "")
	var list struct {
		Services []system.ServiceSummary `json:"services"`
		Total    int                     `json:"total"`
		Limit    int                     `json:"limit"`
		Offset   int                     `json:"offset"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	if list.Total != 2 || len(list.Services) != 2 || list.Limit != 50 {
		t.Fatalf("list = %+v", list)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/services?status=BAD", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/services?limit=x", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit %d", rec.Code)
	}
	path := "/api/v1/services/" + first.ServiceID.String()
	if rec := do(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted %d", rec.Code)
	}
	var pages struct {
		Total int `json:"total"`
	}
	rec = do(t, h, http.MethodGet, "/api/v1/services", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &pages)
	if pages.Total != 1 {
		t.Fatalf("total after delete = %d", pages.Total)
	}
	if rec := do(t, h, http.MethodDelete, path, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete %d", rec.Code)
	}
}

func TestTargetRoutes(t *testing.T) {
	h := testAPI(t).Handler()
	svc := createServiceForAPI(t, h)
	base := "/api/v1/services/" + svc.ServiceID.String() + "/targets"
	rec := do(t, h, http.MethodGet, base, "")
	var targets struct {
		Targets []system.Target `json:"targets"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &targets) != nil || len(targets.Targets) != 1 {
		t.Fatalf("targets %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, base, `{"target_key":"extra","target_name":"extra","target_type":"HTTP",
		"address":"http://extra/health","interval_ms":1000,"timeout_ms":200,
		"failure_threshold":1,"recovery_threshold":1}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create target %d %s", rec.Code, rec.Body.String())
	}
	var created system.Target
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.IntervalMS != 1000 || created.LastCheckAt != nil {
		t.Fatalf("created target = %+v", created)
	}
	targetPath := "/api/v1/targets/" + created.TargetID.String()
	rec = do(t, h, http.MethodPatch, targetPath, `{"target_name":"renamed","interval_ms":2000}`)
	var updated system.Target
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &updated) != nil ||
		updated.TargetName != "renamed" || updated.IntervalMS != 2000 {
		t.Fatalf("update target %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPatch, targetPath, `{"address":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid patch %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPatch, "/api/v1/targets/"+uuid.NewString(), `{"target_name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing target %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPatch, "/api/v1/targets/not-a-uuid", `{"target_name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad uuid %d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, targetPath, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete target %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPatch, targetPath, `{"target_name":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("update deleted target %d", rec.Code)
	}
	if rec := do(t, h, http.MethodDelete, targetPath, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete target %d", rec.Code)
	}
}

func TestEpisodesEndpoint(t *testing.T) {
	server := testAPI(t)
	h := server.Handler()
	svc := createServiceForAPI(t, h)
	targetID := svc.Targets[0].TargetID
	base := time.Now().UTC()
	if _, err := server.Store.ApplyMonitorSample(context.Background(), system.MonitorSample{
		ServiceID: svc.ServiceID, TargetID: targetID, ObservedAt: base, Success: false, StatusClass: "5xx",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Store.ApplyMonitorSample(context.Background(), system.MonitorSample{
		ServiceID: svc.ServiceID, TargetID: targetID, ObservedAt: base.Add(time.Second), Success: false, StatusClass: "5xx",
	}); err != nil {
		t.Fatal(err)
	}
	rec := do(t, h, http.MethodGet, "/api/v1/episodes?limit=20&offset=0", "")
	var list struct {
		Episodes []system.EpisodeSummary `json:"episodes"`
		Total    int                     `json:"total"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("episodes %d %s", rec.Code, rec.Body.String())
	}
	if list.Total != 1 || list.Episodes[0].ServiceName != "orders" {
		t.Fatalf("episodes = %+v", list)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes?status=BAD", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad episode status %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes?from=yesterday", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad from %d", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes?service_id="+uuid.NewString(), ""); rec.Code != http.StatusOK {
		t.Fatalf("filter by service %d", rec.Code)
	}
}
