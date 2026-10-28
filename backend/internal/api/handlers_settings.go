package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"strings"

	"github.com/ecakeman/alertflow/internal/auth"
	"github.com/ecakeman/alertflow/internal/config"
)

// GET /api/settings → current settings + active config directory.
func (s *Server) handleGetSettings(c *gin.Context) error {
	return writeJSON(c, http.StatusOK, toSettingsDTO(s.config(), s.dir()))
}

// PUT /api/settings → overwrite app settings (not services).
func (s *Server) handleUpdateSettings(c *gin.Context) error {
	var in settingsDTO
	if err := decode(c, &in); err != nil {
		return err
	}
	err := s.updateConfig(func(cfg *config.Config) error {
		if in.DefaultWidgetMode != "" {
			cfg.Settings.DefaultWidgetMode = in.DefaultWidgetMode
		}
		if in.Theme != "" {
			cfg.Settings.Theme = in.Theme
		}
		if in.Check.DefaultInterval > 0 {
			cfg.Settings.Check.DefaultInterval = in.Check.DefaultInterval
		}
		if in.Check.Timeout > 0 {
			cfg.Settings.Check.Timeout = in.Check.Timeout
		}
		if in.Check.RetentionDays > 0 {
			cfg.Settings.Check.RetentionDays = in.Check.RetentionDays
		}
		if in.Check.RetryAttempts > 0 {
			cfg.Settings.Check.RetryAttempts = in.Check.RetryAttempts
		}
		// Copy the decoded slice: it must not be aliased into the live config.
		if in.Check.RetryDelays != nil {
			cfg.Settings.Check.RetryDelays = append([]int(nil), in.Check.RetryDelays...)
		}
		if in.Dashboard.WarningPeriodHours > 0 {
			cfg.Settings.Dashboard.WarningPeriodHours = in.Dashboard.WarningPeriodHours
		}
		// Non-nil (including explicitly empty) replaces the list — that's how
		// "include every service again" is expressed; nil (omitted) leaves it.
		if in.Dashboard.UptimeExcludedServices != nil {
			cfg.Settings.Dashboard.UptimeExcludedServices = append([]string(nil), in.Dashboard.UptimeExcludedServices...)
		}
		// Agent config. Secrets keep their stored value when omitted or masked,
		// so the UI can round-trip a masked form without wiping keys.
		agent := &cfg.Settings.Agent
		if in.Agent.BaseURL != "" {
			agent.BaseURL = in.Agent.BaseURL
		}
		agent.APIToken = keepSecret(in.Agent.APIToken, agent.APIToken)
		agent.LLM = mergeEndpoint(in.Agent.LLM, agent.LLM)
		agent.Embed = mergeEndpoint(in.Agent.Embed, agent.Embed)
		agent.Rerank = mergeEndpoint(in.Agent.Rerank, agent.Rerank)
		memoryEnabled := in.Agent.MemoryEnabled
		agent.MemoryEnabled = &memoryEnabled
		if in.Agent.ContextBudgetTokens > 0 {
			agent.ContextBudgetTokens = in.Agent.ContextBudgetTokens
		}
		return nil
	})
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	return writeJSON(c, http.StatusOK, toSettingsDTO(s.config(), s.dir()))
}

// PUT /api/settings/config-path → point the app at a different config folder.
func (s *Server) handleConfigPath(c *gin.Context) error {
	var in struct {
		Path string `json:"path"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	dir := strings.TrimSpace(in.Path)
	if dir == "" {
		return fail(http.StatusBadRequest, "path is required")
	}
	if err := s.switchConfigDir(dir); err != nil {
		return fail(http.StatusBadRequest, "could not use that folder: "+err.Error())
	}
	return writeJSON(c, http.StatusOK, toSettingsDTO(s.config(), s.dir()))
}

// GET /api/config → raw config.yaml text (advanced editor).
func (s *Server) handleGetRawConfig(c *gin.Context) error {
	data, err := config.Marshal(s.config())
	if err != nil {
		return fail(http.StatusInternalServerError, "could not render config")
	}
	return writeJSON(c, http.StatusOK, map[string]any{"content": string(data)})
}

// PUT /api/config → replace config.yaml from raw text (validated).
func (s *Server) handlePutRawConfig(c *gin.Context) error {
	var in struct {
		Content string `json:"content"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	parsed, err := config.Parse([]byte(in.Content))
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	if err := s.updateConfig(func(cfg *config.Config) error {
		*cfg = *parsed
		return nil
	}); err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	return writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/users → all accounts.
func (s *Server) handleListUsers(c *gin.Context) error {
	users, err := s.conn().ListUsers()
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load users")
	}
	out := make([]userDTO, 0, len(users))
	for i := range users {
		out = append(out, toUserDTO(&users[i]))
	}
	return writeJSON(c, http.StatusOK, out)
}

// POST /api/users → create an account.
func (s *Server) handleCreateUser(c *gin.Context) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	username := strings.TrimSpace(in.Username)
	if username == "" {
		return fail(http.StatusBadRequest, "username is required")
	}
	if !validRole(in.Role) {
		return fail(http.StatusBadRequest, "role must be admin or readonly")
	}
	if len(in.Password) < auth.MinPasswordLen || len(in.Password) > auth.MaxPasswordLen {
		return fail(http.StatusBadRequest, "password must be 8–72 characters")
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not hash password")
	}
	u, err := s.conn().CreateUser(username, hash, in.Role)
	if err != nil {
		return fail(http.StatusConflict, "username already exists")
	}
	return writeJSON(c, http.StatusCreated, toUserDTO(u))
}

// PUT /api/users/:id → change role and/or password.
func (s *Server) handleUpdateUser(c *gin.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid user id")
	}
	var in struct {
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := decode(c, &in); err != nil {
		return err
	}
	target, err := s.conn().GetUserByID(id)
	if err != nil {
		return fail(http.StatusNotFound, "user not found")
	}
	role := target.Role
	if in.Role != "" {
		if !validRole(in.Role) {
			return fail(http.StatusBadRequest, "role must be admin or readonly")
		}
		if target.Role == "admin" && in.Role != "admin" {
			if n, _ := s.countAdmins(); n <= 1 {
				return fail(http.StatusBadRequest, "cannot demote the last administrator")
			}
		}
		role = in.Role
	}
	hash := ""
	if in.Password != "" {
		if len(in.Password) < auth.MinPasswordLen || len(in.Password) > auth.MaxPasswordLen {
			return fail(http.StatusBadRequest, "password must be 8–72 characters")
		}
		hash, err = auth.HashPassword(in.Password)
		if err != nil {
			return fail(http.StatusInternalServerError, "could not hash password")
		}
	}
	if err := s.conn().UpdateUser(id, role, hash); err != nil {
		return fail(http.StatusInternalServerError, "could not update user")
	}
	updated, _ := s.conn().GetUserByID(id)
	return writeJSON(c, http.StatusOK, toUserDTO(updated))
}

// DELETE /api/users/:id → remove an account.
func (s *Server) handleDeleteUser(c *gin.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return fail(http.StatusBadRequest, "invalid user id")
	}
	if me := userLocal(c); me != nil && me.ID == id {
		return fail(http.StatusBadRequest, "you cannot delete your own account")
	}
	target, err := s.conn().GetUserByID(id)
	if err != nil {
		return fail(http.StatusNotFound, "user not found")
	}
	if target.Role == "admin" {
		if n, _ := s.countAdmins(); n <= 1 {
			return fail(http.StatusBadRequest, "cannot delete the last administrator")
		}
	}
	if err := s.conn().DeleteUser(id); err != nil {
		return fail(http.StatusInternalServerError, "could not delete user")
	}
	return writeEmpty(c, http.StatusNoContent)
}

func (s *Server) countAdmins() (int, error) {
	users, err := s.conn().ListUsers()
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range users {
		if users[i].Role == "admin" {
			n++
		}
	}
	return n, nil
}

func validRole(role string) bool {
	return role == "admin" || role == "readonly"
}
