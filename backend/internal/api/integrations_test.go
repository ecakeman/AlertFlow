package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/auth"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestRetiredIntegrationWritesAreRejected(t *testing.T) {
	database := db.OpenTest(t)
	s := &Server{cfg: &config.Config{}, database: database}
	attachQuery(s)
	app := s.newEngine()
	user, err := database.CreateUser("admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	const token = "session-token"
	now := time.Now().Unix()
	if err := database.CreateSession(token, user.ID, now, now+3600); err != nil {
		t.Fatal(err)
	}

	status, body := integrationRequest(t, app, http.MethodPost, "/api/integrations", token,
		`{"type":"slack","name":"ops","enabled":true,"config":{"webhookUrl":"https://example.invalid"}}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "retired") {
		t.Fatalf("create slack = %d %s", status, body)
	}

	existing, err := database.CreateIntegration("email", "mail", true, false, []byte(`{"host":"smtp.example.com","from":"a@b.c","to":"d@e.f"}`))
	if err != nil {
		t.Fatal(err)
	}
	status, body = integrationRequest(t, app, http.MethodPut, "/api/integrations/"+strconv.FormatInt(existing.ID, 10), token,
		`{"type":"email","name":"mail","enabled":false,"config":{}}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "retired") {
		t.Fatalf("update email = %d %s", status, body)
	}
	status, body = integrationRequest(t, app, http.MethodPost, "/api/integrations/"+strconv.FormatInt(existing.ID, 10)+"/test", token, "")
	if status != http.StatusBadRequest || !strings.Contains(body, "retired") {
		t.Fatalf("test email = %d %s", status, body)
	}

	status, body = integrationRequest(t, app, http.MethodPost, "/api/integrations", token,
		`{"type":"agent_webhook","name":"agent","enabled":true,"config":{"url":"http://agent.example/hook"}}`)
	if status != http.StatusCreated || !strings.Contains(body, "agent_webhook") {
		t.Fatalf("create agent = %d %s", status, body)
	}
}

func integrationRequest(t *testing.T, app *gin.Engine, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	rec := serve(app, req)
	return rec.Code, rec.Body.String()
}
