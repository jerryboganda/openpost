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
	"github.com/openpost/backend/internal/idempotency"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/openpost/backend/internal/services/providerreadiness"
	"github.com/openpost/backend/internal/services/publicationauth"
	"github.com/openpost/backend/internal/services/publications"
)

type agentPublishNowBody struct {
	ExpectedRevision int `json:"expected_revision"`
}

func (h *AdminAgentHandler) publishAgentPublicationNow(c echo.Context) error {
	const operation = "publication-publish-now"
	ctx := c.Request().Context()
	run, workspaceID, err := h.readableRun(ctx, c.Param("run_id"))
	if err != nil {
		return err
	}
	authorization := c.Request().Header.Get(echo.HeaderAuthorization)
	if !strings.HasPrefix(authorization, "Bearer ") || adminagent.VerifyPublicationWriteCapability(h.callbackSecret, strings.TrimPrefix(authorization, "Bearer "), *run, workspaceID, operation, time.Now()) != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "immediate publication capability required")
	}
	if !explicitPublicationIntent(run.Prompt, operation) {
		return echo.NewHTTPError(http.StatusForbidden, "immediate publication was not requested")
	}
	if err := h.authorizeWorkflowEditor(ctx, run, workspaceID); err != nil {
		return err
	}
	if h.publications == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "publication service unavailable")
	}
	key := c.Request().Header.Get("Idempotency-Key")
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{8,100}$`).MatchString(key) {
		return echo.NewHTTPError(http.StatusBadRequest, "stable idempotency key required")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	if err != nil {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "publish request too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input agentPublishNowBody
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ExpectedRevision < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "only expected_revision is accepted")
	}
	digest := sha256.Sum256(append([]byte(operation+"\n"+c.Param("id")+"\n"), raw...))
	record := agentPublicationAction{RunID: run.ID, ActionKey: key, RequestHash: hex.EncodeToString(digest[:]), State: "pending"}
	insert, err := h.db.NewInsert().Model(&record).On("CONFLICT (run_id, action_key) DO NOTHING").Exec(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	affected, err := insert.RowsAffected()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable")
	}
	recoverPending := affected == 0
	if affected == 0 {
		var prior agentPublicationAction
		if err := h.db.NewSelect().Model(&prior).Where("run_id = ? AND action_key = ?", run.ID, key).Scan(ctx); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "action unavailable")
		}
		if prior.RequestHash != record.RequestHash {
			return echo.NewHTTPError(http.StatusConflict, "idempotency key already used for another request")
		}
		if prior.State == "succeeded" {
			return h.agentPublishNowResponse(c, prior, true)
		}
		if prior.State != "pending" {
			return echo.NewHTTPError(http.StatusConflict, "action failed; inspect recorded outcome")
		}
	}
	var conversation adminagent.AgentConversation
	if err := h.db.NewSelect().Model(&conversation).Column("created_by_user_id").Where("id = ?", run.ConversationID).Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "conversation unavailable")
	}
	actor := conversation.CreatedByUserID
	application := h.publications.publicationApplication()
	current, publishErr := application.Get(ctx, actor, c.Param("id"))
	if publishErr == nil && current.WorkspaceID != workspaceID {
		publishErr = echo.NewHTTPError(http.StatusForbidden, "publication is outside selected Workspace")
	}
	if publishErr == nil && !recoverPending && (current.Status != models.PublicationStatusDraft || current.ScheduledAt != "") {
		publishErr = echo.NewHTTPError(http.StatusConflict, "only unscheduled drafts can be published immediately by the agent")
	}
	if publishErr == nil && !recoverPending && current.Revision != input.ExpectedRevision {
		publishErr = echo.NewHTTPError(http.StatusConflict, "publication revision changed")
	}
	if publishErr == nil && !recoverPending && !agentPublishNowTextOnly(current) {
		publishErr = echo.NewHTTPError(http.StatusBadRequest, "agent immediate publishing supports only text-only drafts without advanced settings or media")
	}
	var jobID string
	if publishErr == nil {
		// The application preflights every destination and records a durable authorization and job atomically.
		publicationCtx := publicationauth.WithActor(ctx, publicationauth.Actor{Origin: publicationauth.OriginWorker, UserID: actor})
		queued, _, queueErr := application.PublishNowIdempotent(publicationCtx, actor, current.ID, input.ExpectedRevision, providerreadiness.ExecutionIntentProduction, idempotency.Request{
			PrincipalID: "agent-run:" + run.ID, WorkspaceID: workspaceID, OperationID: operation, Key: key,
			ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
		})
		publishErr = queueErr
		jobID = queued.JobID
	}
	state, outcome := "succeeded", "publication queued for immediate delivery"
	if publishErr != nil {
		state, outcome = "failed", "immediate publication rejected"
	}
	_, recordErr := h.db.NewUpdate().Model((*agentPublicationAction)(nil)).Set("state = ?, outcome = ?, publication_id = ?, revision = ?, job_id = ?, publication_status = ?, job_status = ?", state, outcome, c.Param("id"), input.ExpectedRevision, jobID, func() string {
		if jobID != "" {
			return models.PublicationStatusScheduled
		}
		return ""
	}(), func() string {
		if jobID != "" {
			return "pending"
		}
		return ""
	}()).Where("run_id = ? AND action_key = ? AND state = 'pending'", run.ID, key).Exec(ctx)
	if recordErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action outcome unavailable; inspect publication history before retrying")
	}
	if err := h.service.RecordWorkflowAction(ctx, run.ID, key, state, outcome); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "action recorded but progress unavailable")
	}
	if publishErr != nil {
		return publicationMutationHTTPError(publishErr, "immediate publication rejected")
	}
	record.State, record.Outcome, record.PublicationID, record.Revision, record.JobID = state, outcome, c.Param("id"), input.ExpectedRevision, jobID
	return h.agentPublishNowResponse(c, record, false)
}

func agentPublishNowTextOnly(publication publications.PublicationResponse) bool {
	if len(publication.Media) != 0 || len(publication.Renditions) == 0 || len(publication.Segments) > 1 {
		return false
	}
	for _, segment := range publication.Segments {
		if len(segment.Media) != 0 || len(segment.Settings) != 0 {
			return false
		}
	}
	for _, rendition := range publication.Renditions {
		if len(rendition.Media) != 0 || len(rendition.Settings) != 0 || rendition.ScheduleOverride != "" || rendition.FormatLocked || len(rendition.Segments) > 1 {
			return false
		}
		for _, segment := range rendition.Segments {
			if len(segment.Media) != 0 || len(segment.Settings) != 0 {
				return false
			}
		}
	}
	return true
}

func (h *AdminAgentHandler) agentPublishNowResponse(c echo.Context, action agentPublicationAction, replayed bool) error {
	ctx := c.Request().Context()
	publicationStatus, jobStatus := action.PublicationStatus, action.JobStatus
	if action.PublicationID != "" {
		var publication models.Publication
		if err := h.db.NewSelect().Model(&publication).Column("status").Where("id = ?", action.PublicationID).Scan(ctx); err == nil {
			publicationStatus = publication.Status
		} else {
			publicationStatus = "unavailable"
		}
	}
	if action.JobID != "" {
		var job models.Job
		if err := h.db.NewSelect().Model(&job).Column("status").Where("id = ?", action.JobID).Scan(ctx); err == nil {
			jobStatus = job.Status
		} else {
			jobStatus = "unavailable"
		}
	}
	c.Response().Header().Set(echo.HeaderCacheControl, "no-store")
	return c.JSON(http.StatusOK, map[string]any{"id": action.PublicationID, "revision": action.Revision, "job_id": action.JobID, "publication_status": publicationStatus, "job_status": jobStatus, "outcome": action.Outcome, "replayed": replayed})
}
