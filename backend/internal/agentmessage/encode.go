package agentmessage

// 本文件负责序列化与体积裁剪。裁剪顺序与错误文案是 Agent 契约的一部分，
// 由 notify 包的黄金字节测试锁定。

import (
	"encoding/json"
	"fmt"

	"github.com/ecakeman/alertflow/internal/query"
)

// Encode builds and serialises one message. chat is nil for a diagnostic
// message. maxBytes <= 0 means MaxPayloadBytes. Both transports call this, so
// the HTTP and gRPC paths cannot drift apart.
func Encode(q *query.Service, diag DiagnosticInput, chat *ChatInput, maxBytes int) ([]byte, error) {
	if q == nil {
		return nil, errQueryServiceMissing
	}
	if chat != nil {
		return EncodeChat(*chat, maxBytes)
	}
	return EncodeDiagnostic(q, diag, maxBytes)
}

// EncodeDiagnostic builds and serialises a diagnostic envelope, trimming the
// diagnostic context until it fits.
func EncodeDiagnostic(q *query.Service, in DiagnosticInput, maxBytes int) ([]byte, error) {
	return encodeMessage(BuildDiagnostic(q, in), payloadLimit(maxBytes))
}

// EncodeChat builds and serialises a chat.requested envelope.
func EncodeChat(in ChatInput, maxBytes int) ([]byte, error) {
	body, err := json.Marshal(BuildChat(in))
	if err != nil {
		return nil, err
	}
	if limit := payloadLimit(maxBytes); len(body) > limit {
		// Permanent: retrying an oversized question can never succeed.
		return nil, fmt.Errorf("agent_webhook: chat request is %d bytes, over the %d limit", len(body), limit)
	}
	return body, nil
}

func payloadLimit(maxBytes int) int {
	if maxBytes <= 0 {
		return MaxPayloadBytes
	}
	return maxBytes
}

// encodeMessage marshals the message, trimming the diagnostic context until it
// fits. Each trimming step is recorded in coverage.truncated, so the Agent can
// tell "the data does not exist" from "the data did not fit".
func encodeMessage(message *Message, limit int) ([]byte, error) {
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(body) <= limit {
		return body, nil
	}
	ctx := message.Payload.DiagnosticContext
	if ctx == nil {
		return nil, fmt.Errorf("agent_webhook: event payload is %d bytes, over the %d limit", len(body), limit)
	}
	for _, trim := range trimSteps {
		body, err = json.Marshal(message)
		if err != nil {
			return nil, err
		}
		if len(body) <= limit {
			return body, nil
		}
		trim(ctx)
	}
	body, err = json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		// Refusing here is deliberate: a permanent error stops the delivery
		// worker from retrying something that can never fit.
		return nil, fmt.Errorf("agent_webhook: diagnostic context is %d bytes, over the %d limit", len(body), limit)
	}
	return body, nil
}

// trimSteps drops the least load-bearing context first: prose before
// measurements, measurements before the incident itself.
var trimSteps = []func(*query.DiagnosticContext){
	func(c *query.DiagnosticContext) {
		if len(c.Incident.Comments) > 10 {
			c.Incident.Comments = c.Incident.Comments[len(c.Incident.Comments)-10:]
			markTruncated(c, "COMMENTS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RelatedIncidents) > 5 {
			c.RelatedIncidents = c.RelatedIncidents[:5]
			markTruncated(c, "RELATED_INCIDENTS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Metrics.Series) > 96 {
			c.Metrics.Series = c.Metrics.Series[len(c.Metrics.Series)-96:]
			markTruncated(c, "METRICS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RecentChecks) > 50 {
			c.RecentChecks = c.RecentChecks[len(c.RecentChecks)-50:]
			markTruncated(c, "CHECKS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Incident.Comments) > 0 {
			c.Incident.Comments = nil
			markTruncated(c, "COMMENTS_DROPPED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RecentChecks) > 10 {
			c.RecentChecks = c.RecentChecks[len(c.RecentChecks)-10:]
			markTruncated(c, "CHECKS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Metrics.Series) > 0 {
			c.Metrics.Series = nil
			markTruncated(c, "METRICS_DROPPED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RelatedIncidents) > 0 {
			c.RelatedIncidents = nil
			markTruncated(c, "RELATED_INCIDENTS_DROPPED_FOR_PAYLOAD")
		}
	},
}

func markTruncated(c *query.DiagnosticContext, code string) {
	for _, existing := range c.Coverage.Truncated {
		if existing == code {
			return
		}
	}
	c.Coverage.Truncated = append(c.Coverage.Truncated, code)
	c.Coverage.Complete = false
}
