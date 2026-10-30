package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	adminAgentStreamLifetime = 45 * time.Minute
	adminAgentCallbackLimit  = 1 << 20
	adminAgentStreamTick     = 250 * time.Millisecond
	adminAgentPollEvery      = 4
	adminAgentHeartbeatEvery = 60
)

type AdminAgentHandler struct {
	service        *adminagent.Service
	db             *bun.DB
	auth           middleware.Authenticator
	callbackSecret string
}

type AdminAgentConversationResponse struct {
	ID          string                    `json:"id"`
	Engine      string                    `json:"engine"`
	Title       string                    `json:"title"`
	WorkspaceID string                    `json:"workspace_id,omitempty"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
	Messages    []adminagent.AgentMessage `json:"messages"`
	Runs        []adminagent.AgentRun     `json:"runs"`
	Events      []adminagent.AgentEvent   `json:"events"`
}

type adminAgentEnginesOutput struct {
	Body struct {
		Engines      []adminagent.EngineStatus `json:"engines"`
		Models       []adminagent.ModelOption  `json:"models"`
		DefaultModel string                    `json:"default_model"`
	}
}

type adminAgentConversationsOutput struct {
	Body struct {
		Conversations []adminagent.AgentConversation `json:"conversations"`
	}
}

type adminAgentConversationOutput struct {
	Body struct {
		AgentConversation AdminAgentConversationResponse `json:"conversation"`
	}
}

type adminAgentRunOutput struct {
	Body struct {
		AgentRun adminagent.AgentRun `json:"run"`
	}
}

type createAdminAgentConversationInput struct {
	Body struct {
		Engine      string `json:"engine" doc:"Engine: assistant (the built-in OpenPost Agent), opencode, codex, or claude"`
		Title       string `json:"title,omitempty" doc:"Optional conversation title; the first message names an untitled conversation"`
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
		Prompt        string   `json:"prompt,omitempty" doc:"Task or question. May be empty when images are attached"`
		AttachmentIDs []string `json:"attachment_ids,omitempty" doc:"Uploaded images to include with this message, up to 5"`
		Model         string   `json:"model,omitempty" doc:"Model id for the built-in agent; empty uses the instance default"`
		Effort        string   `json:"effort,omitempty" doc:"Reasoning effort: auto, none, minimal, low, medium, high, xhigh, or max. The model must offer it; each model in the engines response lists its efforts."`
		Access        string   `json:"access,omitempty" doc:"Access for the built-in agent: full or read_only"`
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
		Summary: "List agent engines and models", Tags: []string{"Admin"},
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
		OperationID: "delete-admin-agent-conversation", Method: http.MethodDelete, Path: "/admin/agent/conversations/{conversation_id}",
		Summary: "Delete an administrator agent conversation", Tags: []string{"Admin"},
		DefaultStatus: http.StatusNoContent,
		Middlewares:   huma.Middlewares{authMiddleware},
	}, h.deleteConversation)
	huma.Register(api, huma.Operation{
		OperationID: "start-admin-agent-run", Method: http.MethodPost, Path: "/admin/agent/conversations/{conversation_id}/messages",
		Summary: "Send a message to the administrator agent", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.startRun)
	huma.Register(api, huma.Operation{
		OperationID: "cancel-admin-agent-run", Method: http.MethodPost, Path: "/admin/agent/runs/{run_id}/cancel",
		Summary: "Cancel an administrator agent run", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{authMiddleware},
	}, h.cancelRun)
}

// RegisterEchoRoutes owns the transports Huma cannot express: the SSE progress
// stream, image upload and download, and the unauthenticated, HMAC-verified
// Actions callback.
func (h *AdminAgentHandler) RegisterEchoRoutes(e *echo.Echo) {
	auth := middleware.BearerMiddleware(h.auth)
	e.GET("/api/v1/admin/agent/runs/:run_id/events", h.streamEvents, auth)
	e.GET("/api/v1/admin/agent/runs/:run_id/context", h.runContext)
	e.POST("/api/v1/admin/agent/callback", h.callback)
	e.POST("/api/v1/admin/agent/attachments", h.uploadAttachment, auth)
	e.GET("/api/v1/admin/agent/attachments/:attachment_id", h.serveAttachment, auth)
}

func (h *AdminAgentHandler) engines(ctx context.Context, _ *struct{}) (*adminAgentEnginesOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	defaultModel, options := h.service.Models(ctx)
	out := &adminAgentEnginesOutput{}
	out.Body.Engines = h.service.Engines()
	out.Body.Models = append(make([]adminagent.ModelOption, 0, len(options)), options...)
	out.Body.DefaultModel = defaultModel
	return out, nil
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
		Conversations []adminagent.AgentConversation `json:"conversations"`
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
		return nil, huma.Error400BadRequest("engine must be assistant, opencode, codex, or claude")
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
	out.Body.AgentConversation = adminAgentConversation(*view)
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
	out.Body.AgentConversation = adminAgentConversation(*view)
	return out, nil
}

func (h *AdminAgentHandler) deleteConversation(
	ctx context.Context,
	input *adminAgentConversationPathInput,
) (*struct{}, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	if err := h.service.DeleteConversation(ctx, input.ConversationID); err != nil {
		switch {
		case errors.Is(err, adminagent.ErrConversationNotFound):
			return nil, huma.Error404NotFound("conversation not found")
		case errors.Is(err, adminagent.ErrRunActive):
			return nil, huma.Error409Conflict("stop the running task before deleting this conversation")
		default:
			log.Printf("admin agent: delete conversation %s: %v", input.ConversationID, err)
			return nil, huma.Error500InternalServerError("failed to delete agent conversation")
		}
	}
	return &struct{}{}, nil
}

func (h *AdminAgentHandler) startRun(
	ctx context.Context,
	input *startAdminAgentRunInput,
) (*adminAgentRunOutput, error) {
	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	run, err := h.service.StartRun(ctx, adminagent.StartRunInput{
		ConversationID: input.ConversationID,
		UserID:         middleware.GetUserID(ctx),
		Prompt:         input.Body.Prompt,
		AttachmentIDs:  input.Body.AttachmentIDs,
		Model:          input.Body.Model,
		Effort:         input.Body.Effort,
		Access:         input.Body.Access,
	})
	if err != nil {
		return nil, startRunError(err)
	}
	out := &adminAgentRunOutput{}
	out.Body.AgentRun = *run
	return out, nil
}

func startRunError(err error) error {
	switch {
	case errors.Is(err, adminagent.ErrConversationNotFound):
		return huma.Error404NotFound("conversation not found")
	case errors.Is(err, adminagent.ErrInvalidPrompt):
		return huma.Error400BadRequest("send a message of up to 20000 characters, an image, or both")
	case errors.Is(err, adminagent.ErrRunActive):
		return huma.Error409Conflict("a task is already running in this conversation; wait for it or stop it")
	case errors.Is(err, adminagent.ErrAttachmentNotFound), errors.Is(err, adminagent.ErrTooManyAttachments),
		errors.Is(err, adminagent.ErrInvalidModel), errors.Is(err, adminagent.ErrInvalidEffort),
		errors.Is(err, adminagent.ErrInvalidAccess):
		return huma.Error400BadRequest(err.Error())
	default:
		return huma.Error500InternalServerError("failed to start agent run")
	}
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
	out.Body.AgentRun = *run
	return out, nil
}

func (h *AdminAgentHandler) uploadAttachment(c echo.Context) error {
	if err := h.authorizeEcho(c); err != nil {
		return err
	}
	ctx := c.Request().Context()
	body := http.MaxBytesReader(c.Response(), c.Request().Body, adminagent.MaxAttachmentBytes+1)
	data, err := io.ReadAll(body)
	if err != nil {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{fieldError: "image is larger than 4 MB"})
	}
	info, err := h.service.SaveAttachment(ctx, middleware.GetUserID(ctx), c.QueryParam("filename"), data)
	if err != nil {
		if errors.Is(err, adminagent.ErrInvalidAttachment) {
			return c.JSON(http.StatusBadRequest, map[string]string{fieldError: err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{fieldError: "failed to store the image"})
	}
	return c.JSON(http.StatusCreated, info)
}

func (h *AdminAgentHandler) serveAttachment(c echo.Context) error {
	if err := h.authorizeEcho(c); err != nil {
		return err
	}
	stored, err := h.service.Attachment(c.Request().Context(), c.Param("attachment_id"))
	if err != nil {
		if errors.Is(err, adminagent.ErrAttachmentNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{fieldError: "image not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{fieldError: "image unavailable"})
	}
	header := c.Response().Header()
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	header.Set("Cache-Control", "private, max-age=86400")
	return c.Blob(http.StatusOK, stored.ContentType, stored.Data)
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
	h.forwardRunEvents(ctx, runID, run, after, send, func() bool {
		if _, err := fmt.Fprint(c.Response().Writer, ": keep-alive\n\n"); err != nil {
			return false
		}
		flusher.Flush()
		return true
	})
	return nil
}

// forwardRunEvents pushes stored events and the in-progress text of the current
// model turn. Stored events are read about once a second, or immediately when
// the live text changes, so a finished turn appears before its live copy clears.
func (h *AdminAgentHandler) forwardRunEvents(
	ctx context.Context,
	runID string,
	run *adminagent.AgentRun,
	after int64,
	send func(string, any) bool,
	keepAlive func() bool,
) {
	lastStatus := run.Status
	initial, lastVersion := h.service.LiveText(runID)
	if initial != "" {
		lastVersion = -1
	}
	ticker := time.NewTicker(adminAgentStreamTick)
	defer ticker.Stop()
	lifetime := time.NewTimer(adminAgentStreamLifetime)
	defer lifetime.Stop()
	for tick := 1; ; tick++ {
		if adminagent.RunStatus(run.Status).Terminal() {
			// Flush anything the last poll missed, so the final answer always
			// arrives before the stream ends.
			_, _ = h.sendNewEvents(ctx, runID, &after, &lastStatus, send)
			send("done", map[string]string{"status": run.Status})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-lifetime.C:
			return
		case <-ticker.C:
		}
		text, version := h.service.LiveText(runID)
		liveChanged := version != lastVersion
		if tick%adminAgentPollEvery == 0 || liveChanged {
			updated, ok := h.sendNewEvents(ctx, runID, &after, &lastStatus, send)
			if !ok {
				return
			}
			run = updated
		}
		if liveChanged {
			lastVersion = version
			if !send("live", map[string]string{"text": text}) {
				return
			}
		}
		if tick%adminAgentHeartbeatEvery == 0 && !keepAlive() {
			return
		}
	}
}

// sendNewEvents forwards stored events after the cursor and any status change.
// It returns the refreshed run, and false when the stream should end.
func (h *AdminAgentHandler) sendNewEvents(
	ctx context.Context,
	runID string,
	after *int64,
	lastStatus *string,
	send func(string, any) bool,
) (*adminagent.AgentRun, bool) {
	events, err := h.service.Events(ctx, runID, *after)
	if err != nil {
		send("stream_error", map[string]string{"message": "progress unavailable"})
		return nil, false
	}
	for _, event := range events {
		if !send("event", event) {
			return nil, false
		}
		*after = event.Seq
	}
	updated, err := h.service.GetRun(ctx, runID)
	if err != nil {
		return nil, false
	}
	if updated.Status != *lastStatus {
		*lastStatus = updated.Status
		if !send("run", updated) {
			return nil, false
		}
	}
	return updated, true
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
	var callback adminagent.AgentCallback
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

func adminAgentConversation(view adminagent.AgentConversationView) AdminAgentConversationResponse {
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
