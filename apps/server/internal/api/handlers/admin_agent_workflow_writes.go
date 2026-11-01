package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/openpost/backend/internal/services/workflows"
	"github.com/openpost/backend/internal/services/workspaceaccess"
	"github.com/uptrace/bun"
)

var workflowCreateIntent = regexp.MustCompile(`(?i)\b(create|make|add|build|set up)\b.{0,100}\bworkflow\b`)
var workflowUpdateIntent = regexp.MustCompile(`(?i)\b(update|edit|modify|change|revise)\b.{0,100}\bworkflow\b`)
var workflowQuestionIntent = regexp.MustCompile(`(?i)^\s*(can|could|would|should|how|what|why|whether|is|are|do|does)\b`)

func explicitWorkflowIntent(prompt, operation string) bool {
	prompt = strings.TrimSpace(prompt)
	if workflowQuestionIntent.MatchString(prompt) || strings.Contains(strings.ToLower(prompt), "do not ") || strings.Contains(strings.ToLower(prompt), "don't ") {
		return false
	}
	if operation == "workflow-create" {
		return workflowCreateIntent.MatchString(prompt)
	}
	if operation == "workflow-update" {
		return workflowUpdateIntent.MatchString(prompt)
	}
	return false
}

func (h *AdminAgentHandler) issueWorkflowWriteCapability(c echo.Context) error {
	body, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	if err != nil {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "request too large")
	}
	signed := append([]byte("POST "+c.Request().URL.EscapedPath()+"\n"), body...)
	if adminagent.VerifyCallback(h.callbackSecret, c.Request().Header.Get(adminagent.CallbackTimestampHeader), c.Request().Header.Get(adminagent.CallbackSignatureHeader), signed, time.Now()) != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid signature")
	}
	var input struct {
		GitHubRunID int64  `json:"github_run_id"`
		Operation   string `json:"operation"`
	}
	if json.Unmarshal(body, &input) != nil || (input.Operation != "workflow-create" && input.Operation != "workflow-update") {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid operation")
	}
	run, workspaceID, err := h.readableRun(c.Request().Context(), c.Param("run_id"))
	if err != nil {
		return err
	}
	if run.GitHubRunID != input.GitHubRunID || !explicitWorkflowIntent(run.Prompt, input.Operation) {
		return echo.NewHTTPError(http.StatusForbidden, "workflow write was not requested")
	}
	if err := h.authorizeWorkflowEditor(c.Request().Context(), run, workspaceID); err != nil {
		return err
	}
	capability, err := adminagent.IssueWorkflowWriteCapability(h.callbackSecret, *run, workspaceID, input.Operation, time.Now())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "capability unavailable")
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusOK, map[string]string{"capability": capability})
}

func (h *AdminAgentHandler) authorizeWorkflowEditor(ctx context.Context, run *adminagent.AgentRun, workspaceID string) error {
	var conversation adminagent.AgentConversation
	if err := h.db.NewSelect().Model(&conversation).Column("created_by_user_id").Where("id = ?", run.ConversationID).Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "conversation unavailable")
	}
	decision, err := workspaceaccess.NewAuthorizer(h.db).Authorize(ctx, workspaceID, workspaceaccess.ActorFacts{UserID: conversation.CreatedByUserID}, workspaceaccess.LevelEdit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "workspace access unavailable")
	}
	if !decision.Allowed {
		return echo.NewHTTPError(http.StatusForbidden, "workflow edit access denied")
	}
	return nil
}

type agentWorkflowAction struct {
	bun.BaseModel `bun:"table:admin_agent_workflow_actions"`
	RunID         string `bun:"run_id,pk"`
	ActionKey     string `bun:"action_key,pk"`
	RequestHash   string `bun:"request_hash"`
	State         string `bun:"state"`
	WorkflowID    string `bun:"workflow_id"`
	Revision      int    `bun:"revision"`
	Outcome       string `bun:"outcome"`
}

func (h *AdminAgentHandler) writeWorkflow(c echo.Context) error {
	operation := "workflow-create"
	if c.Param("id") != "" {
		operation = "workflow-update"
	}
	run, workspaceID, err := h.readableRun(c.Request().Context(), c.Param("run_id"))
	if err != nil {
		return err
	}
	token := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	if len(token) > 1024 || token == c.Request().Header.Get(echo.HeaderAuthorization) || adminagent.VerifyWorkflowWriteCapability(h.callbackSecret, token, *run, workspaceID, operation, time.Now()) != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "workflow write capability required")
	}
	if !explicitWorkflowIntent(run.Prompt, operation) {
		return echo.NewHTTPError(http.StatusForbidden, "workflow write was not requested")
	}
	if err := h.authorizeWorkflowEditor(c.Request().Context(), run, workspaceID); err != nil {
		return err
	}
	if h.workflows == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "workflow service unavailable")
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 100 || !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(key) {
		return echo.NewHTTPError(http.StatusBadRequest, "stable idempotency key required")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 256<<10))
	if err != nil {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "workflow too large")
	}
	var input workflows.SaveRequest
	if json.Unmarshal(raw, &input) != nil || input.Definition.Schema != workflows.SchemaVersion {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid workflow definition")
	}
	if operation == "workflow-create" && input.ExpectedRevision != 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "new workflow must start at revision zero")
	}
	if operation == "workflow-update" && input.ExpectedRevision < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "expected revision required")
	}
	// The agent can author native draft steps, but cannot install arbitrary executable or HTTP tools.
	if !agentAllowedWorkflowSteps(input.Definition.Steps) {
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported agent workflow step")
	}
	digest := sha256.Sum256(append([]byte(operation+"\n"+c.Param("id")+"\n"), raw...))
	hash := hex.EncodeToString(digest[:])
	ctx := c.Request().Context()
	record := agentWorkflowAction{RunID: run.ID, ActionKey: key, RequestHash: hash, State: "pending"}
	result, err := h.db.NewInsert().Model(&record).On("CONFLICT (run_id, action_key) DO NOTHING").Exec(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	if affected == 0 {
		var existing agentWorkflowAction
		if err := h.db.NewSelect().Model(&existing).Where("run_id = ? AND action_key = ?", run.ID, key).Scan(ctx); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "action unavailable")
		}
		if existing.RequestHash != hash {
			return echo.NewHTTPError(http.StatusConflict, "idempotency key already used for another request")
		}
		if existing.State != "succeeded" {
			return echo.NewHTTPError(http.StatusConflict, "action pending or failed; inspect recorded outcome")
		}
		return c.JSON(http.StatusOK, map[string]any{"id": existing.WorkflowID, "revision": existing.Revision, "outcome": existing.Outcome, "replayed": true})
	}
	var conversation adminagent.AgentConversation
	if err := h.db.NewSelect().Model(&conversation).Column("created_by_user_id").Where("id = ?", run.ConversationID).Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "conversation unavailable")
	}
	item, saveErr := h.workflows.Save(ctx, workspaceaccess.ActorFacts{UserID: conversation.CreatedByUserID}, workspaceID, c.Param("id"), input)
	state, outcome := "succeeded", "workflow draft saved"
	if saveErr != nil {
		state, outcome = "failed", "workflow draft save rejected"
	}
	_, updateErr := h.db.NewUpdate().Model((*agentWorkflowAction)(nil)).Set("state = ?, outcome = ?, workflow_id = ?, revision = ?", state, outcome, item.ID, item.Revision).Where("run_id = ? AND action_key = ? AND state = 'pending'", run.ID, key).Exec(ctx)
	if updateErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action result recording unavailable; inspect the workflow before retrying")
	}
	// The durable receipt is authoritative even when the bounded progress stream is full.
	if err := h.service.RecordWorkflowAction(ctx, run.ID, key, state, outcome); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action saved but progress recording unavailable; inspect the recorded outcome")
	}
	if saveErr != nil {
		return workflowHTTPError(saveErr)
	}
	return c.JSON(http.StatusOK, map[string]any{"id": item.ID, "revision": item.Revision, "outcome": outcome})
}

func agentAllowedWorkflowSteps(steps []workflows.Step) bool {
	for _, step := range steps {
		if step.Kind == workflows.KindHTTP || step.Kind == workflows.KindCode || !agentAllowedWorkflowSteps(step.Then) || !agentAllowedWorkflowSteps(step.Else) {
			return false
		}
	}
	return true
}
