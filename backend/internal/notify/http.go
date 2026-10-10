package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SendTimeout is the limit for one HTTP notification attempt.
const SendTimeout = 15 * time.Second

// httpClient is shared by the HTTP-based senders (telegram, slack, webhook).
var httpClient = &http.Client{Timeout: SendTimeout}

// StatusError is an HTTP response that was not successful.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Code, e.Body)
}

// Permanent reports a client error. The channel configuration will not succeed
// by retrying the same request.
func (e *StatusError) Permanent() bool {
	return e.Code >= 400 && e.Code < 500
}

// postJSON POSTs a JSON body and treats any non-2xx response as an error.
func postJSON(ctx context.Context, url string, headers map[string]string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return doRequest(req)
}

// doRequest sends req and returns an error for transport failures or non-2xx.
func doRequest(req *http.Request) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(snippet))}
	}
	return nil
}
