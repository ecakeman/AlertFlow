package system

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEpisodeTimelineMonitorPoints 覆盖时间轴新增的 monitor_points：
// 起点之后最近 200 条、point_seq 升序、软删目标保留、空数组不返回 null。
func TestEpisodeTimelineMonitorPoints(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	svc, target := newSvc(t, s, 2, 1)
	base := time.Now().UTC()
	apply := func(offset int, class, errClass string) ApplyResult {
		t.Helper()
		res, err := s.ApplyMonitorSample(ctx, MonitorSample{
			ServiceID: svc, TargetID: target, ObservedAt: base.Add(time.Duration(offset) * time.Second),
			Success: false, StatusClass: class, ErrorClass: errClass,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// 前两条失败触发 Episode，创建时 monitor_start_point_seq 已覆盖这两条点。
	apply(0, "5xx", "upstream")
	res := apply(1, "5xx", "upstream")
	if !res.CreatedEpisode || res.EpisodeID == uuid.Nil {
		t.Fatalf("episode not created: %+v", res)
	}
	episodeID := res.EpisodeID

	body, err := s.EpisodeTimeline(ctx, episodeID)
	if err != nil {
		t.Fatal(err)
	}
	pts, ok := body["monitor_points"].([]EpisodeMonitorPoint)
	if !ok || pts == nil || len(pts) != 0 {
		t.Fatalf("empty monitor_points = %#v", body["monitor_points"])
	}
	raw, err := json.Marshal(body["monitor_points"])
	if err != nil || string(raw) != "[]" {
		t.Fatalf("empty monitor_points json = %s %v", raw, err)
	}
	// 旧字段全部保留。
	for _, key := range []string{"episode", "region1_projection", "monitor_start_point_seq", "region2_events", "region3_refs"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("legacy field %q missing", key)
		}
	}

	// Episode 起点之后的点按 point_seq 升序返回。
	apply(2, "timeout", "")
	apply(3, "", "")
	// 目标软删除后，历史采样点仍应保留 target_key/target_type。
	if _, err := s.SoftDeleteTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	body, err = s.EpisodeTimeline(ctx, episodeID)
	if err != nil {
		t.Fatal(err)
	}
	pts, ok = body["monitor_points"].([]EpisodeMonitorPoint)
	if !ok || len(pts) != 2 {
		t.Fatalf("monitor_points = %#v", body["monitor_points"])
	}
	if !(pts[0].PointSeq < pts[1].PointSeq) {
		t.Fatalf("point_seq not ascending: %+v", pts)
	}
	if pts[0].TargetID != target || pts[0].TargetKey != "api" || pts[0].TargetType != "HTTP" {
		t.Fatalf("target fields = %+v", pts[0])
	}
	if pts[0].CheckStatus != "FAILURE" || pts[0].StatusClass != "timeout" || pts[0].ErrorClass != "" {
		t.Fatalf("diagnostic fields = %+v", pts[0])
	}
	// 缺省 diagnostic 字段回退为空字符串。
	if pts[1].StatusClass != "" || pts[1].ErrorClass != "" {
		t.Fatalf("default diagnostic fields = %+v", pts[1])
	}
	if pts[0].ObservedAt.IsZero() {
		t.Fatal("observed_at missing")
	}
}
