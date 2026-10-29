package api

import (
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/query"
)

func attachQuery(s *Server) {
	s.queries = query.New(s.database, s.config)
}

func serve(app *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}
