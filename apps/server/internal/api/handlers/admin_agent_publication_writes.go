package handlers

import (
	"bytes"
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
	"github.com/openpost/backend/internal/services/publications"
	"github.com/uptrace/bun"
)

var publicationCreateIntent = regexp.MustCompile(`(?i)\b(create|make|add|write|draft|save)\b.{0,120}\b(post|publication|draft)\b`)
var publicationUpdateIntent = regexp.MustCompile(`(?i)\b(update|edit|modify|revise|change|save)\b.{0,120}\b(post|publication|draft)\b`)
var publicationScheduleIntent = regexp.MustCompile(`(?i)\bschedule\b.{0,120}\b(post|publication|draft)\b`)
var publicationRescheduleIntent = regexp.MustCompile(`(?i)\b(reschedule)\b.{0,120}\b(post|publication|draft)\b`)
var publicationPublishNowIntent = regexp.MustCompile(`(?i)\b(?:publish|send|post)\b[^.!?;\n]{0,100}\b(?:posts?|publications?|drafts?)\b[^.!?;\n]{0,100}\b(?:now|immediately|right away|asap)\b|\b(?:now|immediately|right away|asap)\b[ \t]+(?:publish|send|post)\b[^.!?;\n]{0,100}\b(?:posts?|publications?|drafts?)\b`)
var publicationPublishNowExclusion = regexp.MustCompile(`(?i)\b(?:not|never|no|avoid|without|cancel|hold|wait|delay|later|tomorrow|schedule|reschedule|preview|review|explain|simulate|consider|prepare|plan|if|when|once|after|before|pending)\b|\bhow\s+to\b`)

func publicationWriteOperationAllowed(operation string) bool {
	switch operation {
	case "publication-create", "publication-update", "publication-schedule", "publication-reschedule", "publication-publish-now":
		return true
	}
	return false
}

func explicitPublicationIntent(prompt, operation string) bool {
	prompt = strings.TrimSpace(prompt)
	if workflowQuestionIntent.MatchString(prompt) || strings.Contains(strings.ToLower(prompt), "do not ") || strings.Contains(strings.ToLower(prompt), "don't ") {
		return false
	}
	if operation == "publication-create" {
		return publicationCreateIntent.MatchString(prompt)
	}
	if operation == "publication-update" {
		return publicationUpdateIntent.MatchString(prompt)
	}
	if operation == "publication-schedule" {
		return publicationScheduleIntent.MatchString(prompt)
	}
	if operation == "publication-reschedule" {
		return publicationRescheduleIntent.MatchString(prompt)
	}
	if operation == "publication-publish-now" {
		return !strings.Contains(prompt, "?") && !publicationPublishNowExclusion.MatchString(prompt) && publicationPublishNowIntent.MatchString(prompt)
	}
	return false
}

func (h *AdminAgentHandler) issuePublicationWriteCapability(c echo.Context) error {
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
	if json.Unmarshal(body, &input) != nil || !publicationWriteOperationAllowed(input.Operation) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid operation")
	}
	run, workspaceID, err := h.readableRun(c.Request().Context(), c.Param("run_id"))
	if err != nil {
		return err
	}
	if run.GitHubRunID != input.GitHubRunID || !explicitPublicationIntent(run.Prompt, input.Operation) {
		return echo.NewHTTPError(http.StatusForbidden, "publication operation was not requested")
	}
	if err := h.authorizeWorkflowEditor(c.Request().Context(), run, workspaceID); err != nil {
		return err
	}
	token, err := adminagent.IssuePublicationWriteCapability(h.callbackSecret, *run, workspaceID, input.Operation, time.Now())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "capability unavailable")
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusOK, map[string]string{"capability": token})
}

type agentPublicationAction struct {
	bun.BaseModel     `bun:"table:admin_agent_publication_actions"`
	RunID             string `bun:"run_id,pk"`
	ActionKey         string `bun:"action_key,pk"`
	RequestHash       string `bun:"request_hash"`
	State             string `bun:"state"`
	PublicationID     string `bun:"publication_id"`
	Revision          int    `bun:"revision"`
	Outcome           string `bun:"outcome"`
	JobID             string `bun:"job_id"`
	PublicationStatus string `bun:"publication_status"`
	JobStatus         string `bun:"job_status"`
}

func (h *AdminAgentHandler) writePublication(c echo.Context) error {
	operation := "publication-create"
	if c.Param("id") != "" {
		operation = "publication-update"
	}
	run, workspaceID, err := h.readableRun(c.Request().Context(), c.Param("run_id"))
	if err != nil {
		return err
	}
	token := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	if token == c.Request().Header.Get(echo.HeaderAuthorization) || adminagent.VerifyPublicationWriteCapability(h.callbackSecret, token, *run, workspaceID, operation, time.Now()) != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "publication write capability required")
	}
	if !explicitPublicationIntent(run.Prompt, operation) {
		return echo.NewHTTPError(http.StatusForbidden, "publication draft change was not requested")
	}
	if err := h.authorizeWorkflowEditor(c.Request().Context(), run, workspaceID); err != nil {
		return err
	}
	if h.publications == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "publication service unavailable")
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 100 || !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(key) {
		return echo.NewHTTPError(http.StatusBadRequest, "stable idempotency key required")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 256<<10))
	if err != nil {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "publication draft too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var create publications.CreatePublicationBody
	var update publications.PublicationUpdateBody
	if operation == "publication-create" {
		if decoder.Decode(&create) != nil || (create.WorkspaceID != "" && create.WorkspaceID != workspaceID) || create.ScheduledAt != nil || create.RandomDelayMinutes != nil || create.SocialSetID != "" || len(create.SocialAccountIDs) != 0 || create.RepostOverride != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "unsupported or cross-Workspace publication draft")
		}
		create.WorkspaceID = workspaceID
	} else if decoder.Decode(&update) != nil || update.ExpectedRevision < 1 || update.ScheduledAt != nil || update.ClearSchedule || update.RandomDelayMinutes != nil || update.InheritRandomDelay || update.RepostOverride != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported publication draft update")
	}
	// A draft-only action cannot set provider settings or media that require account-specific readiness.
	var renditions []publications.RenditionInput
	if operation == "publication-create" {
		renditions = create.Renditions
	} else {
		renditions = update.Renditions
	}
	for _, rendition := range renditions {
		if rendition.SocialAccountID == "" || rendition.ScheduleOverride != nil || len(rendition.Settings) != 0 || len(rendition.Media) != 0 || len(rendition.Segments) != 0 || rendition.Profile != "" || rendition.OutputProfile != "" || rendition.FormatLocked {
			return echo.NewHTTPError(http.StatusBadRequest, "destination settings and media require editor review")
		}
	}
	if len(create.Media) != 0 || len(create.Segments) != 0 || len(update.Segments) != 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "segment and media edits require editor review")
	}
	digest := sha256.Sum256(append([]byte(operation+"\n"+c.Param("id")+"\n"), raw...))
	hash := hex.EncodeToString(digest[:])
	ctx := c.Request().Context()
	record := agentPublicationAction{RunID: run.ID, ActionKey: key, RequestHash: hash, State: "pending"}
	result, err := h.db.NewInsert().Model(&record).On("CONFLICT (run_id, action_key) DO NOTHING").Exec(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	if affected == 0 {
		var existing agentPublicationAction
		if err := h.db.NewSelect().Model(&existing).Where("run_id = ? AND action_key = ?", run.ID, key).Scan(ctx); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "action unavailable")
		}
		if existing.RequestHash != hash {
			return echo.NewHTTPError(http.StatusConflict, "idempotency key already used for another request")
		}
		if existing.State != "succeeded" {
			return echo.NewHTTPError(http.StatusConflict, "action pending or failed; inspect recorded outcome")
		}
		return c.JSON(http.StatusOK, map[string]any{"id": existing.PublicationID, "revision": existing.Revision, "outcome": existing.Outcome, "replayed": true})
	}
	var conversation adminagent.AgentConversation
	if err := h.db.NewSelect().Model(&conversation).Column("created_by_user_id").Where("id = ?", run.ConversationID).Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "conversation unavailable")
	}
	actor := conversation.CreatedByUserID
	var saved publications.PublicationResponse
	var saveErr error
	if operation == "publication-create" {
		saved, saveErr = h.publications.publicationApplication().Create(ctx, actor, create)
	} else {
		current, loadErr := h.publications.publicationApplication().Get(ctx, actor, c.Param("id"))
		switch {
		case loadErr != nil:
			saveErr = loadErr
		case current.WorkspaceID != workspaceID || current.Status != "draft" || current.ScheduledAt != "":
			saveErr = echo.NewHTTPError(http.StatusConflict, "only an unscheduled draft can be edited")
		default:
			saveErr = h.publications.publicationApplication().Update(ctx, actor, c.Param("id"), update)
			if saveErr == nil {
				saved, saveErr = h.publications.publicationApplication().Get(ctx, actor, c.Param("id"))
			}
		}
	}
	state, outcome := "succeeded", "publication draft saved"
	if saveErr != nil {
		state, outcome = "failed", "publication draft save rejected"
	}
	_, recordErr := h.db.NewUpdate().Model((*agentPublicationAction)(nil)).Set("state = ?, outcome = ?, publication_id = ?, revision = ?", state, outcome, saved.ID, saved.Revision).Where("run_id = ? AND action_key = ? AND state = 'pending'", run.ID, key).Exec(ctx)
	if recordErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action outcome unavailable; inspect the publication before retrying")
	}
	if err := h.service.RecordWorkflowAction(ctx, run.ID, key, state, outcome); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action recorded but progress unavailable")
	}
	if saveErr != nil {
		return publicationMutationHTTPError(saveErr, "publication draft save rejected")
	}
	return c.JSON(http.StatusOK, map[string]any{"id": saved.ID, "revision": saved.Revision, "outcome": outcome})
}
