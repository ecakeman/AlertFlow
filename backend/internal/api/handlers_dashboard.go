package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"time"
)

type dashboardStatsDTO struct {
	// WarningServiceCount is how many distinct services logged at least one
	// warning cycle within settings.dashboard.warning_period_hours.
	WarningServiceCount int `json:"warningServiceCount"`
}

// GET /api/dashboard/stats → runtime aggregates the main dashboard's stat
// tiles need beyond what GET /api/services already carries — currently just
// the configurable-period warning count. See DashboardSettings.
func (s *Server) handleDashboardStats(c *gin.Context) error {
	count, err := s.query().DashboardStats(c.Request.Context(), time.Now())
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load dashboard stats")
	}
	return writeJSON(c, http.StatusOK, dashboardStatsDTO{WarningServiceCount: count})
}
