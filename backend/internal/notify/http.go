package notify

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SendTimeout is the limit for one HTTP notification attempt.
const SendTimeout = 15 * time.Second

// httpClient is shared by the agent webhook sender.
var httpClient = &http.Client{Timeout: SendTimeout}

// StatusError is an HTTP response that was not successful.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Code, e.Body)
}

// Permanent reports a client error that will not succeed by retrying the same
// request. Timeouts and rate limits stay retryable.
func (e *StatusError) Permanent() bool {
	switch e.Code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	default:
		return e.Code >= 400 && e.Code < 500
	}
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
