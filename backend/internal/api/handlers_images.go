package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"os"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/image"
)

// POST /api/services/:id/image → store a WebP icon (raw body, already optimized
// client-side). Updates the service's icon in config.yaml.
func (s *Server) handleUploadImage(c *gin.Context) error {
	id := c.Param("id")
	if s.config().Find(id) == nil {
		return fail(http.StatusNotFound, "service not found")
	}
	body, err := readBody(c)
	if err != nil {
		return err
	}
	name, err := image.Save(config.ImagesPath(s.dir()), id, body)
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	if err := s.updateConfig(func(cfg *config.Config) error {
		if svc := cfg.Find(id); svc != nil {
			svc.Icon = name
		}
		return nil
	}); err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	return writeJSON(c, http.StatusOK, map[string]any{"icon": "/images/" + name})
}

// DELETE /api/services/:id/image → remove a service's icon.
func (s *Server) handleDeleteImage(c *gin.Context) error {
	id := c.Param("id")
	if err := image.Delete(config.ImagesPath(s.dir()), id); err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	if err := s.updateConfig(func(cfg *config.Config) error {
		if svc := cfg.Find(id); svc != nil {
			svc.Icon = ""
		}
		return nil
	}); err != nil {
		return fail(http.StatusInternalServerError, err.Error())
	}
	return writeEmpty(c, http.StatusNoContent)
}

// GET /images/:file → serve a stored icon (authenticated).
func (s *Server) handleServeImage(c *gin.Context) error {
	if s.authenticate(c) == nil {
		return fail(http.StatusUnauthorized, "authentication required")
	}
	path, err := image.ResolvePath(config.ImagesPath(s.dir()), c.Param("file"))
	if err != nil {
		return fail(http.StatusBadRequest, "invalid image")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail(http.StatusNotFound, "image not found")
	}
	// Icons keep a stable filename across replacement, so revalidate each time.
	c.Header("Cache-Control", "no-cache")
	return writeBytes(c, "image/webp", data)
}
