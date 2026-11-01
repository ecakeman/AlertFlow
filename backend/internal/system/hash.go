package system

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// MonitorField 是区域一参与哈希的白名单字段。
type MonitorField struct {
	TargetKey   string `json:"target_key"`
	TargetType  string `json:"target_type"`
	Status      string `json:"status"`
	StatusClass string `json:"status_class"`
	ErrorClass  string `json:"error_class"`
}

// AppCanon 是区域二参与哈希的字段。event_id 与时间不在其中。
type AppCanon struct {
	EventKind    string          `json:"event_kind"`
	HealthStatus string          `json:"health_status"`
	Payload      json.RawMessage `json:"payload"`
}

type canonicalInput struct {
	AppEvents         []AppCanon     `json:"app_events"`
	MonitorProjection []MonitorField `json:"monitor_projection"`
}

// CanonicalPayload 规范化 JSON，对象键按编码顺序稳定。
func CanonicalPayload(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage([]byte("{}")), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashInput 计算诊断输入哈希。投影按 target_key 排序，应用事件保持追加顺序。
func HashInput(projection []MonitorField, events []AppCanon) (string, error) {
	proj := append([]MonitorField(nil), projection...)
	sort.Slice(proj, func(i, j int) bool { return proj[i].TargetKey < proj[j].TargetKey })
	if events == nil {
		events = []AppCanon{}
	}
	body, err := json.Marshal(canonicalInput{AppEvents: events, MonitorProjection: proj})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// RequestHash 对应用上报原文做哈希，用于同 event_id 的内容冲突检测。
func RequestHash(raw []byte) (string, error) {
	canon, err := CanonicalPayload(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}
