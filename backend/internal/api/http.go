package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// httpError is a status code plus the message the client sees. The adapt
// wrapper renders it as { "error": msg }.
type httpError struct {
	Code    int
	Message string
}

func (e *httpError) Error() string { return e.Message }

func fail(code int, msg string) error {
	return &httpError{Code: code, Message: msg}
}

// adapt turns a handler that returns an error into a Gin handler. A nil error
// means the handler already wrote the success response.
func adapt(fn func(*gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			writeError(c, err)
		}
	}
}

func writeError(c *gin.Context, err error) {
	code := http.StatusInternalServerError
	msg := "internal server error"
	var he *httpError
	if errors.As(err, &he) {
		code = he.Code
		msg = he.Message
	}
	if code >= 500 {
		slog.ErrorContext(c.Request.Context(), "request error",
			"method", c.Request.Method, "path", c.Request.URL.Path, "status", code, "error", err.Error())
	}
	c.AbortWithStatusJSON(code, map[string]any{"error": msg})
}

func writeJSON(c *gin.Context, code int, v any) error {
	c.JSON(code, v)
	return nil
}

func writeEmpty(c *gin.Context, code int) error {
	c.Status(code)
	return nil
}

func writeBytes(c *gin.Context, contentType string, data []byte) error {
	c.Data(http.StatusOK, contentType, data)
	return nil
}

// readBody reads the request body up to maxBodyBytes. A larger body is 413.
// A read failure that is not the size limit is a 400, same as a bad JSON body.
func readBody(c *gin.Context) ([]byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, fail(http.StatusRequestEntityTooLarge, "request body too large")
		}
		return nil, fail(http.StatusBadRequest, "invalid request body")
	}
	return data, nil
}

// decode unmarshals the JSON request body into dst.
func decode(c *gin.Context, dst any) error {
	data, err := readBody(c)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fail(http.StatusBadRequest, "invalid request body")
	}
	return nil
}

func cookieValue(c *gin.Context, name string) string {
	v, err := c.Cookie(name)
	if err != nil {
		return ""
	}
	return v
}

// clientIP is the connection address. Proxy headers are not trusted.
func clientIP(c *gin.Context) string {
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}
