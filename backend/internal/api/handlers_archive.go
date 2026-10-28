package api

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"net/http"

	"github.com/ecakeman/alertflow/internal/archive"
)

// maxImportSize caps an uploaded backup archive.
const maxImportSize = 20 << 20 // 20 MiB

// GET /api/config/export → download a backup.zip (config.yaml + images/).
func (s *Server) handleExport(c *gin.Context) error {
	var buf bytes.Buffer
	if err := archive.Export(s.dir(), &buf, s.conn()); err != nil {
		return fail(http.StatusInternalServerError, "export failed")
	}
	c.Header("Content-Disposition", `attachment; filename="upmonitor-backup.zip"`)
	return writeBytes(c, "application/zip", buf.Bytes())
}

// POST /api/config/import → validate a backup archive, snapshot the current
// config, then apply the archive (raw application/zip body).
func (s *Server) handleImport(c *gin.Context) error {
	data, err := readBody(c)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fail(http.StatusBadRequest, "empty archive")
	}
	if len(data) > maxImportSize {
		return fail(http.StatusRequestEntityTooLarge, "archive too large")
	}
	cfg, err := archive.Import(s.dir(), data, s.conn())
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	// Swap the freshly-applied config into memory and re-sync monitoring.
	s.mu.Lock()
	s.cfg = cfg
	sched := s.sched
	s.mu.Unlock()
	sched.Sync(cfg.Services)

	return writeJSON(c, http.StatusOK, map[string]any{"ok": true, "services": len(cfg.Services)})
}
