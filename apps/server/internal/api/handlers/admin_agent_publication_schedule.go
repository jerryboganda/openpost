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
)

type agentScheduleBody struct {
	ExpectedRevision int `json:"expected_revision"`
	ScheduledAt time.Time `json:"scheduled_at"`
}

func (h *AdminAgentHandler) scheduleAgentPublication(c echo.Context) error {
	operation := "publication-schedule"
	if strings.HasSuffix(c.Path(), "/reschedule") { operation = "publication-reschedule" }
	run, workspaceID, err := h.readableRun(c.Request().Context(), c.Param("run_id"))
	if err != nil { return err }
	token := strings.TrimPrefix(c.Request().Header.Get(echo.HeaderAuthorization), "Bearer ")
	if token == c.Request().Header.Get(echo.HeaderAuthorization) || adminagent.VerifyPublicationWriteCapability(h.callbackSecret, token, *run, workspaceID, operation, time.Now()) != nil { return echo.NewHTTPError(http.StatusUnauthorized, "publication scheduling capability required") }
	if !explicitPublicationIntent(run.Prompt, operation) { return echo.NewHTTPError(http.StatusForbidden, "publication scheduling was not requested") }
	if err := h.authorizeWorkflowEditor(c.Request().Context(), run, workspaceID); err != nil { return err }
	if h.publications == nil { return echo.NewHTTPError(http.StatusServiceUnavailable, "publication service unavailable") }
	key := c.Request().Header.Get("Idempotency-Key")
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{8,100}$`).MatchString(key) { return echo.NewHTTPError(http.StatusBadRequest, "stable idempotency key required") }
	raw, err := io.ReadAll(http.MaxBytesReader(c.Response(), c.Request().Body, 4096))
	if err != nil { return echo.NewHTTPError(http.StatusRequestEntityTooLarge, "schedule request too large") }
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var input agentScheduleBody
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ExpectedRevision < 1 || input.ScheduledAt.IsZero() { return echo.NewHTTPError(http.StatusBadRequest, "scheduled_at and expected_revision required") }
	ctx := c.Request().Context()
	digest := sha256.Sum256(append([]byte(operation+"\n"+c.Param("id")+"\n"), raw...))
	record := agentPublicationAction{RunID: run.ID, ActionKey: key, RequestHash: hex.EncodeToString(digest[:]), State: "pending"}
	result, err := h.db.NewInsert().Model(&record).On("CONFLICT (run_id, action_key) DO NOTHING").Exec(ctx)
	if err != nil { return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable") }
	affected, err := result.RowsAffected()
	if err != nil { return echo.NewHTTPError(http.StatusInternalServerError, "action reservation unavailable") }
	if affected == 0 {
		var prior agentPublicationAction
		if err := h.db.NewSelect().Model(&prior).Where("run_id = ? AND action_key = ?", run.ID, key).Scan(ctx); err != nil { return echo.NewHTTPError(http.StatusInternalServerError, "action unavailable") }
		if prior.RequestHash != record.RequestHash { return echo.NewHTTPError(http.StatusConflict, "idempotency key already used for another request") }
		if prior.State != "succeeded" { return echo.NewHTTPError(http.StatusConflict, "action pending or failed; inspect recorded outcome") }
		return c.JSON(http.StatusOK, map[string]any{"id": prior.PublicationID, "revision": prior.Revision, "outcome": prior.Outcome, "replayed": true})
	}
	if !input.ScheduledAt.After(time.Now().UTC()) {
		_, _ = h.db.NewUpdate().Model((*agentPublicationAction)(nil)).Set("state = 'failed', outcome = ?", "publication scheduling rejected").Where("run_id = ? AND action_key = ?", run.ID, key).Exec(ctx)
		return echo.NewHTTPError(http.StatusBadRequest, "scheduled_at must be in the future")
	}
	var conversation adminagent.AgentConversation
	if err := h.db.NewSelect().Model(&conversation).Column("created_by_user_id").Where("id = ?", run.ConversationID).Scan(ctx); err != nil { return echo.NewHTTPError(http.StatusInternalServerError, "conversation unavailable") }
	actor := conversation.CreatedByUserID
	application := h.publications.publicationApplication()
	current, saveErr := application.Get(ctx, actor, c.Param("id"))
	if saveErr == nil && current.WorkspaceID != workspaceID { saveErr = echo.NewHTTPError(http.StatusForbidden, "publication is outside selected Workspace") }
	if saveErr == nil {
		if operation == "publication-schedule" {
			if current.Status != "draft" { saveErr = echo.NewHTTPError(http.StatusConflict, "only a stored draft can be scheduled") }
		} else {
			var prior agentPublicationAction
			priorErr := h.db.NewSelect().Model(&prior).Where("run_id = ? AND publication_id = ? AND state = 'succeeded' AND outcome = ?", run.ID, current.ID, "publication scheduled").Limit(1).Scan(ctx)
			if current.Status != "scheduled" || priorErr != nil { saveErr = echo.NewHTTPError(http.StatusConflict, "rescheduling requires this run's prior scheduled publication") }
		}
	}
	if saveErr == nil && current.Revision != input.ExpectedRevision { saveErr = echo.NewHTTPError(http.StatusConflict, "publication revision changed") }
	if saveErr == nil {
		if operation == "publication-schedule" {
			_, saveErr = application.ScheduleDraft(ctx, actor, current.ID, input.ExpectedRevision, input.ScheduledAt)
		} else {
			saveErr = application.Update(ctx, actor, current.ID, publications.PublicationUpdateBody{ExpectedRevision: input.ExpectedRevision, ScheduledAt: &input.ScheduledAt})
		}
	}
	var revision int
	if saveErr == nil {
		updated, getErr := application.Get(ctx, actor, current.ID)
		if getErr != nil { saveErr = getErr } else { revision = updated.Revision }
	}
	state, outcome := "succeeded", "publication scheduled"
	if operation == "publication-reschedule" { outcome = "publication rescheduled" }
	if saveErr != nil { state, outcome = "failed", "publication scheduling rejected" }
	_, recordErr := h.db.NewUpdate().Model((*agentPublicationAction)(nil)).Set("state = ?, outcome = ?, publication_id = ?, revision = ?", state, outcome, c.Param("id"), revision).Where("run_id = ? AND action_key = ? AND state = 'pending'", run.ID, key).Exec(ctx)
	if recordErr != nil { return echo.NewHTTPError(http.StatusInternalServerError, "action outcome unavailable; inspect the publication before retrying") }
	if err := h.service.RecordWorkflowAction(ctx, run.ID, key, state, outcome); err != nil { return echo.NewHTTPError(http.StatusInternalServerError, "action recorded but progress unavailable") }
	if saveErr != nil { return publicationMutationHTTPError(saveErr, "publication scheduling rejected") }
	return c.JSON(http.StatusOK, map[string]any{"id": current.ID, "revision": revision, "outcome": outcome})
}
