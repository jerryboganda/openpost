package adminagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
	onDispatch  func(DispatchRequest)
	onCancel    func(AgentRun)
	lookup      func(AgentRun) (GitHubRunState, error)
}

func (f *fakeDispatcher) Dispatch(_ context.Context, request DispatchRequest) (DispatchResult, error) {
	f.dispatched = append(f.dispatched, request)
	if f.onDispatch != nil {
		f.onDispatch(request)
	}
	if f.dispatchErr != nil {
		return DispatchResult{}, f.dispatchErr
	}
	return DispatchResult{Ref: "main", URL: "https://github.com/example/actions"}, nil
}

func (f *fakeDispatcher) Cancel(_ context.Context, run AgentRun) error {
	f.cancelled = append(f.cancelled, run.ID)
	if f.onCancel != nil {
		f.onCancel(run)
	}
	return f.cancelErr
}

func (f *fakeDispatcher) LookupRun(_ context.Context, run AgentRun) (GitHubRunState, error) {
	if f.lookup == nil {
		return GitHubRunState{}, ErrGitHubRunUnidentified
	}
	return f.lookup(run)
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
	for _, name := range []string{"176_admin_agent.sql", "177_admin_agent_chat.sql", "182_admin_agent_one_active_run.sql", "178_admin_agent_github_run_id.sql"} {
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

func TestOnlyOneActiveRunPerConversation(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)

	type result struct {
		run *AgentRun
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			run, err := startRun(ctx, service, conversation.ID, "Inspect the repository.")
			results <- result{run, err}
		}()
	}
	close(start)

	var active *AgentRun
	var conflicts int
	for i := 0; i < 2; i++ {
		result := <-results
		if errors.Is(result.err, ErrRunActive) {
			conflicts++
		} else {
			require.NoError(t, result.err)
			active = result.run
		}
	}
	require.NotNil(t, active)
	require.Equal(t, 1, conflicts)
	require.Len(t, dispatcher.dispatched, 1)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Runs, 1)
	require.Len(t, view.Messages, 1)

	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: active.ID, Status: string(RunSucceeded)})
	require.NoError(t, err)
	_, err = startRun(ctx, service, conversation.ID, "Inspect the next change.")
	require.NoError(t, err)
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

func TestCallbackAuditIsBoundedAndRedacted(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect actions")
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{
		RunID: run.ID, Status: string(RunRunning),
		Events: []CallbackEvent{{Key: "action-001", Kind: "action", AgentMessage: "authorization=Bearer private-value"}},
	})
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{
		RunID: run.ID, Status: string(RunRunning),
		Events: []CallbackEvent{{Key: "action-001", Kind: "action", AgentMessage: "duplicate"}},
	})
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{
		RunID: run.ID, Status: string(RunSucceeded), Summary: "token=hidden-value",
	})
	require.NoError(t, err)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.NotContains(t, view.Runs[0].Summary, "hidden-value")
	require.NotContains(t, view.Messages[1].Content, "hidden-value")
	var matched int
	for _, event := range view.Events {
		if event.EventKey == "action-001" {
			matched++
			require.NotContains(t, event.AgentMessage, "private-value")
		}
	}
	require.Equal(t, 1, matched)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning), Events: []CallbackEvent{{Key: "large", Data: json.RawMessage(`{"secret":"raw"}`)}}})
	// Terminal status ignores stale callbacks; validation still rejects raw data.
	require.ErrorIs(t, err, ErrInvalidCallback)
}

func TestRunContextIsBoundedAndConversationScoped(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	service := NewService(db, &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "workspace", "", EngineCodex)
	require.NoError(t, err)
	other, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)

	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 25; i++ {
		message := &AgentMessage{
			ID: fmt.Sprintf("message-%02d", i), ConversationID: conversation.ID,
			Role: "assistant", Content: strings.Repeat("\"\u2603", 3000),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		_, err = db.NewInsert().Model(message).Exec(ctx)
		require.NoError(t, err)
	}
	_, err = db.NewInsert().Model(&AgentMessage{
		ID: "other-message", ConversationID: other.ID, Role: "user",
		Content: "PRIVATE OTHER CONVERSATION", CreatedAt: base,
	}).Exec(ctx)
	require.NoError(t, err)

	run, err := startRun(ctx, service, conversation.ID, "Current instruction")
	require.NoError(t, err)
	_, err = db.NewInsert().Model(&AgentMessage{
		ID: "future-message", ConversationID: conversation.ID, Role: "assistant",
		Content: "FUTURE", CreatedAt: run.CreatedAt.Add(time.Second),
	}).Exec(ctx)
	require.NoError(t, err)

	result, err := service.GetRunContext(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, "workspace", result.WorkspaceID)
	require.Equal(t, "Current instruction", result.Prompt)
	require.NotEmpty(t, result.History)
	require.LessOrEqual(t, len(result.History), maxContextHistoryMessages)
	encoded, err := json.Marshal(result.History)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), maxContextHistoryBytes+1)
	for _, entry := range result.History {
		encoded, err := json.Marshal(entry)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), maxContextMessageBytes)
		require.Contains(t, entry.Content, "[truncated]")
		require.NotContains(t, entry.Content, "PRIVATE OTHER CONVERSATION")
		require.NotContains(t, entry.Content, "FUTURE")
		require.NotContains(t, entry.Content, "Current instruction")
	}
}

func TestMissingDispatchIsReportedTruthfully(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	engines, err := service.EnginesWithCredentials(ctx)
	require.NoError(t, err)
	for _, engine := range engines {
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

func TestEngineCredentialReportsAreDurableAndEngineScoped(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	engines, err := service.EnginesWithCredentials(ctx)
	require.NoError(t, err)
	require.Equal(t, "not_applicable", engines[0].CredentialStatus)
	for _, engine := range engines[1:] {
		require.True(t, engine.DispatchConfigured)
		require.True(t, engine.Available)
		require.Equal(t, "unchecked", engine.CredentialStatus)
	}
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect the code")
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning),
		Events: []CallbackEvent{{Key: "credentials-present", Kind: "credential", AgentMessage: "Credential was present."}}})
	require.NoError(t, err)
	engines, err = service.EnginesWithCredentials(ctx)
	require.NoError(t, err)
	require.Equal(t, "present", engines[2].CredentialStatus)
	require.NotNil(t, engines[2].CredentialCheckedAt)
	require.Equal(t, "unchecked", engines[1].CredentialStatus)

	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunFailed),
		FailureCode: "missing_codex_credentials", FailureMessage: "Codex needs OPENAI_API_KEY.",
		Events: []CallbackEvent{{Key: "credentials-missing", Kind: "credential", AgentMessage: "Credential is missing."}}})
	require.NoError(t, err)
	engines, err = service.EnginesWithCredentials(ctx)
	require.NoError(t, err)
	require.Equal(t, "missing", engines[2].CredentialStatus)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), stored.Status)
	require.Equal(t, "missing_codex_credentials", stored.FailureCode)
}

func TestLateCredentialFailureRefinesGitHubRecovery(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{lookup: func(AgentRun) (GitHubRunState, error) {
		return GitHubRunState{ID: 934, Status: "completed", Conclusion: "failure"}, nil
	}})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineClaude)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect.")
	require.NoError(t, err)
	_, err = service.ReconcileInterrupted(ctx)
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunFailed),
		FailureCode: "missing_claude_credentials", FailureMessage: "Claude Code needs ANTHROPIC_API_KEY.",
		Events: []CallbackEvent{{Key: "credentials-missing", Kind: "credential", AgentMessage: "Credential is missing."}}})
	require.NoError(t, err)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, "missing_claude_credentials", stored.FailureCode)
}

func TestDispatchDoesNotDowngradeEarlyTerminalCallback(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	var callbackErr error
	githubID := int64(12345)
	dispatcher.onDispatch = func(request DispatchRequest) {
		_, callbackErr = service.ApplyCallback(ctx, AgentCallback{RunID: request.RunID, GitHubRunID: &githubID, Status: string(RunSucceeded), Summary: "Engine finished early."})
	}
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect the code.")
	require.NoError(t, err)
	require.NoError(t, callbackErr)
	require.Equal(t, string(RunSucceeded), run.Status)
	require.Equal(t, "Engine finished early.", run.Summary)
	require.Equal(t, githubID, run.GitHubRunID)
	require.Equal(t, "https://github.com/example/actions", run.WorkflowURL)
}

func TestGitHubRunIDIsBoundToOneAgentRunAndCancellationWaits(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Do work.")
	require.NoError(t, err)

	_, err = service.CancelRun(ctx, run.ID)
	require.ErrorIs(t, err, ErrGitHubRunIDPending)
	require.Empty(t, dispatcher.cancelled)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunDispatched), stored.Status)

	githubID := int64(12345)
	callback := AgentCallback{RunID: run.ID, GitHubRunID: &githubID, Status: string(RunRunning),
		Events: []CallbackEvent{{Key: "registered", Kind: "progress", AgentMessage: "Registered."}}}
	_, err = service.ApplyCallback(ctx, callback)
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, callback)
	require.NoError(t, err)
	stored, err = service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, githubID, stored.GitHubRunID)
	events, err := service.Events(ctx, run.ID, 0)
	require.NoError(t, err)
	registered := 0
	for _, event := range events {
		if event.EventKey == "registered" {
			registered++
		}
	}
	require.Equal(t, 1, registered)

	otherID := int64(54321)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, GitHubRunID: &otherID, Status: string(RunRunning)})
	require.ErrorIs(t, err, ErrInvalidCallback)
	otherConversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	otherRun, err := startRun(ctx, service, otherConversation.ID, "Other task.")
	require.NoError(t, err)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: otherRun.ID, GitHubRunID: &githubID, Status: string(RunRunning)})
	require.ErrorIs(t, err, ErrInvalidCallback, "one GitHub Actions run must not bind to two AgentRuns")
	otherStored, err := service.GetRun(ctx, otherRun.ID)
	require.NoError(t, err)
	require.Zero(t, otherStored.GitHubRunID)

	dispatcher.onCancel = func(cancelled AgentRun) {
		require.Equal(t, run.ID, cancelled.ID)
		require.Equal(t, conversation.ID, cancelled.ConversationID)
		require.Equal(t, githubID, cancelled.GitHubRunID)
	}
	_, err = service.CancelRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, []string{run.ID}, dispatcher.cancelled)
}

func TestGitHubRunIDValidationAndLateBinding(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineClaude)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect.")
	require.NoError(t, err)
	zero := int64(0)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, GitHubRunID: &zero, Status: string(RunRunning)})
	require.ErrorIs(t, err, ErrInvalidCallback)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunSucceeded)})
	require.NoError(t, err)
	githubID := int64(12345)
	stored, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, GitHubRunID: &githubID, Status: string(RunRunning)})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), stored.Status)
	require.Equal(t, githubID, stored.GitHubRunID)
}

func TestCancelStopsFurtherStatusChanges(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Do work.")
	require.NoError(t, err)
	githubID := int64(12345)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning), GitHubRunID: &githubID})
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

func TestCancelDoesNotOverwriteCallbackDuringDispatchCancellation(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Check the branch.")
	require.NoError(t, err)
	githubID := int64(12345)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning), GitHubRunID: &githubID})
	require.NoError(t, err)
	var callbackErr error
	dispatcher.onCancel = func(AgentRun) {
		_, callbackErr = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunSucceeded), Summary: "Completed before cancellation."})
	}

	_, err = service.CancelRun(ctx, run.ID)
	require.ErrorIs(t, err, ErrRunTerminal)
	require.NoError(t, callbackErr)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), stored.Status)
	require.Equal(t, "Completed before cancellation.", stored.Summary)
}

func TestCallbackDoesNotOverwriteCancellationBetweenReadAndWrite(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Check the branch.")
	require.NoError(t, err)
	githubID := int64(12345)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning), GitHubRunID: &githubID})
	require.NoError(t, err)
	var cancelErr error
	canceled := false
	service.now = func() time.Time {
		if !canceled {
			canceled = true
			_, cancelErr = service.CancelRun(ctx, run.ID)
		}
		return time.Now()
	}

	result, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunSucceeded), Summary: "Too late."})
	require.NoError(t, err)
	require.NoError(t, cancelErr)
	require.Equal(t, string(RunCanceled), result.Status)
	require.Empty(t, result.Summary)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Messages, 1)
}

func TestCancellationFailureKeepsRunActive(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{cancelErr: errors.New("GitHub rejected cancellation")}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Inspect the code.")
	require.NoError(t, err)
	githubID := int64(12345)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning), GitHubRunID: &githubID})
	require.NoError(t, err)

	_, err = service.CancelRun(ctx, run.ID)
	require.ErrorContains(t, err, "rejected cancellation")
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunRunning), stored.Status)
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

func TestCallbackDoesNotOverwriteAnotherCallbackBetweenReadAndWrite(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Check the branch.")
	require.NoError(t, err)
	var winnerErr error
	completed := false
	service.now = func() time.Time {
		if !completed {
			completed = true
			_, winnerErr = service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunSucceeded), Summary: "First terminal result."})
		}
		return time.Now()
	}

	result, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunFailed), Summary: "Stale failure."})
	require.NoError(t, err)
	require.NoError(t, winnerErr)
	require.Equal(t, string(RunSucceeded), result.Status)
	require.Equal(t, "First terminal result.", result.Summary)
}

func TestUnidentifiedCodingRunRemainsActiveAndCanComplete(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Deploy the change.")
	require.NoError(t, err)

	count, err := service.ReconcileInterrupted(ctx)
	require.ErrorIs(t, err, ErrGitHubRunUnidentified)
	require.Zero(t, count)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunDispatched), stored.Status)
	require.Empty(t, stored.FailureCode)
	require.Nil(t, stored.FinishedAt)
	events, err := service.Events(ctx, run.ID, 0)
	require.NoError(t, err)
	require.Contains(t, eventKeys(events), "recovery-unknown")

	stillActive, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunRunning)})
	require.NoError(t, err)
	require.Equal(t, string(RunRunning), stillActive.Status)

	corrected, err := service.ApplyCallback(ctx, AgentCallback{
		RunID:   run.ID,
		Status:  string(RunSucceeded),
		Summary: "The engine had already finished after the restart.",
	})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), corrected.Status)
	require.Empty(t, corrected.FailureCode)
	require.Empty(t, corrected.FailureMessage)
	late, err := service.ApplyCallback(ctx, AgentCallback{RunID: run.ID, Status: string(RunFailed), Summary: "Stale failure."})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), late.Status)
	require.Equal(t, corrected.Summary, late.Summary)
}

func eventKeys(events []AgentEvent) []string {
	keys := make([]string, 0, len(events))
	for _, event := range events {
		keys = append(keys, event.EventKey)
	}
	return keys
}

func TestReconcileInterruptedFailsOnlyInProcessAssistant(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	service := NewService(db, &fakeDispatcher{})
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineAssistant)
	require.NoError(t, err)
	run := &AgentRun{ID: "assistant-test-run", ConversationID: conversation.ID, Engine: string(EngineAssistant),
		Status: string(RunRunning), Prompt: "Hello", CreatedAt: time.Now().UTC()}
	_, err = db.NewInsert().Model(run).Exec(ctx)
	require.NoError(t, err)

	count, err := service.ReconcileInterrupted(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunFailed), stored.Status)
	require.Equal(t, "interrupted", stored.FailureCode)
	require.Contains(t, stored.FailureMessage, "cannot resume")
	events, err := service.Events(ctx, run.ID, 0)
	require.NoError(t, err)
	require.Contains(t, eventKeys(events), "assistant-interrupted")
}

func TestReconcileGitHubActiveAndCompletedRuns(t *testing.T) {
	ctx := context.Background()
	db := adminAgentTestDB(t)
	dispatcher := &fakeDispatcher{}
	service := NewService(db, dispatcher)
	activeConversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	active, err := startRun(ctx, service, activeConversation.ID, "Keep coding.")
	require.NoError(t, err)
	activeID := int64(321)
	_, err = service.ApplyCallback(ctx, AgentCallback{RunID: active.ID, GitHubRunID: &activeID, Status: string(RunRunning)})
	require.NoError(t, err)
	completedConversation, err := service.CreateConversation(ctx, "admin", "", "", EngineClaude)
	require.NoError(t, err)
	completed, err := startRun(ctx, service, completedConversation.ID, "Complete the task.")
	require.NoError(t, err)
	dispatcher.lookup = func(run AgentRun) (GitHubRunState, error) {
		if run.ID == active.ID {
			require.Equal(t, activeID, run.GitHubRunID)
			return GitHubRunState{ID: activeID, Status: "in_progress"}, nil
		}
		require.Zero(t, run.GitHubRunID)
		return GitHubRunState{ID: 654, Status: "completed", Conclusion: "success"}, nil
	}

	count, err := service.ReconcileInterrupted(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	activeStored, err := service.GetRun(ctx, active.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunRunning), activeStored.Status)
	require.Nil(t, activeStored.FinishedAt)
	completedStored, err := service.GetRun(ctx, completed.ID)
	require.NoError(t, err)
	require.Equal(t, int64(654), completedStored.GitHubRunID)
	require.Equal(t, string(RunSucceeded), completedStored.Status)
	require.Empty(t, completedStored.Summary, "recovery must never invent a final agent message")
	require.Empty(t, completedStored.FailureMessage)
	view, err := service.GetConversation(ctx, completed.ConversationID)
	require.NoError(t, err)
	require.Len(t, view.Messages, 1)
	require.Contains(t, eventKeys(view.Events), "recovery-completed")
}

func TestReconcileGitHubConclusions(t *testing.T) {
	for _, test := range []struct {
		conclusion string
		status     RunStatus
		code       string
	}{
		{"failure", RunFailed, "github_failure"},
		{"timed_out", RunFailed, "github_timed_out"},
		{"action_required", RunFailed, "github_action_required"},
		{"cancelled", RunCanceled, ""},
		{"neutral", RunFailed, "github_neutral"},
	} {
		t.Run(test.conclusion, func(t *testing.T) {
			ctx := context.Background()
			dispatcher := &fakeDispatcher{lookup: func(AgentRun) (GitHubRunState, error) {
				return GitHubRunState{ID: 765, Status: "completed", Conclusion: test.conclusion}, nil
			}}
			service := NewService(adminAgentTestDB(t), dispatcher)
			conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineOpenCode)
			require.NoError(t, err)
			run, err := startRun(ctx, service, conversation.ID, "Inspect.")
			require.NoError(t, err)
			count, err := service.ReconcileInterrupted(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, count)
			stored, err := service.GetRun(ctx, run.ID)
			require.NoError(t, err)
			require.Equal(t, string(test.status), stored.Status)
			require.Equal(t, test.code, stored.FailureCode)
			require.NotNil(t, stored.FinishedAt)
			require.Empty(t, stored.Summary)
		})
	}
}

func TestReconcileGitHubLookupFailureDoesNotChangeRunStatus(t *testing.T) {
	ctx := context.Background()
	dispatcher := &fakeDispatcher{lookup: func(AgentRun) (GitHubRunState, error) {
		return GitHubRunState{}, context.DeadlineExceeded
	}}
	service := NewService(adminAgentTestDB(t), dispatcher)
	conversation, err := service.CreateConversation(ctx, "admin", "", "", EngineCodex)
	require.NoError(t, err)
	run, err := startRun(ctx, service, conversation.ID, "Do work.")
	require.NoError(t, err)
	count, err := service.ReconcileInterrupted(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, count)
	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunDispatched), stored.Status)
	require.Zero(t, stored.GitHubRunID)
	require.Empty(t, stored.FailureCode)
	require.Nil(t, stored.FinishedAt)
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
