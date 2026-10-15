package api

import (
	"time"

	"github.com/gofiber/fiber/v3"
)

type dashboardStatsDTO struct {
	// WarningServiceCount is how many distinct services logged at least one
	// warning cycle within settings.dashboard.warning_period_hours.
	WarningServiceCount int `json:"warningServiceCount"`
}

// GET /api/dashboard/stats → runtime aggregates the main dashboard's stat
// tiles need beyond what GET /api/services already carries — currently just
// the configurable-period warning count. See DashboardSettings.
func (s *Server) handleDashboardStats(c fiber.Ctx) error {
	count, err := s.query().DashboardStats(time.Now())
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not load dashboard stats")
	}
	return c.JSON(dashboardStatsDTO{WarningServiceCount: count})
}
