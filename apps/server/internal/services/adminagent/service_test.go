package adminagent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/openpost/backend/internal/ai"
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
		"CREATE TABLE users (id TEXT PRIMARY KEY)",
		"CREATE TABLE workspaces (id TEXT PRIMARY KEY)",
		"INSERT INTO users (id) VALUES ('admin')",
		"INSERT INTO workspaces (id) VALUES ('workspace')",
	} {
		_, err := db.Exec(statement)
		require.NoError(t, err)
	}
	migration, err := os.ReadFile("../../database/migrations/176_admin_agent.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	return db
}

func TestConversationRunAndCallbackLifecycle(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)

	conversation, err := service.CreateConversation(ctx, "admin", "workspace", "Deploy check", EngineOpenCode)
	require.NoError(t, err)

	run, err := service.StartRun(ctx, conversation.ID, "Inspect the deployment and report readiness.")
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
	run, err := service.StartRun(ctx, conversation.ID, "Change the login copy.")
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
	run, err := service.StartRun(ctx, conversation.ID, "AgentRun the tests.")
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
	run, err := service.StartRun(ctx, conversation.ID, "Do work.")
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

func TestInterruptedRunsAreRecoverableAndCorrectable(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := service.StartRun(ctx, conversation.ID, "Deploy the change.")
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

func TestPromptAndEngineValidation(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	_, err := service.CreateConversation(ctx, "admin", "", "", Engine("shell"))
	require.ErrorIs(t, err, ErrInvalidEngine)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	_, err = service.StartRun(ctx, conversation.ID, "   ")
	require.ErrorIs(t, err, ErrInvalidPrompt)
}

type fakeAssistantGenerator struct {
	text string
	err  error
}

func (f *fakeAssistantGenerator) Generate(_ context.Context, _ ai.GenerateRequest) (ai.GenerateResult, error) {
	if f.err != nil {
		return ai.GenerateResult{}, f.err
	}
	return ai.GenerateResult{Text: f.text, Model: "opencode-test"}, nil
}

func waitForTerminalRun(t *testing.T, service *Service, runID string) *AgentRun {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.GetRun(context.Background(), runID)
		require.NoError(t, err)
		if RunStatus(run.Status).Terminal() {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", runID)
	return nil
}

func TestBuiltInAssistantAnswersAndPersistsHistory(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	service.SetAssistant(&fakeAssistantGenerator{text: "The instance has three publications."}, "opencode-test")

	engines := service.Engines()
	require.True(t, engines[0].Available)
	require.Equal(t, EngineAssistant, engines[0].ID)

	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineAssistant)
	require.NoError(t, err)
	run, err := service.StartRun(ctx, conversation.ID, "How many publications exist?")
	require.NoError(t, err)
	require.Equal(t, string(RunRunning), run.Status)

	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunSucceeded), finished.Status)
	require.Contains(t, finished.Summary, "three publications")

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Messages, 2)
	require.Equal(t, "assistant", view.Messages[1].Role)
	require.Contains(t, view.Messages[1].Content, "three publications")
}

func TestBuiltInAssistantFailureIsTruthful(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	service.SetAssistant(&fakeAssistantGenerator{
		err: &ai.ProviderError{Provider: "OpenCode", StatusCode: 401},
	}, "opencode-test")
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineAssistant)
	require.NoError(t, err)
	run, err := service.StartRun(ctx, conversation.ID, "Hello")
	require.NoError(t, err)
	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunFailed), finished.Status)
	require.Equal(t, "invalid_credentials", finished.FailureCode)
	require.NotContains(t, finished.FailureMessage, "sk-")
}

func TestBuiltInAssistantMissingProviderIsReported(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineAssistant)
	require.NoError(t, err)
	run, err := service.StartRun(ctx, conversation.ID, "Hello")
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), run.Status)
	require.Equal(t, "not_configured", run.FailureCode)
}
