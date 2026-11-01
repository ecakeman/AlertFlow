package monitor

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	"github.com/google/uuid"
)

// Probe 执行一次 HTTP 或 TCP 探测，不写数据库。
func Probe(ctx context.Context, target system.DueTarget) system.MonitorSample {
	sample := system.MonitorSample{ServiceID: target.ServiceID, TargetID: target.TargetID, ObservedAt: time.Now().UTC()}
	switch target.TargetType {
	case "TCP":
		dialer := net.Dialer{Timeout: target.Timeout}
		conn, err := dialer.DialContext(ctx, "tcp", target.Address)
		if err != nil {
			sample.ErrorClass = "connect"
			sample.Raw = rawErr(err)
			return sample
		}
		_ = conn.Close()
		sample.Success = true
		sample.ErrorClass = ""
		sample.Raw = []byte(`{"connected":true}`)
		return sample
	default:
		reqCtx, cancel := context.WithTimeout(ctx, target.Timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.Address, nil)
		if err != nil {
			sample.StatusClass = "error"
			sample.Raw = rawErr(err)
			return sample
		}
		client := &http.Client{Timeout: target.Timeout, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
		resp, err := client.Do(req)
		if err != nil {
			sample.StatusClass = "timeout"
			if reqCtx.Err() == nil {
				sample.StatusClass = "error"
			}
			sample.Raw = rawErr(err)
			return sample
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		sample.StatusClass = statusClass(resp.StatusCode)
		sample.Success = resp.StatusCode >= 200 && resp.StatusCode < 400
		raw, _ := json.Marshal(map[string]any{"status_code": resp.StatusCode, "body": string(body)})
		sample.Raw = raw
		return sample
	}
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 200:
		return "2xx"
	default:
		return "error"
	}
}

func rawErr(err error) []byte {
	raw, _ := json.Marshal(map[string]string{"error": err.Error()})
	return raw
}

// Run 用固定大小的 worker 池按间隔探测。取消 ctx 后退出。
func Run(ctx context.Context, store *system.Store, workers int) {
	if workers <= 0 {
		workers = 4
	}
	jobs := make(chan system.DueTarget, workers)
	for i := 0; i < workers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case target, ok := <-jobs:
					if !ok {
						return
					}
					sample := Probe(ctx, target)
					_, _ = store.ApplyMonitorSample(ctx, sample)
				}
			}
		}()
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	next := map[uuid.UUID]time.Time{}
	for {
		select {
		case <-ctx.Done():
			close(jobs)
			return
		case <-ticker.C:
			targets, err := store.ListDueTargets(ctx)
			if err != nil {
				continue
			}
			now := time.Now()
			for _, target := range targets {
				due := next[target.TargetID]
				if now.Before(due) {
					continue
				}
				next[target.TargetID] = now.Add(target.Interval)
				select {
				case jobs <- target:
				default:
				}
			}
		}
	}
}
