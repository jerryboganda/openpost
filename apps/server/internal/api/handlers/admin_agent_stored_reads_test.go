package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/models"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/stretchr/testify/require"
)

func TestAgentWorkflowOutlineDropsInputsAndLimitsNodes(t *testing.T) {
	raw := `{"schema":1,"source":{"kind":"manual","url":"https://private.example/secret","connection_id":"credential"},"steps":[{"id":"one","kind":"http_request","inputs":{"api_key":{"literal":"top-secret"}},"then":[{"id":"two","kind":"text","inputs":{"text":{"literal":"nested-secret"}}}]}]}`
	outline, err := agentSafeWorkflowDefinition(raw)
	require.NoError(t, err)
	encoded, err := json.Marshal(outline)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"id":"two"`)
	require.NotContains(t, string(encoded), "secret")
	require.NotContains(t, string(encoded), "connection_id")
	require.NotContains(t, string(encoded), "url")
	long := `{"schema":1,"source":{"kind":"manual"},"steps":[` + strings.Repeat(`{"id":"a","kind":"text"},`, 50) + `{"id":"last","kind":"text"}]}`
	limited, err := agentSafeWorkflowDefinition(long)
	require.NoError(t, err)
	require.Len(t, limited.Steps, 40)
	require.True(t, limited.Truncated)
}

func TestAgentStoredReadsBoundWorkspaceAndHidePayloads(t *testing.T) {
	ctx := t.Context()
	db := createHandlerTestDB(t, (*models.User)(nil), (*models.Workspace)(nil), (*models.WorkspaceMember)(nil), (*models.SocialAccount)(nil), (*models.AnalyticsSyncState)(nil))
	_, err := db.NewInsert().Model(&models.User{ID: "user-1", Email: "admin@example.com", IsAdmin: true}).Exec(ctx)
	require.NoError(t, err)
	for _, workspace := range []string{"selected", "other"} {
		_, err = db.NewInsert().Model(&models.Workspace{ID: workspace, Name: workspace}).Exec(ctx)
		require.NoError(t, err)
	}
	_, err = db.NewInsert().Model(&models.WorkspaceMember{WorkspaceID: "selected", UserID: "user-1", Role: models.WorkspaceRoleViewer, Status: models.WorkspaceMemberStatusActive}).Exec(ctx)
	require.NoError(t, err)
	for _, account := range []models.SocialAccount{
		{ID: "connected", WorkspaceID: "selected", Slug: "connected", Platform: "bluesky", AccountID: "connected", AccessTokenEnc: []byte("test"), IsActive: true},
		{ID: "disconnected", WorkspaceID: "selected", Slug: "disconnected", Platform: "x", AccountID: "disconnected", AccessTokenEnc: []byte("test"), IsActive: false},
		{ID: "foreign", WorkspaceID: "other", Slug: "foreign", Platform: "x", AccountID: "foreign", AccessTokenEnc: []byte("test"), IsActive: true},
	} {
		_, err = db.NewInsert().Model(&account).Exec(ctx)
		require.NoError(t, err)
	}
	for _, state := range []models.AnalyticsSyncState{
		{ID: "account:connected", WorkspaceID: "selected", SubjectType: "account", SubjectID: "connected", SocialAccountID: "connected", Platform: "bluesky", Status: "unsupported", MetricsJSON: `{}`, MetricMetadataJSON: `{}`, UpdatedAt: time.Now().UTC()},
		{ID: "account:disconnected", WorkspaceID: "selected", SubjectType: "account", SubjectID: "disconnected", SocialAccountID: "disconnected", Platform: "x", Status: "ok", MetricsJSON: `{"followers":4}`, MetricMetadataJSON: `{}`, UpdatedAt: time.Now().UTC()},
		{ID: "account:foreign", WorkspaceID: "other", SubjectType: "account", SubjectID: "foreign", SocialAccountID: "foreign", Platform: "x", Status: "ok", MetricsJSON: `{}`, MetricMetadataJSON: `{}`, UpdatedAt: time.Now().UTC()},
	} {
		_, err = db.NewInsert().Model(&state).Exec(ctx)
		require.NoError(t, err)
	}
	for _, schema := range []string{"145_workflows.sql", "176_admin_agent.sql", "177_admin_agent_chat.sql", "182_admin_agent_one_active_run.sql", "178_admin_agent_github_run_id.sql"} {
		data, readErr := os.ReadFile("../../database/migrations/" + schema)
		require.NoError(t, readErr)
		_, err = db.Exec(string(data))
		require.NoError(t, err)
	}
	now := time.Now().UTC()
	_, err = db.NewRaw(`INSERT INTO workflows (id, workspace_id, name, draft_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"flow-selected", "selected", "Selected", `{"schema":1,"source":{"kind":"manual","connection_id":"secret"},"steps":[]}`, now, now).Exec(ctx)
	require.NoError(t, err)
	_, err = db.NewRaw(`INSERT INTO workflows (id, workspace_id, name, draft_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"flow-other", "other", "Foreign", `{}`, now, now).Exec(ctx)
	require.NoError(t, err)
	_, err = db.NewRaw(`INSERT INTO workflow_runs (id, workflow_id, workspace_id, workflow_name, workflow_revision, mode, state, definition_json, authority_json, source_json, remaining_json, results_json, error, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run-selected", "flow-selected", "selected", "Selected", 1, "live", "failed", `{}`, `{"secret":"token"}`, `{"secret":"token"}`, `[]`, `[{"output":"secret"}]`, "secret", now, now).Exec(ctx)
	require.NoError(t, err)
	_, err = db.NewRaw(`INSERT INTO workflow_runs (id, workflow_id, workspace_id, workflow_name, workflow_revision, mode, state, definition_json, authority_json, source_json, remaining_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run-foreign", "flow-other", "other", "Foreign", 1, "live", "failed", `{}`, `{}`, `{}`, `[]`, now, now).Exec(ctx)
	require.NoError(t, err)
	service := adminagent.NewService(db, &agentTestDispatcher{})
	conversation, err := service.CreateConversation(ctx, "user-1", "selected", "", adminagent.EngineCodex)
	require.NoError(t, err)
	run, err := service.StartRun(ctx, adminagent.StartRunInput{ConversationID: conversation.ID, UserID: "user-1", Prompt: "inspect stored state"})
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, adminagent.AgentCallback{RunID: run.ID, GitHubRunID: ptrInt64(123), Status: "running"})
	require.NoError(t, err)
	handler := NewAdminAgentHandler(service, db, browserSessionTestAuthenticator(), "secret")
	e := echo.New()
	handler.RegisterEchoRoutes(e)
	for _, operation := range []string{"analytics", "workflows", "workflow-runs"} {
		capability, err := adminagent.IssueReadCapability("secret", adminagent.AgentRun{ID: run.ID, GitHubRunID: 123}, "selected", operation, time.Now())
		require.NoError(t, err)
		path := "/api/v1/admin/agent/runs/" + run.ID + "/read/" + operation
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+capability)
		response := httptest.NewRecorder()
		e.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.NotContains(t, response.Body.String(), "foreign")
		require.NotContains(t, response.Body.String(), "secret")
		if operation == "analytics" {
			require.Contains(t, response.Body.String(), `"collection_status":"unsupported"`)
			require.Contains(t, response.Body.String(), `"connection_status":"not_connected"`)
		}
	}
}
