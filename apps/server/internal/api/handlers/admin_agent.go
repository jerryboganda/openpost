package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/api/middleware"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/uptrace/bun"
)

const (
	adminAgentStreamLifetime = 10 * time.Minute
	adminAgentCallbackLimit  = 1 << 20
)

type AdminAgentHandler struct {
	service        *adminagent.Service
	db             *bun.DB
	auth           middleware.Authenticator
	callbackSecret string
}

type AdminAgentConversationResponse struct {
	ID          string               `json:"id"`
	Engine      string               `json:"engine"`
	Title       string               `json:"title"`
	WorkspaceID string               `json:"workspace_id,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Messages    []adminagent.Message `json:"messages"`
	Runs        []adminagent.Run     `json:"runs"`
	Events      []adminagent.Event   `json:"events"`
}

type adminAgentEnginesOutput struct {
	Body struct {
		Engines []adminagent.EngineStatus `json:"engines"`
	}
}

type adminAgentConversationsOutput struct {
	Body struct {
		Conversations []adminagent.Conversation `json:"conversations"`
	}
}

type adminAgentConversationOutput struct {
	Body struct {
		Conversation AdminAgentConversationResponse `json:"conversation"`
	}
}

type adminAgentRunOutput struct {
	Body struct {
		Run adminagent.Run `json:"run"`
	}
}

type createAdminAgentConversationInput struct {
	Body struct {
		Engine      string `json:"engine" doc:"Coding engine: opencode, codex, or claude"`
		Title       string `json:"title,omitempty" doc:"Optional conversation title"`
		WorkspaceID string `json:"workspace_id,omitempty" doc:"Optional workspace boundary for the conversation"`
	}
}

type adminAgentConversationPathInput struct {
	ConversationID string `path:"conversation_id"`
}

type adminAgentRunPathInput struct {
	RunID string `path:"run_id"`
}

type startAdminAgentRunInput struct {
	ConversationID string `path:"conversation_id"`
	Body           struct {
		Prompt string `json:"prompt" doc:"Task or question for the selected coding engine"`
	}
}

func NewAdminAgentHandler(
	service *adminagent.Service,
	db *bun.DB,
	auth middleware.Authenticator,
	callbackSecret string,
) *AdminAgentHandler {
	return &AdminAgentHandler{service: service, db: db, auth: auth, callbackSecret: callbackSecret}
}

func (h *AdminAgentHandler) RegisterRoutes(api huma.API) {
	authMiddleware := middleware.AuthMiddleware(api, h.auth)
	huma.Register(api, huma.Operation{
		OperationID: "list-admin-agent-engines", Method: http.MethodGet, Path: "/admin/agent/engines",
		Summary: "List coding engines", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.engines)
	huma.Register(api, huma.Operation{
		OperationID: "list-admin-agent-conversations", Method: http.MethodGet, Path: "/admin/agent/conversations",
		Summary: "List administrator agent conversations", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.listConversations)
	huma.Register(api, huma.Operation{
		OperationID: "create-admin-agent-conversation", Method: http.MethodPost, Path: "/admin/agent/conversations",
		Summary: "Create an administrator agent conversation", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.createConversation)
	huma.Register(api, huma.Operation{
		OperationID: "get-admin-agent-conversation", Method: http.MethodGet, Path: "/admin/agent/conversations/{conversation_id}",
		Summary: "Get an administrator agent conversation", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.getConversation)
	huma.Register(api, huma.Operation{
		OperationID: "start-admin-agent-run", Method: http.MethodPost, Path: "/admin/agent/conversations/{conversation_id}/messages",
		Summary: "Start an administrator agent run", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.startRun)
	huma.Register(api, huma.Operation{
		OperationID: "cancel-admin-agent-run", Method: http.MethodPost, Path: "/admin/agent/runs/{run_id}/cancel",
		Summary: "Cancel an administrator agent run", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.cancelRun)
}

// RegisterEchoRoutes owns the two transports Huma cannot express: an SSE
// progress stream and the unauthenticated, HMAC-verified Actions callback.
func (h *AdminAgentHandler) RegisterEchoRoutes(e *echo.Echo) {
	auth := middleware.BearerMiddleware(h.auth)
	e.GET("/api/v1/admin/agent/runs/:run_id/events", h.streamEvents, auth)
	e.GET("/api/v1/admin/agent/runs/:run_id/context", h.runContext)
	e.POST("/api/v1/admin/agent/callback", h.callback)
}

func (h *AdminAgentHandler) engines(ctx context.Context, _ *struct{}) (*adminAgentEnginesOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	return &adminAgentEnginesOutput{Body: struct {
		Engines []adminagent.EngineStatus `json:"engines"`
	}{Engines: h.service.Engines()}}, nil
}

func (h *AdminAgentHandler) listConversations(ctx context.Context, _ *struct{}) (*adminAgentConversationsOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	conversations, err := h.service.ListConversations(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to list agent conversations")
	}
	return &adminAgentConversationsOutput{Body: struct {
		Conversations []adminagent.Conversation `json:"conversations"`
	}{Conversations: conversations}}, nil
}

func (h *AdminAgentHandler) createConversation(
	ctx context.Context,
	input *createAdminAgentConversationInput,
) (*adminAgentConversationOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	engine, err := adminagent.ParseEngine(input.Body.Engine)
	if err != nil {
		return nil, huma.Error400BadRequest("engine must be opencode, codex, or claude")
	}
	workspaceID := input.Body.WorkspaceID
	if workspaceID != "" {
		exists, err := h.db.NewSelect().Model((*models.Workspace)(nil)).
			Where("id = ?", workspaceID).Exists(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to validate workspace")
		}
		if !exists {
			return nil, huma.Error404NotFound("workspace not found")
		}
	}
	conversation, err := h.service.CreateConversation(
		ctx, middleware.GetUserID(ctx), workspaceID, input.Body.Title, engine,
	)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to create agent conversation")
	}
	view, err := h.service.GetConversation(ctx, conversation.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("failed to load agent conversation")
	}
	out := &adminAgentConversationOutput{}
	out.Body.Conversation = adminAgentConversation(*view)
	return out, nil
}

func (h *AdminAgentHandler) getConversation(
	ctx context.Context,
	input *adminAgentConversationPathInput,
) (*adminAgentConversationOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	view, err := h.service.GetConversation(ctx, input.ConversationID)
	if err != nil {
		if errors.Is(err, adminagent.ErrConversationNotFound) {
			return nil, huma.Error404NotFound("conversation not found")
		}
		return nil, huma.Error500InternalServerError("failed to load agent conversation")
	}
	out := &adminAgentConversationOutput{}
	out.Body.Conversation = adminAgentConversation(*view)
	return out, nil
}

func (h *AdminAgentHandler) startRun(
	ctx context.Context,
	input *startAdminAgentRunInput,
) (*adminAgentRunOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	run, err := h.service.StartRun(ctx, input.ConversationID, input.Body.Prompt)
	if err != nil {
		switch {
		case errors.Is(err, adminagent.ErrConversationNotFound):
			return nil, huma.Error404NotFound("conversation not found")
		case errors.Is(err, adminagent.ErrInvalidPrompt):
			return nil, huma.Error400BadRequest("prompt must contain between 1 and 20000 characters")
		default:
			return nil, huma.Error500InternalServerError("failed to start agent run")
		}
	}
	out := &adminAgentRunOutput{}
	out.Body.Run = *run
	return out, nil
}

func (h *AdminAgentHandler) cancelRun(
	ctx context.Context,
	input *adminAgentRunPathInput,
) (*adminAgentRunOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	run, err := h.service.CancelRun(ctx, input.RunID)
	if err != nil {
		switch {
		case errors.Is(err, adminagent.ErrRunNotFound):
			return nil, huma.Error404NotFound("run not found")
		case errors.Is(err, adminagent.ErrRunTerminal):
			return nil, huma.Error409Conflict("run has already finished")
		default:
			return nil, huma.Error500InternalServerError("failed to cancel agent run")
		}
	}
	out := &adminAgentRunOutput{}
	out.Body.Run = *run
	return out, nil
}

func (h *AdminAgentHandler) streamEvents(c echo.Context) error {
	if err := h.authorizeEcho(c); err != nil {
		return err
	}
	ctx := c.Request().Context()
	runID := c.Param("run_id")
	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)
	run, err := h.service.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, adminagent.ErrRunNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{fieldError: "run not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{fieldError: "run unavailable"})
	}
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{fieldError: "streaming unavailable"})
	}
	header := c.Response().Header()
	header.Set(echo.HeaderContentType, "text/event-stream")
	header.Set(echo.HeaderCacheControl, "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	c.Response().WriteHeader(http.StatusOK)

	send := func(event string, value any) bool {
		data, err := json.Marshal(value)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(c.Response().Writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send("run", run) {
		return nil
	}
	h.forwardRunEvents(ctx, runID, run, after, send)
	return nil
}

func (h *AdminAgentHandler) forwardRunEvents(
	ctx context.Context,
	runID string,
	run *adminagent.Run,
	after int64,
	send func(string, any) bool,
) {
	lastStatus := run.Status
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lifetime := time.NewTimer(adminAgentStreamLifetime)
	defer lifetime.Stop()
	for {
		if adminagent.RunStatus(run.Status).Terminal() {
			send("done", map[string]string{"status": run.Status})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
			events, err := h.service.Events(ctx, runID, after)
			if err != nil {
				send("stream_error", map[string]string{"message": "progress unavailable"})
				return
			}
			for _, event := range events {
				if !send("event", event) {
					return
				}
				after = event.Seq
			}
			updated, err := h.service.GetRun(ctx, runID)
			if err != nil {
				return
			}
			run = updated
			if run.Status != lastStatus {
				lastStatus = run.Status
				if !send("run", run) {
					return
				}
			}
		}
	}
}

func (h *AdminAgentHandler) runContext(c echo.Context) error {
	runID := c.Param("run_id")
	timestamp := c.Request().Header.Get(adminagent.CallbackTimestampHeader)
	signature := c.Request().Header.Get(adminagent.CallbackSignatureHeader)
	if err := adminagent.VerifyCallback(h.callbackSecret, timestamp, signature, []byte(runID), time.Now()); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{fieldError: "invalid callback signature"})
	}
	run, err := h.service.GetRun(c.Request().Context(), runID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{fieldError: "run not found"})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"run_id":          run.ID,
		"conversation_id": run.ConversationID,
		"engine":          run.Engine,
		"prompt":          run.Prompt,
		"base_revision":   run.BaseRevision,
	})
}

func (h *AdminAgentHandler) callback(c echo.Context) error {
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, adminAgentCallbackLimit))
	if err != nil {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{fieldError: "callback too large"})
	}
	timestamp := c.Request().Header.Get(adminagent.CallbackTimestampHeader)
	signature := c.Request().Header.Get(adminagent.CallbackSignatureHeader)
	if err := adminagent.VerifyCallback(h.callbackSecret, timestamp, signature, body, time.Now()); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{fieldError: "invalid callback signature"})
	}
	var callback adminagent.Callback
	if err := json.Unmarshal(body, &callback); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{fieldError: "invalid callback payload"})
	}
	if _, err := h.service.ApplyCallback(c.Request().Context(), callback); err != nil {
		if errors.Is(err, adminagent.ErrRunNotFound) {
			return c.NoContent(http.StatusNoContent)
		}
		if errors.Is(err, adminagent.ErrInvalidCallback) {
			return c.JSON(http.StatusBadRequest, map[string]string{fieldError: "invalid callback payload"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{fieldError: "callback failed"})
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *AdminAgentHandler) authorizeEcho(c echo.Context) error {
	if err := requireBrowserSessionInstanceAdmin(c.Request().Context(), h.db); err != nil {
		var statusErr huma.StatusError
		if errors.As(err, &statusErr) {
			return c.JSON(statusErr.GetStatus(), map[string]string{fieldError: statusErr.Error()})
		}
		return c.JSON(http.StatusForbidden, map[string]string{fieldError: "instance admin role required"})
	}
	return nil
}

func adminAgentConversation(view adminagent.ConversationView) AdminAgentConversationResponse {
	workspaceID := ""
	if view.WorkspaceID != nil {
		workspaceID = *view.WorkspaceID
	}
	return AdminAgentConversationResponse{
		ID:          view.ID,
		Engine:      view.Engine,
		Title:       view.Title,
		WorkspaceID: workspaceID,
		CreatedAt:   view.CreatedAt,
		UpdatedAt:   view.UpdatedAt,
		Messages:    view.Messages,
		Runs:        view.Runs,
		Events:      view.Events,
	}
}
