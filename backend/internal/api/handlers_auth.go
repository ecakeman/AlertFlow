package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/auth"
)

// GET /api/setup/status → whether the first-run admin still needs creating.
func (s *Server) handleSetupStatus(c *gin.Context) error {
	n, err := s.conn().CountUsers()
	if err != nil {
		return fail(http.StatusInternalServerError, "database error")
	}
	return writeJSON(c, http.StatusOK, map[string]any{"needsSetup": n == 0})
}

// POST /api/setup → create the first admin account (only when none exist).
func (s *Server) handleSetup(c *gin.Context) error {
	n, err := s.conn().CountUsers()
	if err != nil {
		return fail(http.StatusInternalServerError, "database error")
	}
	if n > 0 {
		return fail(http.StatusConflict, "setup already completed")
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		return err
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return fail(http.StatusBadRequest, "username is required")
	}
	if len(req.Password) < auth.MinPasswordLen || len(req.Password) > auth.MaxPasswordLen {
		return fail(http.StatusBadRequest, "password must be 8–72 characters")
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not hash password")
	}
	u, err := s.conn().CreateUser(username, hash, "admin")
	if err != nil {
		return fail(http.StatusInternalServerError, "could not create user")
	}
	if err := s.startSession(c, u.ID); err != nil {
		return fail(http.StatusInternalServerError, "could not start session")
	}
	return writeJSON(c, http.StatusOK, toUserDTO(u))
}

// POST /api/auth/login → verify credentials and set the session cookie.
func (s *Server) handleLogin(c *gin.Context) error {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		return err
	}
	username := strings.TrimSpace(req.Username)
	if !s.logins.allow(username) {
		return fail(http.StatusTooManyRequests, "too many attempts, please wait a minute")
	}
	u, err := s.conn().GetUserByUsername(username)
	if err != nil || !auth.VerifyPassword(u.PasswordHash, req.Password) {
		return fail(http.StatusUnauthorized, "invalid username or password")
	}
	if err := s.startSession(c, u.ID); err != nil {
		return fail(http.StatusInternalServerError, "could not start session")
	}
	return writeJSON(c, http.StatusOK, toUserDTO(u))
}

// POST /api/auth/logout → delete the session and clear the cookie.
func (s *Server) handleLogout(c *gin.Context) error {
	if token := cookieValue(c, auth.CookieName); token != "" {
		_ = s.conn().DeleteSession(token)
	}
	clearSessionCookie(c)
	return writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/auth/me → the current user.
func (s *Server) handleMe(c *gin.Context) error {
	return writeJSON(c, http.StatusOK, toUserDTO(userLocal(c)))
}

// startSession creates a session row and sets the cookie.
func (s *Server) startSession(c *gin.Context, userID int64) error {
	token, err := auth.GenerateToken()
	if err != nil {
		return err
	}
	now := time.Now()
	expires := now.Add(auth.SessionTTL)
	if err := s.conn().CreateSession(token, userID, now.Unix(), expires.Unix()); err != nil {
		return err
	}
	setSessionCookie(c, token, expires)
	return nil
}
