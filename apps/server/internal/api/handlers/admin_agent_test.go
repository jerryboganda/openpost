package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humaecho"
	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/stretchr/testify/require"
)

type agentTestDispatcher struct {
	dispatched []adminagent.DispatchRequest
	cancelled  []string
}

func (d *agentTestDispatcher) Dispatch(_ context.Context, request adminagent.DispatchRequest) (adminagent.DispatchResult, error) {
	d.dispatched = append(d.dispatched, request)
	return adminagent.DispatchResult{Ref: "main", URL: "https://github.com/example/actions"}, nil
}

func (d *agentTestDispatcher) Cancel(_ context.Context, run adminagent.AgentRun) error {
	d.cancelled = append(d.cancelled, run.ID)
	return nil
}

func newAdminAgentTestServer(t *testing.T, isAdmin bool, dispatcher *agentTestDispatcher) (*echo.Echo, *adminagent.Service, *AdminAgentHandler) {
	t.Helper()
	db := createHandlerTestDB(t, (*models.User)(nil))
	_, err := db.NewInsert().Model(&models.User{
		ID: "user-1", Email: "admin@example.com", DisplayName: "Admin",
		PasswordHash: "hash", IsAdmin: isAdmin, CreatedAt: time.Now().UTC(),
	}).Exec(t.Context())
	require.NoError(t, err)
	migration, err := os.ReadFile("../../database/migrations/176_admin_agent.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)

	service := adminagent.NewService(db, dispatcher)
	handler := NewAdminAgentHandler(service, db, browserSessionTestAuthenticator(), "callback-secret")
	e := echo.New()
	api := humaecho.NewWithGroup(e, e.Group("/api/v1"), huma.DefaultConfig("Test", "1.0.0"))
	handler.RegisterRoutes(api)
	handler.RegisterEchoRoutes(e)
	return e, service, handler
}

func requestAgent(t *testing.T, e *echo.Echo, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	request := httptest.NewRequestWithContext(t.Context(), method, path, &payload)
	request.Header.Set("Authorization", "Bearer web-token")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func TestAdminAgentAuthorizationAndConversationFlow(t *testing.T) {
	t.Run("ordinary user cannot list conversations", func(t *testing.T) {
		e, _, _ := newAdminAgentTestServer(t, false, &agentTestDispatcher{})
		response := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/conversations", nil)
		require.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("api token cannot use the administrator agent", func(t *testing.T) {
		e, _, _ := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
		request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/agent/conversations", nil)
		request.Header.Set("Authorization", "Bearer bad-token")
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		require.NotEqual(t, http.StatusOK, recorder.Code)
	})

	t.Run("admin creates a conversation and starts a run", func(t *testing.T) {
		dispatcher := &agentTestDispatcher{}
		e, service, _ := newAdminAgentTestServer(t, true, dispatcher)

		created := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations", map[string]string{
			"engine": "opencode",
			"title":  "Check production",
		})
		require.Equal(t, http.StatusOK, created.Code, created.Body.String())
		var createdBody struct {
			AgentConversation struct {
				ID string `json:"id"`
			} `json:"conversation"`
		}
		require.NoError(t, json.NewDecoder(created.Body).Decode(&createdBody))
		require.NotEmpty(t, createdBody.AgentConversation.ID)
		require.Len(t, dispatcher.dispatched, 0)

		started := requestAgent(t, e, http.MethodPost,
			"/api/v1/admin/agent/conversations/"+createdBody.AgentConversation.ID+"/messages",
			map[string]string{"prompt": "Inspect readiness."})
		require.Equal(t, http.StatusOK, started.Code, started.Body.String())
		var startedBody struct {
			AgentRun adminagent.AgentRun `json:"run"`
		}
		require.NoError(t, json.NewDecoder(started.Body).Decode(&startedBody))
		require.Equal(t, "dispatched", startedBody.AgentRun.Status)

		view, err := service.GetConversation(t.Context(), createdBody.AgentConversation.ID)
		require.NoError(t, err)
		require.Len(t, view.Messages, 1)
		require.Equal(t, "user", view.Messages[0].Role)
	})

	t.Run("unknown engine is rejected", func(t *testing.T) {
		e, _, _ := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
		response := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations", map[string]string{
			"engine": "shell",
		})
		require.Equal(t, http.StatusBadRequest, response.Code)
	})
}

func TestAdminAgentCallbackAndStream(t *testing.T) {
	dispatcher := &agentTestDispatcher{}
	e, service, _ := newAdminAgentTestServer(t, true, dispatcher)
	conversation, err := service.CreateConversation(t.Context(), "user-1", "", "", adminagent.EngineCodex)
	require.NoError(t, err)
	run, err := service.StartRun(t.Context(), conversation.ID, "Ship the change.")
	require.NoError(t, err)

	callback := map[string]any{
		"run_id":  run.ID,
		"status":  "succeeded",
		"summary": "Opened a pull request and reported the checks.",
		"events": []map[string]string{
			{"key": "phase-tests", "kind": "test", "message": "Backend tests passed"},
		},
	}
	body, err := json.Marshal(callback)
	require.NoError(t, err)
	timestamp := time.Now().UTC().Format(time.RFC3339)
	signature := adminagent.SignCallback("callback-secret", timestamp, body)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/agent/callback", bytes.NewReader(body))
	request.Header.Set(adminagent.CallbackTimestampHeader, timestamp)
	request.Header.Set(adminagent.CallbackSignatureHeader, signature)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())

	stored, err := service.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", stored.Status)
	require.Equal(t, "Opened a pull request and reported the checks.", stored.Summary)

	forged := httptest.NewRequest(http.MethodPost, "/api/v1/admin/agent/callback", bytes.NewReader(body))
	forged.Header.Set(adminagent.CallbackTimestampHeader, timestamp)
	forged.Header.Set(adminagent.CallbackSignatureHeader, "sha256=deadbeef")
	forgedRecorder := httptest.NewRecorder()
	e.ServeHTTP(forgedRecorder, forged)
	require.Equal(t, http.StatusUnauthorized, forgedRecorder.Code)

	stream := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/runs/"+run.ID+"/events", nil)
	require.Equal(t, http.StatusOK, stream.Code)
	require.Contains(t, stream.Body.String(), "event: run")
	require.Contains(t, stream.Body.String(), "event: done")
	require.Contains(t, stream.Body.String(), `"status":"succeeded"`)
}
