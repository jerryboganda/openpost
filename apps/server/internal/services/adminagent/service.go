// Package adminagent owns administrator conversations with external coding
// engines. The server stores durable conversations, runs, and an append-only
// action log; execution happens on GitHub Actions, never on this host.
package adminagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

const (
	MaxPromptCharacters  = 20_000
	MaxSummaryCharacters = 20_000
	maxConversationTitle = 120
)

type Engine string

const (
	EngineOpenCode Engine = "opencode"
	EngineCodex    Engine = "codex"
	EngineClaude   Engine = "claude"
)

func (e Engine) Valid() bool {
	return e == EngineOpenCode || e == EngineCodex || e == EngineClaude
}

func ParseEngine(value string) (Engine, error) {
	engine := Engine(strings.TrimSpace(strings.ToLower(value)))
	if !engine.Valid() {
		return "", ErrInvalidEngine
	}
	return engine, nil
}

type RunStatus string

const (
	RunQueued     RunStatus = "queued"
	RunDispatched RunStatus = "dispatched"
	RunRunning    RunStatus = "running"
	RunSucceeded  RunStatus = "succeeded"
	RunFailed     RunStatus = "failed"
	RunCanceled   RunStatus = "canceled"
)

func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCanceled
}

var (
	ErrConversationNotFound = errors.New("conversation not found")
	ErrRunNotFound          = errors.New("run not found")
	ErrRunTerminal          = errors.New("run has already finished")
	ErrInvalidEngine        = errors.New("unsupported engine")
	ErrInvalidPrompt        = errors.New("prompt must contain between 1 and 20000 characters")
	ErrInvalidCallback      = errors.New("invalid agent callback")
)

type Conversation struct {
	bun.BaseModel   `bun:"table:admin_agent_conversations"`
	ID              string    `bun:"id,pk" json:"id"`
	Engine          string    `bun:"engine,notnull" json:"engine"`
	Title           string    `bun:"title,notnull" json:"title"`
	CreatedByUserID string    `bun:"created_by_user_id,notnull" json:"-"`
	WorkspaceID     *string   `bun:"workspace_id" json:"workspace_id,omitempty"`
	CreatedAt       time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt       time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

type Message struct {
	bun.BaseModel  `bun:"table:admin_agent_messages"`
	ID             string    `bun:"id,pk" json:"id"`
	ConversationID string    `bun:"conversation_id,notnull" json:"conversation_id"`
	Role           string    `bun:"role,notnull" json:"role"`
	Content        string    `bun:"content,notnull" json:"content"`
	RunID          string    `bun:"run_id" json:"run_id,omitempty"`
	CreatedAt      time.Time `bun:"created_at,notnull" json:"created_at"`
}

type Run struct {
	bun.BaseModel  `bun:"table:admin_agent_runs"`
	ID             string     `bun:"id,pk" json:"id"`
	ConversationID string     `bun:"conversation_id,notnull" json:"conversation_id"`
	Engine         string     `bun:"engine,notnull" json:"engine"`
	Status         string     `bun:"status,notnull" json:"status"`
	Prompt         string     `bun:"prompt,notnull" json:"prompt"`
	BaseRevision   string     `bun:"base_revision,notnull" json:"base_revision,omitempty"`
	Branch         string     `bun:"branch,notnull" json:"branch,omitempty"`
	DispatchRef    string     `bun:"dispatch_ref,notnull" json:"dispatch_ref,omitempty"`
	WorkflowURL    string     `bun:"workflow_url,notnull" json:"workflow_url,omitempty"`
	FailureCode    string     `bun:"failure_code,notnull" json:"failure_code,omitempty"`
	FailureMessage string     `bun:"failure_message,notnull" json:"failure_message,omitempty"`
	Summary        string     `bun:"summary,notnull" json:"summary,omitempty"`
	CreatedAt      time.Time  `bun:"created_at,notnull" json:"created_at"`
	StartedAt      *time.Time `bun:"started_at" json:"started_at,omitempty"`
	FinishedAt     *time.Time `bun:"finished_at" json:"finished_at,omitempty"`
}

type Event struct {
	bun.BaseModel `bun:"table:admin_agent_events"`
	ID            string    `bun:"id,pk" json:"id"`
	RunID         string    `bun:"run_id,notnull" json:"run_id"`
	Seq           int64     `bun:"seq,notnull" json:"seq"`
	EventKey      string    `bun:"event_key,notnull" json:"-"`
	Kind          string    `bun:"kind,notnull" json:"kind"`
	Message       string    `bun:"message,notnull" json:"message"`
	DataJSON      string    `bun:"data_json,notnull" json:"-"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
}

type EngineStatus struct {
	ID        Engine `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type ConversationView struct {
	Conversation
	Messages []Message `json:"messages"`
	Runs     []Run     `json:"runs"`
	Events   []Event   `json:"events"`
}

type CallbackEvent struct {
	Key     string          `json:"key"`
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Callback struct {
	RunID          string          `json:"run_id"`
	Status         string          `json:"status"`
	Summary        string          `json:"summary"`
	FailureCode    string          `json:"failure_code"`
	FailureMessage string          `json:"failure_message"`
	Branch         string          `json:"branch"`
	BaseRevision   string          `json:"base_revision"`
	Events         []CallbackEvent `json:"events"`
}

type DispatchRequest struct {
	RunID          string
	ConversationID string
	Engine         Engine
	Prompt         string
	BaseRevision   string
}

type DispatchResult struct {
	Ref string
	URL string
}

type Dispatcher interface {
	Dispatch(ctx context.Context, request DispatchRequest) (DispatchResult, error)
	Cancel(ctx context.Context, run Run) error
}

type Service struct {
	db         *bun.DB
	dispatcher Dispatcher
	now        func() time.Time
}

func NewService(db *bun.DB, dispatcher Dispatcher) *Service {
	return &Service{db: db, dispatcher: dispatcher, now: time.Now}
}

// Engines reports dispatch readiness. Engine credentials live in GitHub Actions
// secrets, so a run reports a missing credential as a failed run rather than
// claiming the engine is available.
func (s *Service) Engines() []EngineStatus {
	reason := ""
	if s.dispatcher == nil {
		reason = "GitHub Actions dispatch is not configured"
	}
	statuses := make([]EngineStatus, 0, 3)
	for _, engine := range []struct {
		id    Engine
		label string
	}{
		{EngineOpenCode, "OpenCode"},
		{EngineCodex, "Codex"},
		{EngineClaude, "Claude Code"},
	} {
		statuses = append(statuses, EngineStatus{
			ID:        engine.id,
			Label:     engine.label,
			Available: reason == "",
			Reason:    reason,
		})
	}
	return statuses
}

func (s *Service) CreateConversation(
	ctx context.Context,
	userID, workspaceID, title string,
	engine Engine,
) (*Conversation, error) {
	if !engine.Valid() {
		return nil, ErrInvalidEngine
	}
	title = strings.TrimSpace(title)
	if len([]rune(title)) > maxConversationTitle {
		title = string([]rune(title)[:maxConversationTitle])
	}
	now := s.now().UTC()
	conversation := &Conversation{
		ID:              uuid.NewString(),
		Engine:          string(engine),
		Title:           title,
		CreatedByUserID: userID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if workspace := strings.TrimSpace(workspaceID); workspace != "" {
		conversation.WorkspaceID = &workspace
	}
	if _, err := s.db.NewInsert().Model(conversation).Exec(ctx); err != nil {
		return nil, fmt.Errorf("create admin agent conversation: %w", err)
	}
	return conversation, nil
}

func (s *Service) ListConversations(ctx context.Context) ([]Conversation, error) {
	conversations := make([]Conversation, 0)
	if err := s.db.NewSelect().Model(&conversations).
		OrderExpr("updated_at DESC").
		Limit(100).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent conversations: %w", err)
	}
	return conversations, nil
}

func (s *Service) GetConversation(ctx context.Context, id string) (*ConversationView, error) {
	conversation := new(Conversation)
	if err := s.db.NewSelect().Model(conversation).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("load admin agent conversation: %w", err)
	}
	view := &ConversationView{Conversation: *conversation}
	if err := s.db.NewSelect().Model(&view.Messages).
		Where("conversation_id = ?", id).
		OrderExpr("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent messages: %w", err)
	}
	if err := s.db.NewSelect().Model(&view.Runs).
		Where("conversation_id = ?", id).
		OrderExpr("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent runs: %w", err)
	}
	runIDs := make([]string, 0, len(view.Runs))
	for _, run := range view.Runs {
		runIDs = append(runIDs, run.ID)
	}
	view.Events = make([]Event, 0)
	if len(runIDs) > 0 {
		if err := s.db.NewSelect().Model(&view.Events).
			Where("run_id IN (?)", bun.In(runIDs)).
			OrderExpr("created_at ASC, seq ASC").
			Scan(ctx); err != nil {
			return nil, fmt.Errorf("list admin agent events: %w", err)
		}
	}
	return view, nil
}

func (s *Service) StartRun(ctx context.Context, conversationID, prompt string) (*Run, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" || len([]rune(prompt)) > MaxPromptCharacters {
		return nil, ErrInvalidPrompt
	}
	conversation := new(Conversation)
	if err := s.db.NewSelect().Model(conversation).Where("id = ?", conversationID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("load admin agent conversation: %w", err)
	}
	now := s.now().UTC()
	run := &Run{
		ID:             uuid.NewString(),
		ConversationID: conversation.ID,
		Engine:         conversation.Engine,
		Status:         string(RunQueued),
		Prompt:         prompt,
		CreatedAt:      now,
	}
	message := &Message{
		ID:             uuid.NewString(),
		ConversationID: conversation.ID,
		Role:           "user",
		Content:        prompt,
		RunID:          run.ID,
		CreatedAt:      now,
	}
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(run).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(message).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*Conversation)(nil)).
			Set("updated_at = ?", now).
			Where("id = ?", conversation.ID).
			Exec(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("start admin agent run: %w", err)
	}

	engine, _ := ParseEngine(conversation.Engine)
	if s.dispatcher == nil {
		if err := s.finishWithoutDispatch(ctx, run, "not_configured", "GitHub Actions dispatch is not configured"); err != nil {
			return nil, err
		}
		return run, nil
	}
	result, err := s.dispatcher.Dispatch(ctx, DispatchRequest{
		RunID:          run.ID,
		ConversationID: run.ConversationID,
		Engine:         engine,
		Prompt:         run.Prompt,
	})
	if err != nil {
		if err := s.finishWithoutDispatch(ctx, run, "dispatch_failed", "Could not dispatch the run to GitHub Actions"); err != nil {
			return nil, err
		}
		return run, nil
	}
	run.Status = string(RunDispatched)
	run.DispatchRef = result.Ref
	run.WorkflowURL = result.URL
	started := s.now().UTC()
	run.StartedAt = &started
	if _, err := s.db.NewUpdate().Model(run).
		Column("status", "dispatch_ref", "workflow_url", "started_at").
		Where("id = ?", run.ID).
		Exec(ctx); err != nil {
		return nil, fmt.Errorf("record admin agent dispatch: %w", err)
	}
	s.appendEvent(ctx, run.ID, "dispatch", "dispatch", "Dispatched to GitHub Actions", nil)
	return run, nil
}

func (s *Service) finishWithoutDispatch(ctx context.Context, run *Run, code, message string) error {
	now := s.now().UTC()
	run.Status = string(RunFailed)
	run.FailureCode = code
	run.FailureMessage = message
	run.FinishedAt = &now
	if _, err := s.db.NewUpdate().Model(run).
		Column("status", "failure_code", "failure_message", "finished_at").
		Where("id = ?", run.ID).
		Exec(ctx); err != nil {
		return fmt.Errorf("record admin agent dispatch failure: %w", err)
	}
	s.appendEvent(ctx, run.ID, code, "error", message, nil)
	return nil
}

func (s *Service) CancelRun(ctx context.Context, runID string) (*Run, error) {
	run := new(Run)
	if err := s.db.NewSelect().Model(run).Where("id = ?", runID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("load admin agent run: %w", err)
	}
	if RunStatus(run.Status).Terminal() {
		return run, ErrRunTerminal
	}
	if s.dispatcher != nil {
		_ = s.dispatcher.Cancel(ctx, *run)
	}
	now := s.now().UTC()
	run.Status = string(RunCanceled)
	run.FinishedAt = &now
	if _, err := s.db.NewUpdate().Model(run).
		Column("status", "finished_at").
		Where("id = ?", run.ID).
		Exec(ctx); err != nil {
		return nil, fmt.Errorf("cancel admin agent run: %w", err)
	}
	s.appendEvent(ctx, run.ID, "canceled", "status", "Canceled by an administrator", nil)
	return run, nil
}

func (s *Service) ApplyCallback(ctx context.Context, callback Callback) (*Run, error) {
	callback.RunID = strings.TrimSpace(callback.RunID)
	callback.Status = strings.TrimSpace(callback.Status)
	if callback.RunID == "" {
		return nil, ErrInvalidCallback
	}
	status := RunStatus(callback.Status)
	if status != RunRunning && status != RunSucceeded && status != RunFailed {
		return nil, ErrInvalidCallback
	}
	run := new(Run)
	if err := s.db.NewSelect().Model(run).Where("id = ?", callback.RunID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("load admin agent run: %w", err)
	}
	if RunStatus(run.Status) == RunCanceled {
		s.appendEvent(ctx, run.ID, "late-result", "error",
			"The engine reported a result after the run was canceled", nil)
		return run, nil
	}

	now := s.now().UTC()
	run.Status = string(status)
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	if status == RunSucceeded || status == RunFailed {
		run.FinishedAt = &now
	}
	if summary := strings.TrimSpace(callback.Summary); summary != "" {
		if len([]rune(summary)) > MaxSummaryCharacters {
			summary = string([]rune(summary)[:MaxSummaryCharacters])
		}
		run.Summary = summary
	}
	if branch := strings.TrimSpace(callback.Branch); branch != "" {
		run.Branch = branch
	}
	if revision := strings.TrimSpace(callback.BaseRevision); revision != "" {
		run.BaseRevision = revision
	}
	if status == RunFailed {
		run.FailureCode = strings.TrimSpace(callback.FailureCode)
		if run.FailureCode == "" {
			run.FailureCode = "engine_failed"
		}
		run.FailureMessage = strings.TrimSpace(callback.FailureMessage)
		if run.FailureMessage == "" {
			run.FailureMessage = "The engine reported a failure"
		}
	}
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model(run).
			Column("status", "branch", "base_revision", "summary", "failure_code", "failure_message", "started_at", "finished_at").
			Where("id = ?", run.ID).
			Exec(ctx); err != nil {
			return err
		}
		for index, event := range callback.Events {
			key := strings.TrimSpace(event.Key)
			if key == "" {
				key = fmt.Sprintf("callback-%d", index)
			}
			if err := appendEventTx(ctx, tx, run.ID, key, event.Kind, event.Message, event.Data, now); err != nil {
				return err
			}
		}
		if RunStatus(run.Status).Terminal() && run.Summary != "" {
			return upsertAssistantMessageTx(ctx, tx, run, now)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("apply admin agent callback: %w", err)
	}
	return run, nil
}

func (s *Service) Events(ctx context.Context, runID string, afterSeq int64) ([]Event, error) {
	if _, err := s.runExists(ctx, runID); err != nil {
		return nil, err
	}
	events := make([]Event, 0)
	if err := s.db.NewSelect().Model(&events).
		Where("run_id = ? AND seq > ?", runID, afterSeq).
		OrderExpr("seq ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent events: %w", err)
	}
	return events, nil
}

func (s *Service) GetRun(ctx context.Context, runID string) (*Run, error) {
	run := new(Run)
	if err := s.db.NewSelect().Model(run).Where("id = ?", runID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("load admin agent run: %w", err)
	}
	return run, nil
}

// FailInterrupted marks runs that were in flight when this process last
// stopped. The engine may still have finished, so the failure states that the
// outcome is unknown instead of claiming success or failure.
func (s *Service) FailInterrupted(ctx context.Context) (int, error) {
	now := s.now().UTC()
	runs := make([]Run, 0)
	if err := s.db.NewSelect().Model(&runs).
		Where("status IN (?)", bun.In([]string{
			string(RunQueued), string(RunDispatched), string(RunRunning),
		})).
		Scan(ctx); err != nil {
		return 0, fmt.Errorf("list interrupted admin agent runs: %w", err)
	}
	for index := range runs {
		run := &runs[index]
		run.Status = string(RunFailed)
		run.FailureCode = "interrupted"
		run.FailureMessage = "OpenPost restarted before the engine reported a result. The outcome is unknown."
		run.FinishedAt = &now
		if _, err := s.db.NewUpdate().Model(run).
			Column("status", "failure_code", "failure_message", "finished_at").
			Where("id = ?", run.ID).
			Exec(ctx); err != nil {
			return index, fmt.Errorf("mark interrupted admin agent run: %w", err)
		}
		s.appendEvent(ctx, run.ID, fmt.Sprintf("interrupted-%d", index), "error",
			run.FailureMessage, nil)
	}
	return len(runs), nil
}

func (s *Service) runExists(ctx context.Context, runID string) (bool, error) {
	exists, err := s.db.NewSelect().Model((*Run)(nil)).Where("id = ?", runID).Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("load admin agent run: %w", err)
	}
	if !exists {
		return false, ErrRunNotFound
	}
	return true, nil
}

func (s *Service) appendEvent(ctx context.Context, runID, key, kind, message string, data json.RawMessage) {
	_ = appendEventTx(ctx, s.db, runID, key, kind, message, data, s.now().UTC())
}

type execer interface {
	NewInsert() *bun.InsertQuery
	NewSelect() *bun.SelectQuery
	NewUpdate() *bun.UpdateQuery
}

func appendEventTx(
	ctx context.Context,
	db execer,
	runID, key, kind, message string,
	data json.RawMessage,
	now time.Time,
) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "progress"
	}
	if key != "" {
		exists, err := db.NewSelect().Model((*Event)(nil)).
			Where("run_id = ? AND event_key = ?", runID, key).
			Exists(ctx)
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
	}
	var maxSeq int64
	if err := db.NewSelect().Model((*Event)(nil)).
		ColumnExpr("COALESCE(MAX(seq), 0)").
		Where("run_id = ?", runID).
		Scan(ctx, &maxSeq); err != nil {
		return err
	}
	encoded := "{}"
	if len(data) > 0 {
		encoded = string(data)
	}
	event := &Event{
		ID:        uuid.NewString(),
		RunID:     runID,
		Seq:       maxSeq + 1,
		EventKey:  key,
		Kind:      kind,
		Message:   strings.TrimSpace(message),
		DataJSON:  encoded,
		CreatedAt: now,
	}
	_, err := db.NewInsert().Model(event).Exec(ctx)
	return err
}

func upsertAssistantMessageTx(ctx context.Context, db execer, run *Run, now time.Time) error {
	existing := new(Message)
	err := db.NewSelect().Model(existing).
		Where("run_id = ? AND role = ?", run.ID, "assistant").
		Scan(ctx)
	if err == nil {
		_, updateErr := db.NewUpdate().Model(existing).
			Set("content = ?", run.Summary).
			Where("id = ?", existing.ID).
			Exec(ctx)
		return updateErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = db.NewInsert().Model(&Message{
		ID:             uuid.NewString(),
		ConversationID: run.ConversationID,
		Role:           "assistant",
		Content:        run.Summary,
		RunID:          run.ID,
		CreatedAt:      now,
	}).Exec(ctx)
	return err
}
