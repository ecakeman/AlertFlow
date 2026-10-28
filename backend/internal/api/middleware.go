package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/auth"
	"github.com/ecakeman/alertflow/internal/db"
)

const userLocalKey = "user"

// authenticate resolves the session cookie to a user and slides its expiry.
func (s *Server) authenticate(c *gin.Context) *db.User {
	token := cookieValue(c, auth.CookieName)
	if token == "" {
		return nil
	}
	now := time.Now()
	database := s.conn()
	sess, err := database.GetSession(token, now.Unix())
	if err != nil {
		return nil
	}
	u, err := database.GetUserByID(sess.UserID)
	if err != nil {
		return nil
	}
	_ = database.TouchSession(sess.Token, now.Add(auth.SessionTTL).Unix())
	return u
}

// userLocal returns the authenticated user stashed on the context.
func userLocal(c *gin.Context) *db.User {
	v, ok := c.Get(userLocalKey)
	if !ok {
		return nil
	}
	u, _ := v.(*db.User)
	return u
}

// authMW requires a valid session and stashes the user on the context.
func (s *Server) authMW(c *gin.Context) {
	u := s.authenticate(c)
	if u == nil {
		writeError(c, fail(http.StatusUnauthorized, "authentication required"))
		return
	}
	c.Set(userLocalKey, u)
}

// adminMW requires the admin role (runs after authMW).
func (s *Server) adminMW(c *gin.Context) {
	if u := userLocal(c); u == nil || u.Role != "admin" {
		writeError(c, fail(http.StatusForbidden, "administrator access required"))
		return
	}
}

// requestLogger logs API/image requests with method, status and duration.
func (s *Server) requestLogger(c *gin.Context) {
	start := time.Now()
	c.Next()
	p := c.Request.URL.Path
	if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/images/") {
		slog.InfoContext(c.Request.Context(), "http request",
			"method", c.Request.Method,
			"path", p,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(c),
		)
	}
}

// recoverJSON turns a panic into the same JSON 500 as any other server error.
func recoverJSON(c *gin.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(c.Request.Context(), "panic",
				"method", c.Request.Method, "path", c.Request.URL.Path, "error", rec)
			writeError(c, fail(http.StatusInternalServerError, "internal server error"))
		}
	}()
	c.Next()
}

// setSessionCookie writes the session cookie.
func setSessionCookie(c *gin.Context, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.CookieName, token, maxAge, "/", "", c.Request.TLS != nil, true)
}

// clearSessionCookie expires the session cookie (logout).
func clearSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.CookieName, "", -1, "/", "", c.Request.TLS != nil, true)
}
