package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humaecho"
	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/ai"
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

// agentTestChat answers every turn. A non-nil gate holds the answer until it
// is closed, which keeps a run active for conflict tests.
type agentTestChat struct{ gate chan struct{} }

func (c *agentTestChat) Chat(ctx context.Context, _ ai.ChatRequest, onText func(string)) (ai.ChatResult, error) {
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-ctx.Done():
			return ai.ChatResult{}, ctx.Err()
		}
	}
	onText("Done.")
	return ai.ChatResult{Text: "Done.", FinishReason: "stop"}, nil
}

func newAdminAgentTestServer(t *testing.T, isAdmin bool, dispatcher *agentTestDispatcher) (*echo.Echo, *adminagent.Service) {
	t.Helper()
	return newAdminAgentTestServerWithChat(t, isAdmin, dispatcher, &agentTestChat{})
}

func newAdminAgentTestServerWithChat(
	t *testing.T,
	isAdmin bool,
	dispatcher *agentTestDispatcher,
	chat ai.ChatGenerator,
) (*echo.Echo, *adminagent.Service) {
	t.Helper()
	db := createHandlerTestDB(t, (*models.User)(nil))
	// The in-memory shared-cache database fails at once when two connections
	// touch the same table, and a run that was just stopped may still be
	// releasing a cursor. Production databases wait instead, so the test uses
	// one connection and lets concurrent work take turns.
	db.SetMaxOpenConns(1)
	_, err := db.NewInsert().Model(&models.User{
		ID: "user-1", Email: "admin@example.com", DisplayName: "Admin",
		PasswordHash: "hash", IsAdmin: isAdmin, CreatedAt: time.Now().UTC(),
	}).Exec(t.Context())
	require.NoError(t, err)
	for _, name := range []string{"176_admin_agent.sql", "177_admin_agent_chat.sql"} {
		migration, err := os.ReadFile("../../database/migrations/" + name)
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}

	service := adminagent.NewService(db, dispatcher)
	service.SetAgent(chat, "deepseek-v4.1-flash", adminagent.OpenCodeModels())
	handler := NewAdminAgentHandler(service, db, browserSessionTestAuthenticator(), "callback-secret")
	e := echo.New()
	api := humaecho.NewWithGroup(e, e.Group("/api/v1"), huma.DefaultConfig("Test", "1.0.0"))
	handler.RegisterRoutes(api)
	handler.RegisterEchoRoutes(e)
	return e, service
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

func agentTestPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for x := 0; x < 8; x++ {
		for y := 0; y < 6; y++ {
			canvas.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	require.NoError(t, png.Encode(&buffer, canvas))
	return buffer.Bytes()
}

func uploadAgentImage(t *testing.T, e *echo.Echo, filename, contentType string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/admin/agent/attachments?filename="+url.QueryEscape(filename), bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer web-token")
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	return recorder
}

func decodeAgentBody(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(target), recorder.Body.String())
}

func createAgentConversation(t *testing.T, e *echo.Echo, engine string) string {
	t.Helper()
	created := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations", map[string]string{"engine": engine})
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var body struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	decodeAgentBody(t, created, &body)
	require.NotEmpty(t, body.Conversation.ID)
	return body.Conversation.ID
}

func waitForAgentRun(t *testing.T, service *adminagent.Service, runID string) *adminagent.AgentRun {
	t.Helper()
	require.Eventually(t, func() bool {
		run, err := service.GetRun(t.Context(), runID)
		return err == nil && adminagent.RunStatus(run.Status).Terminal()
	}, 10*time.Second, 10*time.Millisecond)
	run, err := service.GetRun(t.Context(), runID)
	require.NoError(t, err)
	return run
}

func TestAdminAgentAuthorizationAndConversationFlow(t *testing.T) {
	t.Run("ordinary user cannot list conversations", func(t *testing.T) {
		e, _ := newAdminAgentTestServer(t, false, &agentTestDispatcher{})
		response := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/conversations", nil)
		require.Equal(t, http.StatusForbidden, response.Code)
	})

	t.Run("api token cannot use the administrator agent", func(t *testing.T) {
		e, _ := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/admin/agent/conversations", nil)
		request.Header.Set("Authorization", "Bearer bad-token")
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, request)
		require.NotEqual(t, http.StatusOK, recorder.Code)
	})

	t.Run("admin creates a conversation and starts a run", func(t *testing.T) {
		dispatcher := &agentTestDispatcher{}
		e, service := newAdminAgentTestServer(t, true, dispatcher)

		conversationID := createAgentConversation(t, e, "opencode")
		require.Len(t, dispatcher.dispatched, 0)

		started := requestAgent(t, e, http.MethodPost,
			"/api/v1/admin/agent/conversations/"+conversationID+"/messages",
			map[string]string{"prompt": "Inspect readiness."})
		require.Equal(t, http.StatusOK, started.Code, started.Body.String())
		var startedBody struct {
			AgentRun adminagent.AgentRun `json:"run"`
		}
		decodeAgentBody(t, started, &startedBody)
		require.Equal(t, "dispatched", startedBody.AgentRun.Status)

		view, err := service.GetConversation(t.Context(), conversationID)
		require.NoError(t, err)
		require.Len(t, view.Messages, 1)
		require.Equal(t, "user", view.Messages[0].Role)
	})

	t.Run("unknown engine is rejected", func(t *testing.T) {
		e, _ := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
		response := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations", map[string]string{
			"engine": "shell",
		})
		require.Equal(t, http.StatusBadRequest, response.Code)
	})
}

func TestAdminAgentListsEnginesAndModels(t *testing.T) {
	e, _ := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
	response := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/engines", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var body struct {
		Engines []struct {
			ID        string `json:"id"`
			Label     string `json:"label"`
			Available bool   `json:"available"`
		} `json:"engines"`
		Models []struct {
			ID     string `json:"id"`
			Vision bool   `json:"vision"`
		} `json:"models"`
		DefaultModel string `json:"default_model"`
	}
	decodeAgentBody(t, response, &body)
	require.Equal(t, "assistant", body.Engines[0].ID)
	require.Equal(t, "OpenPost Agent", body.Engines[0].Label)
	require.True(t, body.Engines[0].Available)
	require.Equal(t, "deepseek-v4.1-flash", body.DefaultModel)
	require.Len(t, body.Models, len(adminagent.OpenCodeModels()))
	var vision int
	for _, model := range body.Models {
		if model.Vision {
			vision++
		}
	}
	require.Positive(t, vision)
}

func TestAdminAgentMessageOptionsConflictsAndDeletion(t *testing.T) {
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	e, _ := newAdminAgentTestServerWithChat(t, true, &agentTestDispatcher{}, &agentTestChat{gate: gate})
	conversationID := createAgentConversation(t, e, "assistant")
	messages := "/api/v1/admin/agent/conversations/" + conversationID + "/messages"

	for name, body := range map[string]map[string]any{
		"unknown model":    {"prompt": "x", "model": "gpt-99"},
		"unknown access":   {"prompt": "x", "access": "root"},
		"unknown effort":   {"prompt": "x", "effort": "extreme"},
		"empty message":    {"prompt": "   "},
		"too many images":  {"prompt": "x", "attachment_ids": []string{"1", "2", "3", "4", "5", "6"}},
		"missing an image": {"prompt": "x", "attachment_ids": []string{"missing"}},
	} {
		response := requestAgent(t, e, http.MethodPost, messages, body)
		require.Equal(t, http.StatusBadRequest, response.Code, name+": "+response.Body.String())
	}

	started := requestAgent(t, e, http.MethodPost, messages, map[string]any{
		"prompt": "Look around.", "model": "kimi-k3", "effort": "high", "access": "read_only",
	})
	require.Equal(t, http.StatusOK, started.Code, started.Body.String())
	var startedBody struct {
		Run adminagent.AgentRun `json:"run"`
	}
	decodeAgentBody(t, started, &startedBody)
	require.Equal(t, "running", startedBody.Run.Status)
	require.Equal(t, "kimi-k3", startedBody.Run.Model)
	require.Equal(t, "high", startedBody.Run.Effort)
	require.Equal(t, "read_only", startedBody.Run.Access)

	require.Equal(t, http.StatusConflict, requestAgent(t, e, http.MethodPost, messages, map[string]string{"prompt": "Again"}).Code,
		"one task at a time per conversation")
	require.Equal(t, http.StatusConflict,
		requestAgent(t, e, http.MethodDelete, "/api/v1/admin/agent/conversations/"+conversationID, nil).Code,
		"a running task must be stopped before deleting")

	canceled := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/runs/"+startedBody.Run.ID+"/cancel", nil)
	require.Equal(t, http.StatusOK, canceled.Code, canceled.Body.String())
	deleted := requestAgent(t, e, http.MethodDelete, "/api/v1/admin/agent/conversations/"+conversationID, nil)
	require.Equal(t, http.StatusNoContent, deleted.Code, deleted.Body.String())
	require.Equal(t, http.StatusNotFound,
		requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/conversations/"+conversationID, nil).Code)
	require.Equal(t, http.StatusNotFound,
		requestAgent(t, e, http.MethodDelete, "/api/v1/admin/agent/conversations/"+conversationID, nil).Code)
}

func TestAdminAgentImageUploadServingAndMessageFlow(t *testing.T) {
	e, service := newAdminAgentTestServer(t, true, &agentTestDispatcher{})
	picture := agentTestPNG(t)

	uploaded := uploadAgentImage(t, e, "shot.png", "image/png", picture)
	require.Equal(t, http.StatusCreated, uploaded.Code, uploaded.Body.String())
	var info struct {
		ID          string `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		SizeBytes   int    `json:"size_bytes"`
	}
	decodeAgentBody(t, uploaded, &info)
	require.Equal(t, "shot.png", info.Filename)
	require.Equal(t, "image/png", info.ContentType)
	require.Equal(t, 8, info.Width)
	require.Equal(t, 6, info.Height)
	require.Equal(t, len(picture), info.SizeBytes)

	served := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/attachments/"+info.ID, nil)
	require.Equal(t, http.StatusOK, served.Code)
	require.Equal(t, "image/png", served.Header().Get("Content-Type"))
	require.Equal(t, "nosniff", served.Header().Get("X-Content-Type-Options"))
	require.Contains(t, served.Header().Get("Content-Security-Policy"), "sandbox")
	require.Equal(t, picture, served.Body.Bytes())
	require.Equal(t, http.StatusNotFound, requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/attachments/missing", nil).Code)

	for name, upload := range map[string]struct {
		contentType string
		data        []byte
	}{
		"text posing as an image": {"image/png", []byte("not an image")},
		"svg with a script":       {"image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`)},
		"empty body":              {"image/png", nil},
	} {
		response := uploadAgentImage(t, e, "x.png", upload.contentType, upload.data)
		require.Equal(t, http.StatusBadRequest, response.Code, name)
	}

	conversationID := createAgentConversation(t, e, "assistant")
	started := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations/"+conversationID+"/messages",
		map[string]any{"prompt": "What is this?", "attachment_ids": []string{info.ID}})
	require.Equal(t, http.StatusOK, started.Code, started.Body.String())
	var run struct {
		Run adminagent.AgentRun `json:"run"`
	}
	decodeAgentBody(t, started, &run)
	require.Equal(t, "succeeded", waitForAgentRun(t, service, run.Run.ID).Status)

	view := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/conversations/"+conversationID, nil)
	require.Equal(t, http.StatusOK, view.Code)
	var conversation struct {
		Conversation struct {
			Messages []struct {
				Role        string `json:"role"`
				Content     string `json:"content"`
				Attachments []struct {
					ID       string `json:"id"`
					Filename string `json:"filename"`
				} `json:"attachments"`
			} `json:"messages"`
		} `json:"conversation"`
	}
	decodeAgentBody(t, view, &conversation)
	require.Len(t, conversation.Conversation.Messages, 2)
	require.Equal(t, "user", conversation.Conversation.Messages[0].Role)
	require.Len(t, conversation.Conversation.Messages[0].Attachments, 1)
	require.Equal(t, info.ID, conversation.Conversation.Messages[0].Attachments[0].ID)
	require.Equal(t, "shot.png", conversation.Conversation.Messages[0].Attachments[0].Filename)
	require.Equal(t, "Done.", conversation.Conversation.Messages[1].Content)

	reused := requestAgent(t, e, http.MethodPost, "/api/v1/admin/agent/conversations/"+conversationID+"/messages",
		map[string]any{"prompt": "Again", "attachment_ids": []string{info.ID}})
	require.Equal(t, http.StatusBadRequest, reused.Code, "an image belongs to one message")

	stream := requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/runs/"+run.Run.ID+"/events", nil)
	require.Equal(t, http.StatusOK, stream.Code)
	require.Contains(t, stream.Body.String(), `"kind":"text"`)
	require.Contains(t, stream.Body.String(), "event: done")
}

func TestAdminAgentImagesAreAdministratorOnly(t *testing.T) {
	e, _ := newAdminAgentTestServer(t, false, &agentTestDispatcher{})
	require.Equal(t, http.StatusForbidden, uploadAgentImage(t, e, "shot.png", "image/png", agentTestPNG(t)).Code)
	require.Equal(t, http.StatusForbidden, requestAgent(t, e, http.MethodGet, "/api/v1/admin/agent/attachments/anything", nil).Code)
}

func TestAdminAgentCallbackAndStream(t *testing.T) {
	dispatcher := &agentTestDispatcher{}
	e, service := newAdminAgentTestServer(t, true, dispatcher)
	conversation, err := service.CreateConversation(t.Context(), "user-1", "", "", adminagent.EngineCodex)
	require.NoError(t, err)
	run, err := service.StartRun(t.Context(), adminagent.StartRunInput{
		ConversationID: conversation.ID, UserID: "user-1", Prompt: "Ship the change.",
	})
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

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/admin/agent/callback", bytes.NewReader(body))
	request.Header.Set(adminagent.CallbackTimestampHeader, timestamp)
	request.Header.Set(adminagent.CallbackSignatureHeader, signature)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())

	stored, err := service.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", stored.Status)
	require.Equal(t, "Opened a pull request and reported the checks.", stored.Summary)

	forged := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/admin/agent/callback", bytes.NewReader(body))
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
