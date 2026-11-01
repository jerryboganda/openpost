package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/platform"
	"github.com/uptrace/bun"
)

// Read only saved analytics state. These status codes and metrics are the last
// persisted observations, and a disconnected account may retain older values.
type agentAnalyticsSnapshot struct {
	SubjectType      string                                      `json:"subject_type"`
	SubjectID        string                                      `json:"subject_id"`
	SocialAccountID  string                                      `json:"social_account_id"`
	Platform         string                                      `json:"platform"`
	ConnectionStatus string                                      `json:"connection_status"`
	CollectionStatus string                                      `json:"collection_status"`
	ErrorCode        string                                      `json:"error_code,omitempty"`
	Metrics          map[string]int64                            `json:"metrics"`
	MetricMetadata   map[string]platform.AnalyticsMetricMetadata `json:"metric_metadata"`
	LastSyncedAt     *time.Time                                  `json:"last_synced_at,omitempty"`
	NextSyncAt       *time.Time                                  `json:"next_sync_at,omitempty"`
	UpdatedAt        time.Time                                   `json:"updated_at"`
}

func (h *AdminAgentHandler) readAnalytics(c echo.Context) error {
	workspaceID, err := h.authorizeRead(c, "analytics")
	if err != nil {
		return err
	}
	cursor, err := agentReadCursor(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var states []models.AnalyticsSyncState
	query := h.db.NewSelect().Model(&states).
		Column("id", "subject_type", "subject_id", "social_account_id", "platform", "status", "error_code", "metrics_json", "metric_metadata_json", "last_success_at", "next_sync_at", "updated_at").
		Where("workspace_id = ?", workspaceID).
		OrderExpr("updated_at DESC, id DESC").Limit(agentReadPageSize + 1)
	if cursor != nil {
		query = query.Where("(updated_at < ? OR (updated_at = ? AND id < ?))", cursor.Timestamp, cursor.Timestamp, cursor.ID)
	}
	if err := query.Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "analytics unavailable")
	}
	hasMore := len(states) > agentReadPageSize
	if hasMore {
		states = states[:agentReadPageSize]
	}
	accountIDs := make([]string, 0, len(states))
	for _, state := range states {
		accountIDs = append(accountIDs, state.SocialAccountID)
	}
	connected := map[string]bool{}
	if len(accountIDs) > 0 {
		var accounts []models.SocialAccount
		if err := h.db.NewSelect().Model(&accounts).Column("id", "is_active").
			Where("workspace_id = ? AND id IN (?)", workspaceID, bun.In(accountIDs)).Scan(ctx); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "analytics accounts unavailable")
		}
		for _, account := range accounts {
			connected[account.ID] = account.IsActive
		}
	}
	items := make([]agentAnalyticsSnapshot, 0, len(states))
	for _, state := range states {
		// Stored normalized metrics are integers; never forward arbitrary JSON or
		// provider messages. The source field in metric metadata is free text.
		if len(state.MetricsJSON) > 16<<10 || len(state.MetricMetadataJSON) > 32<<10 {
			return echo.NewHTTPError(http.StatusInternalServerError, "analytics measurement unavailable")
		}
		metrics := map[string]int64{}
		metadata := map[string]platform.AnalyticsMetricMetadata{}
		if json.Unmarshal([]byte(state.MetricsJSON), &metrics) != nil ||
			json.Unmarshal([]byte(state.MetricMetadataJSON), &metadata) != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "analytics measurement unavailable")
		}
		for key, meta := range metadata {
			meta.Source = ""
			metadata[key] = meta
		}
		if metrics == nil {
			metrics = map[string]int64{}
		}
		if metadata == nil {
			metadata = map[string]platform.AnalyticsMetricMetadata{}
		}
		item := agentAnalyticsSnapshot{
			SubjectType: state.SubjectType, SubjectID: state.SubjectID, SocialAccountID: state.SocialAccountID,
			Platform: state.Platform, CollectionStatus: state.Status, ErrorCode: truncateAgentField(state.ErrorCode),
			Metrics: metrics, MetricMetadata: metadata, UpdatedAt: state.UpdatedAt,
			ConnectionStatus: "not_connected",
		}
		if connected[state.SocialAccountID] {
			item.ConnectionStatus = "connected"
		}
		if !state.LastSuccessAt.IsZero() {
			item.LastSyncedAt = &state.LastSuccessAt
		}
		if !state.NextSyncAt.IsZero() {
			item.NextSyncAt = &state.NextSyncAt
		}
		items = append(items, item)
	}
	next := ""
	if hasMore {
		last := states[len(states)-1]
		next = encodeTimestampIDCursor(last.UpdatedAt, last.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"workspace_id": workspaceID, "source": "stored_latest_sync_state", "snapshots": items,
		"has_more": hasMore, "next_cursor": next,
	})
}

// Decode only workflow topology. Source settings, authored input values,
// credential references and run payloads are intentionally excluded.
type agentWorkflowStep struct {
	ID   string              `json:"id"`
	Kind string              `json:"kind"`
	Name string              `json:"name,omitempty"`
	Then []agentWorkflowStep `json:"then,omitempty"`
	Else []agentWorkflowStep `json:"else,omitempty"`
}

type agentWorkflowDefinition struct {
	Schema int `json:"schema"`
	Source struct {
		Kind string `json:"kind"`
	} `json:"source"`
	Steps     []agentWorkflowStep `json:"steps"`
	Truncated bool                `json:"truncated,omitempty"`
}

func agentSafeWorkflowDefinition(raw string) (agentWorkflowDefinition, error) {
	var outline agentWorkflowDefinition
	if len(raw) > 256<<10 {
		return agentWorkflowDefinition{Truncated: true, Steps: []agentWorkflowStep{}}, nil
	}
	if err := json.Unmarshal([]byte(raw), &outline); err != nil {
		return outline, err
	}
	remaining := 40
	outline.Steps = agentLimitWorkflowSteps(outline.Steps, &remaining, &outline.Truncated, 0)
	return outline, nil
}

func agentLimitWorkflowSteps(steps []agentWorkflowStep, remaining *int, truncated *bool, depth int) []agentWorkflowStep {
	result := make([]agentWorkflowStep, 0, min(len(steps), *remaining))
	for _, step := range steps {
		if *remaining == 0 || depth >= 10 {
			*truncated = true
			break
		}
		*remaining = *remaining - 1
		step.Name = truncateAgentField(step.Name)
		step.Then = agentLimitWorkflowSteps(step.Then, remaining, truncated, depth+1)
		step.Else = agentLimitWorkflowSteps(step.Else, remaining, truncated, depth+1)
		result = append(result, step)
	}
	return result
}

type agentWorkflowRow struct {
	bun.BaseModel     `bun:"table:workflows"`
	ID                string
	Name              string
	Revision          int
	PublishedRevision int
	Enabled           bool
	DraftJSON         string
	PublishedJSON     string
	UpdatedAt         time.Time
}

type agentWorkflow struct {
	ID                  string                   `json:"id"`
	Name                string                   `json:"name"`
	Revision            int                      `json:"revision"`
	PublishedRevision   int                      `json:"published_revision"`
	Enabled             bool                     `json:"enabled"`
	DraftDefinition     agentWorkflowDefinition  `json:"draft_definition"`
	PublishedDefinition *agentWorkflowDefinition `json:"published_definition,omitempty"`
	UpdatedAt           time.Time                `json:"updated_at"`
}

func (h *AdminAgentHandler) readWorkflows(c echo.Context) error {
	workspaceID, err := h.authorizeRead(c, "workflows")
	if err != nil {
		return err
	}
	cursor, err := agentReadCursor(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var rows []agentWorkflowRow
	query := h.db.NewSelect().Model(&rows).
		Column("id", "name", "revision", "published_revision", "enabled", "draft_json", "published_json", "updated_at").
		Where("workspace_id = ?", workspaceID).
		OrderExpr("updated_at DESC, id DESC").Limit(agentReadPageSize + 1)
	if cursor != nil {
		query = query.Where("(updated_at < ? OR (updated_at = ? AND id < ?))", cursor.Timestamp, cursor.Timestamp, cursor.ID)
	}
	if err := query.Scan(ctx); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "workflows unavailable")
	}
	hasMore := len(rows) > agentReadPageSize
	if hasMore {
		rows = rows[:agentReadPageSize]
	}
	items := make([]agentWorkflow, 0, len(rows))
	for _, row := range rows {
		draft, err := agentSafeWorkflowDefinition(row.DraftJSON)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "workflow definition unavailable")
		}
		item := agentWorkflow{
			ID: row.ID, Name: truncateAgentField(row.Name), Revision: row.Revision,
			PublishedRevision: row.PublishedRevision, Enabled: row.Enabled,
			DraftDefinition: draft, UpdatedAt: row.UpdatedAt,
		}
		if row.PublishedRevision > 0 {
			published, err := agentSafeWorkflowDefinition(row.PublishedJSON)
			if err != nil {
				return echo.NewHTTPError(http.StatusInternalServerError, "published workflow unavailable")
			}
			item.PublishedDefinition = &published
		}
		items = append(items, item)
	}
	next := ""
	if hasMore {
		last := rows[len(rows)-1]
		next = encodeTimestampIDCursor(last.UpdatedAt, last.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"workspace_id": workspaceID, "definition_scope": "structure_only", "workflows": items,
		"has_more": hasMore, "next_cursor": next,
	})
}

// Workflow runs may contain OAuth authority and arbitrary source/step output.
// Select only public run status fields, never definition or result payloads.
type agentWorkflowRun struct {
	bun.BaseModel    `bun:"table:workflow_runs"`
	ID               string     `json:"id"`
	WorkflowID       string     `json:"workflow_id"`
	WorkflowName     string     `json:"workflow_name"`
	WorkflowRevision int        `json:"workflow_revision"`
	Mode             string     `json:"mode"`
	State            string     `json:"state"`
	Revision         int        `json:"revision"`
	CurrentStepID    string     `json:"current_step_id,omitempty"`
	WakeAt           *time.Time `json:"wake_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (h *AdminAgentHandler) readWorkflowRuns(c echo.Context) error {
	workspaceID, err := h.authorizeRead(c, "workflow-runs")
	if err != nil {
		return err
	}
	cursor, err := agentReadCursor(c)
	if err != nil {
		return err
	}
	var runs []agentWorkflowRun
	query := h.db.NewSelect().Model(&runs).
		Column("id", "workflow_id", "workflow_name", "workflow_revision", "mode", "state", "revision", "current_step_id", "wake_at", "created_at", "updated_at").
		Where("workspace_id = ?", workspaceID).
		OrderExpr("created_at DESC, id DESC").Limit(agentReadPageSize + 1)
	if cursor != nil {
		query = query.Where("(created_at < ? OR (created_at = ? AND id < ?))", cursor.Timestamp, cursor.Timestamp, cursor.ID)
	}
	if err := query.Scan(c.Request().Context()); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "workflow runs unavailable")
	}
	hasMore := len(runs) > agentReadPageSize
	if hasMore {
		runs = runs[:agentReadPageSize]
	}
	for index := range runs {
		runs[index].WorkflowName = truncateAgentField(runs[index].WorkflowName)
	}
	next := ""
	if hasMore {
		last := runs[len(runs)-1]
		next = encodeTimestampIDCursor(last.CreatedAt, last.ID)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"workspace_id": workspaceID, "runs": runs, "has_more": hasMore, "next_cursor": next,
	})
}
