package adminagent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
)

type fakeDispatcher struct {
	dispatched  []DispatchRequest
	cancelled   []string
	dispatchErr error
	cancelErr   error
}

func (f *fakeDispatcher) Dispatch(_ context.Context, request DispatchRequest) (DispatchResult, error) {
	f.dispatched = append(f.dispatched, request)
	if f.dispatchErr != nil {
		return DispatchResult{}, f.dispatchErr
	}
	return DispatchResult{Ref: "main", URL: "https://github.com/example/actions"}, nil
}

func (f *fakeDispatcher) Cancel(_ context.Context, run AgentRun) error {
	f.cancelled = append(f.cancelled, run.ID)
	return f.cancelErr
}

func adminAgentTestDB(t *testing.T) *bun.DB {
	t.Helper()
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	db := bun.NewDB(raw, sqlitedialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		"CREATE TABLE users (id TEXT PRIMARY KEY, email TEXT NOT NULL DEFAULT '', is_admin BOOLEAN NOT NULL DEFAULT 0)",
		"CREATE TABLE workspaces (id TEXT PRIMARY KEY)",
		"INSERT INTO users (id, email, is_admin) VALUES ('admin', 'admin@example.com', 1)",
		"INSERT INTO users (id, email, is_admin) VALUES ('other', 'other@example.com', 1)",
		"INSERT INTO workspaces (id) VALUES ('workspace')",
	} {
		_, err := db.Exec(statement)
		require.NoError(t, err)
	}
	for _, name := range []string{"176_admin_agent.sql", "177_admin_agent_chat.sql"} {
		migration, err := os.ReadFile("../../database/migrations/" + name)
		require.NoError(t, err)
		_, err = db.Exec(string(migration))
		require.NoError(t, err)
	}
	return db
}

func startRun(ctx context.Context, service *Service, conversationID, prompt string) (*AgentRun, error) {
	return service.StartRun(ctx, StartRunInput{ConversationID: conversationID, UserID: "admin", Prompt: prompt})
}

func TestConversationRunAndCallbackLifecycle(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)

	conversation, err := service.CreateConversation(ctx, "admin", "workspace", "Deploy check", EngineOpenCode)
	require.NoError(t, err)

	run, err := startRun(ctx, service, conversation.ID, "Inspect the deployment and report readiness.")
	require.NoError(t, err)
	require.Equal(t, string(RunDispatched), run.Status)
	require.Len(t, dispatcher.dispatched, 1)
	require.Equal(t, EngineOpenCode, dispatcher.dispatched[0].Engine)

	_, err = service.ApplyCallback(ctx, AgentCallback{
		RunID:  run.ID,
		Status: string(RunRunning),
		Events: []CallbackEvent{{Key: "phase-setup", Kind: "progress", AgentMessage: "Installing the engine"}},
	})
	require.NoError(t, err)

	_, err = service.ApplyCallback(ctx, AgentCallback{
		RunID:        run.ID,
		Status:       string(RunSucceeded),
		Summary:      "Verified readiness and reported the revision.",
		Branch:       "agent/check",
		BaseRevision: "0123456789abcdef0123456789abcdef01234567",
		Events:       []CallbackEvent{{Key: "phase-setup", Kind: "progress", AgentMessage: "duplicate must be ignored"}},
	})
	require.NoError(t, err)

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Messages, 2)
	require.Equal(t, "user", view.Messages[0].Role)
	require.Equal(t, "assistant", view.Messages[1].Role)
	require.Equal(t, "Verified readiness and reported the revision.", view.Messages[1].Content)
	require.Len(t, view.Runs, 1)
	require.Equal(t, string(RunSucceeded), view.Runs[0].Status)
	require.Equal(t, "agent/check", view.Runs[0].Branch)
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", view.Runs[0].BaseRevision)
	require.NotNil(t, view.Runs[0].FinishedAt)

	var setupEvents int
	for _, event := range view.Events {
		if event.EventKey == "phase-setup" {
			setupEvents++
			require.Equal(t, "Installing the engine", event.AgentMessage)
		}
	}
	require.Equal(t, 1, setupEvents, "duplicate callback keys must be idempotent")
}

func TestMissingDispatchIsReportedTruthfully(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	for _, engine := range service.Engines() {
		require.False(t, engine.Available)
		require.NotEmpty(t, engine.Reason)
	}
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Change the login copy.")
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), run.Status)
	require.Equal(t, "not_configured", run.FailureCode)
	require.Contains(t, run.FailureMessage, "not configured")
	require.Nil(t, run.StartedAt)
}

func TestDispatchFailureDoesNotClaimSuccess(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{dispatchErr: errors.New("secret upstream failure")}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineClaude)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Run the tests.")
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), run.Status)
	require.Equal(t, "dispatch_failed", run.FailureCode)
	require.NotContains(t, run.FailureMessage, "secret upstream failure")
}

func TestCancelStopsFurtherStatusChanges(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Do work.")
	require.NoError(t, err)

	cancelled, err := service.CancelRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunCanceled), cancelled.Status)
	require.Equal(t, []string{run.ID}, dispatcher.cancelled)

	afterCancel, err := service.ApplyCallback(ctx, AgentCallback{
		RunID:   run.ID,
		Status:  string(RunSucceeded),
		Summary: "Late success must not override the administrator.",
	})
	require.NoError(t, err)
	require.Equal(t, string(RunCanceled), afterCancel.Status)

	_, err = service.CancelRun(ctx, run.ID)
	require.ErrorIs(t, err, ErrRunTerminal)
}

func TestCancellationFailureKeepsRunActive(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{cancelErr: errors.New("GitHub run not visible yet")}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect the code.")
	require.NoError(t, err)

	_, err = service.CancelRun(ctx, run.ID)
	require.ErrorContains(t, err, "not visible yet")
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunDispatched), stored.Status)
	require.Nil(t, stored.FinishedAt)
}

func TestCompletedRunIgnoresDelayedCallbacks(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineClaude)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Check the change.")
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunSucceeded), Summary: "Verified."})
	require.NoError(t, err)
	for _, status := range []RunStatus{RunRunning, RunFailed} {
		late, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(status), Summary: "Stale result."})
		require.NoError(t, err)
		require.Equal(t, string(RunSucceeded), late.Status)
		require.Equal(t, "Verified.", late.Summary)
	}
}

func TestInterruptedRunsAreRecoverableAndCorrectable(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Deploy the change.")
	require.NoError(t, err)

	count, err := service.FailInterrupted(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), stored.Status)
	require.Equal(t, "interrupted", stored.FailureCode)
	require.Contains(t, stored.FailureMessage, "outcome is unknown")

	corrected, err := service.ApplyCallback(ctx, AgentCallback{
		RunID:   run.ID,
		Status:  string(RunSucceeded),
		Summary: "The engine had already finished after the restart.",
	})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), corrected.Status)
}

func TestCallbackSignatureBindsTimestampAndBody(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	timestamp := now.Format(time.RFC3339)
	body := []byte(`{"run_id":"run-1","status":"succeeded"}`)
	signature := SignCallback("shared-secret", timestamp, body)
	require.NoError(t, VerifyCallback("shared-secret", timestamp, signature, body, now))
	require.ErrorIs(t, VerifyCallback("other-secret", timestamp, signature, body, now), ErrInvalidCallback)
	require.ErrorIs(t, VerifyCallback("shared-secret", timestamp, signature, []byte(`{"run_id":"run-2"}`), now), ErrInvalidCallback)
	require.ErrorIs(t, VerifyCallback("shared-secret", now.Add(-10*time.Minute).Format(time.RFC3339), signature, body, now), ErrInvalidCallback)
	require.ErrorIs(t, VerifyCallback("", timestamp, signature, body, now), ErrInvalidCallback)
}

func TestPromptEngineAndOptionValidation(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	_, err := service.CreateConversation(ctx, "admin", "", "", Engine("shell"))
	require.ErrorIs(t, err, ErrInvalidEngine)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	_, err = startRun(ctx, service, conversation.ID, "   ")
	require.ErrorIs(t, err, ErrInvalidPrompt)

	_, err = service.StartRun(ctx, StartRunInput{ConversationID: conversation.ID, UserID: "admin", Prompt: "x", Access: "root"})
	require.ErrorIs(t, err, ErrInvalidAccess)
}

func TestConversationsAreTitledFromTheFirstMessageAndDeletable(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Why did the nightly publish job fail?\nPlease check the logs too.")
	require.NoError(t, err)

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, "Why did the nightly publish job fail?", view.Title)

	require.ErrorIs(t, service.DeleteConversation(ctx, conversation.ID), ErrRunActive, "a task in progress must be stopped first")
	_, err = service.CancelRun(ctx, run.ID)
	require.NoError(t, err)
	require.NoError(t, service.DeleteConversation(ctx, conversation.ID))
	_, err = service.GetConversation(ctx, conversation.ID)
	require.ErrorIs(t, err, ErrConversationNotFound)
	require.ErrorIs(t, service.DeleteConversation(ctx, conversation.ID), ErrConversationNotFound)

	var leftovers int
	require.NoError(t, service.db.NewSelect().Table("admin_agent_events").ColumnExpr("COUNT(*)").Scan(ctx, &leftovers))
	require.Zero(t, leftovers, "events must be removed with their conversation")
}
