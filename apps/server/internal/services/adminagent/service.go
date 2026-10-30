// Package adminagent owns administrator conversations with the built-in agent
// and with external coding engines. The server stores durable conversations,
// runs, and an append-only action log. The built-in agent works through tools:
// GitHub, GitHub Actions, the OpenPost API, and read-only production
// inspection. Builds, tests, and coding-engine runs happen on GitHub Actions,
// never on this host.
package adminagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	EngineAssistant Engine = "assistant"
	EngineOpenCode  Engine = "opencode"
	EngineCodex     Engine = "codex"
	EngineClaude    Engine = "claude"
)

func (e Engine) Valid() bool {
	return e == EngineAssistant || e == EngineOpenCode || e == EngineCodex || e == EngineClaude
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
	ErrRunActive            = errors.New("a run is already in progress in this conversation")
)

type AgentConversation struct {
	bun.BaseModel   `bun:"table:admin_agent_conversations"`
	ID              string    `bun:"id,pk" json:"id"`
	Engine          string    `bun:"engine,notnull" json:"engine"`
	Title           string    `bun:"title,notnull" json:"title"`
	CreatedByUserID string    `bun:"created_by_user_id,notnull" json:"-"`
	WorkspaceID     *string   `bun:"workspace_id" json:"workspace_id,omitempty"`
	CreatedAt       time.Time `bun:"created_at,notnull" json:"created_at"`
	UpdatedAt       time.Time `bun:"updated_at,notnull" json:"updated_at"`
}

type AgentMessage struct {
	bun.BaseModel  `bun:"table:admin_agent_messages"`
	ID             string           `bun:"id,pk" json:"id"`
	ConversationID string           `bun:"conversation_id,notnull" json:"conversation_id"`
	Role           string           `bun:"role,notnull" json:"role"`
	Content        string           `bun:"content,notnull" json:"content"`
	RunID          string           `bun:"run_id" json:"run_id,omitempty"`
	CreatedAt      time.Time        `bun:"created_at,notnull" json:"created_at"`
	Attachments    []AttachmentInfo `bun:"-" json:"attachments,omitempty"`
}

type AgentRun struct {
	bun.BaseModel  `bun:"table:admin_agent_runs"`
	ID             string     `bun:"id,pk" json:"id"`
	ConversationID string     `bun:"conversation_id,notnull" json:"conversation_id"`
	Engine         string     `bun:"engine,notnull" json:"engine"`
	Status         string     `bun:"status,notnull" json:"status"`
	Prompt         string     `bun:"prompt,notnull" json:"prompt"`
	Model          string     `bun:"model,notnull" json:"model,omitempty"`
	Effort         string     `bun:"effort,notnull" json:"effort,omitempty"`
	Access         string     `bun:"access,notnull" json:"access,omitempty"`
	InputTokens    int64      `bun:"input_tokens,notnull" json:"input_tokens,omitempty"`
	OutputTokens   int64      `bun:"output_tokens,notnull" json:"output_tokens,omitempty"`
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

type AgentEvent struct {
	bun.BaseModel `bun:"table:admin_agent_events"`
	ID            string    `bun:"id,pk" json:"id"`
	RunID         string    `bun:"run_id,notnull" json:"run_id"`
	Seq           int64     `bun:"seq,notnull" json:"seq"`
	EventKey      string    `bun:"event_key,notnull" json:"-"`
	Kind          string    `bun:"kind,notnull" json:"kind"`
	AgentMessage  string    `bun:"message,notnull" json:"message"`
	DataJSON      string    `bun:"data_json,notnull" json:"-"`
	CreatedAt     time.Time `bun:"created_at,notnull" json:"created_at"`
	// Data is the decoded DataJSON: a tool call's name, arguments, and result,
	// or the model and access of a run. It is empty for plain progress events.
	Data map[string]any `bun:"-" json:"data,omitempty" doc:"Structured details such as a tool call's name, arguments, and result"`
}

// decodeEventData fills Data from the stored JSON, so clients receive objects
// rather than encoded strings. Unreadable or empty payloads stay empty.
func decodeEventData(events []AgentEvent) {
	for index := range events {
		raw := strings.TrimSpace(events[index].DataJSON)
		if raw == "" || raw == "{}" || raw == "null" {
			continue
		}
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil && len(data) > 0 {
			events[index].Data = data
		}
	}
}

type EngineStatus struct {
	ID        Engine `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type AgentConversationView struct {
	AgentConversation
	Messages []AgentMessage `json:"messages"`
	Runs     []AgentRun     `json:"runs"`
	Events   []AgentEvent   `json:"events"`
}

type CallbackEvent struct {
	Key          string          `json:"key"`
	Kind         string          `json:"kind"`
	AgentMessage string          `json:"message"`
	Data         json.RawMessage `json:"data,omitempty"`
}

type AgentCallback struct {
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
	Cancel(ctx context.Context, run AgentRun) error
}

type Service struct {
	db          *bun.DB
	dispatcher  Dispatcher
	agent       *agentRunner
	models      modelCatalog
	github      *githubAPI
	sessions    SessionIssuer
	logs        LogSource
	internalAPI http.Handler
	env         Environment
	specs       specCache
	live        sync.Map
	cancels     sync.Map
	now         func() time.Time
}

func NewService(db *bun.DB, dispatcher Dispatcher) *Service {
	return &Service{db: db, dispatcher: dispatcher, now: time.Now}
}

// Engines reports dispatch readiness. Engine credentials live in GitHub Actions
// secrets, so a run reports a missing credential as a failed run rather than
// claiming the engine is available.
func (s *Service) Engines() []EngineStatus {
	dispatchReason := ""
	if s.dispatcher == nil {
		dispatchReason = "GitHub Actions dispatch is not configured"
	}
	agentReason := ""
	if s.agent == nil {
		agentReason = "The built-in AI provider is not configured"
	}
	statuses := make([]EngineStatus, 0, 4)
	statuses = append(statuses, EngineStatus{
		ID:        EngineAssistant,
		Label:     "OpenPost Agent",
		Available: agentReason == "",
		Reason:    agentReason,
	})
	for _, engine := range []struct {
		id    Engine
		label string
	}{
		{EngineOpenCode, "OpenCode on GitHub Actions"},
		{EngineCodex, "Codex on GitHub Actions"},
		{EngineClaude, "Claude Code on GitHub Actions"},
	} {
		statuses = append(statuses, EngineStatus{
			ID:        engine.id,
			Label:     engine.label,
			Available: dispatchReason == "",
			Reason:    dispatchReason,
		})
	}
	return statuses
}

func (s *Service) CreateConversation(
	ctx context.Context,
	userID, workspaceID, title string,
	engine Engine,
) (*AgentConversation, error) {
	if !engine.Valid() {
		return nil, ErrInvalidEngine
	}
	title = strings.TrimSpace(title)
	if len([]rune(title)) > maxConversationTitle {
		title = string([]rune(title)[:maxConversationTitle])
	}
	now := s.now().UTC()
	conversation := &AgentConversation{
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

func (s *Service) ListConversations(ctx context.Context) ([]AgentConversation, error) {
	conversations := make([]AgentConversation, 0)
	if err := s.db.NewSelect().Model(&conversations).
		OrderExpr("updated_at DESC").
		Limit(100).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent conversations: %w", err)
	}
	return conversations, nil
}

func (s *Service) GetConversation(ctx context.Context, id string) (*AgentConversationView, error) {
	conversation := new(AgentConversation)
	if err := s.db.NewSelect().Model(conversation).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("load admin agent conversation: %w", err)
	}
	view := &AgentConversationView{AgentConversation: *conversation}
	if err := s.db.NewSelect().Model(&view.Messages).
		Where("conversation_id = ?", id).
		OrderExpr("created_at ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent messages: %w", err)
	}
	if err := s.attachInfosToMessages(ctx, view.Messages); err != nil {
		return nil, err
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
	view.Events = make([]AgentEvent, 0)
	if len(runIDs) > 0 {
		if err := s.db.NewSelect().Model(&view.Events).
			Where("run_id IN (?)", bun.List(runIDs)).
			OrderExpr("created_at ASC, seq ASC").
			Scan(ctx); err != nil {
			return nil, fmt.Errorf("list admin agent events: %w", err)
		}
		decodeEventData(view.Events)
	}
	return view, nil
}

// DeleteConversation removes a conversation with its messages, runs, events,
// and images. A conversation with a task in progress must be stopped first.
func (s *Service) DeleteConversation(ctx context.Context, id string) error {
	exists, err := s.db.NewSelect().Model((*AgentConversation)(nil)).Where("id = ?", id).Exists(ctx)
	if err != nil {
		return fmt.Errorf("load admin agent conversation: %w", err)
	}
	if !exists {
		return ErrConversationNotFound
	}
	active, err := s.hasActiveRun(ctx, id)
	if err != nil {
		return err
	}
	if active {
		return ErrRunActive
	}
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Model((*AgentEvent)(nil)).
			Where("run_id IN (SELECT id FROM admin_agent_runs WHERE conversation_id = ?)", id).
			Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*attachmentRow)(nil)).Where("conversation_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*AgentMessage)(nil)).Where("conversation_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*AgentRun)(nil)).Where("conversation_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.NewDelete().Model((*AgentConversation)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("delete admin agent conversation: %w", err)
	}
	return nil
}

func (s *Service) attachInfosToMessages(ctx context.Context, messages []AgentMessage) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.Role == "user" {
			ids = append(ids, message.ID)
		}
	}
	grouped, err := s.attachmentInfos(ctx, ids)
	if err != nil {
		return err
	}
	for index := range messages {
		messages[index].Attachments = grouped[messages[index].ID]
	}
	return nil
}

// StartRunInput describes one administrator message. Model, Effort, and Access
// apply to the built-in agent; empty values use the defaults.
type StartRunInput struct {
	ConversationID string
	UserID         string
	Prompt         string
	AttachmentIDs  []string
	Model          string
	Effort         string
	Access         string
}

func (s *Service) StartRun(ctx context.Context, input StartRunInput) (*AgentRun, error) {
	prompt := strings.TrimSpace(input.Prompt)
	hasImages := len(input.AttachmentIDs) > 0
	if (prompt == "" && !hasImages) || len([]rune(prompt)) > MaxPromptCharacters {
		return nil, ErrInvalidPrompt
	}
	conversation := new(AgentConversation)
	if err := s.db.NewSelect().Model(conversation).Where("id = ?", input.ConversationID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("load admin agent conversation: %w", err)
	}
	active, err := s.hasActiveRun(ctx, conversation.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, ErrRunActive
	}
	options, err := s.runOptions(conversation, input)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	run := &AgentRun{
		ID:             uuid.NewString(),
		ConversationID: conversation.ID,
		Engine:         conversation.Engine,
		Status:         string(RunQueued),
		Prompt:         prompt,
		Model:          options.model,
		Effort:         options.effort,
		Access:         options.access,
		CreatedAt:      now,
	}
	message := &AgentMessage{
		ID:             uuid.NewString(),
		ConversationID: conversation.ID,
		Role:           "user",
		Content:        prompt,
		RunID:          run.ID,
		CreatedAt:      now,
	}
	if err := s.insertRun(ctx, conversation, run, message, input); err != nil {
		if errors.Is(err, ErrAttachmentNotFound) || errors.Is(err, ErrTooManyAttachments) {
			return nil, err
		}
		return nil, fmt.Errorf("start admin agent run: %w", err)
	}

	return s.launchRun(ctx, conversation, run)
}

// hasActiveRun reports whether a task is still queued or working in the
// conversation. One task runs at a time per conversation.
func (s *Service) hasActiveRun(ctx context.Context, conversationID string) (bool, error) {
	active, err := s.db.NewSelect().Model((*AgentRun)(nil)).
		Where("conversation_id = ? AND status IN (?)", conversationID,
			bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("check admin agent runs: %w", err)
	}
	return active, nil
}

// insertRun stores the run, the user message, and its images together, so a
// rejected image leaves neither a message nor a run behind. The first message
// also names an untitled conversation.
func (s *Service) insertRun(
	ctx context.Context,
	conversation *AgentConversation,
	run *AgentRun,
	message *AgentMessage,
	input StartRunInput,
) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(run).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(message).Exec(ctx); err != nil {
			return err
		}
		if err := bindAttachments(ctx, tx, input.UserID, conversation.ID, message.ID, input.AttachmentIDs); err != nil {
			return err
		}
		update := tx.NewUpdate().Model((*AgentConversation)(nil)).
			Set("updated_at = ?", run.CreatedAt).
			Where("id = ?", conversation.ID)
		if strings.TrimSpace(conversation.Title) == "" {
			update = update.Set("title = ?", conversationTitle(run.Prompt))
		}
		_, err := update.Exec(ctx)
		return err
	})
}

type runSettings struct {
	model  string
	effort string
	access string
}

// runOptions validates the per-message choices. They only matter for the
// built-in agent, so Actions engines store the defaults.
func (s *Service) runOptions(conversation *AgentConversation, input StartRunInput) (runSettings, error) {
	access, err := normalizeAccess(input.Access)
	if err != nil {
		return runSettings{}, err
	}
	if Engine(conversation.Engine) != EngineAssistant {
		return runSettings{access: access}, nil
	}
	model, err := s.models.resolve(input.Model)
	if err != nil {
		return runSettings{}, err
	}
	if _, err := normalizeEffort(input.Effort); err != nil {
		return runSettings{}, err
	}
	return runSettings{model: model.ID, effort: strings.ToLower(strings.TrimSpace(input.Effort)), access: access}, nil
}

func conversationTitle(prompt string) string {
	title := firstLine(prompt, 60)
	if title == "" {
		return "Image"
	}
	return title
}

func (s *Service) launchRun(ctx context.Context, conversation *AgentConversation, run *AgentRun) (*AgentRun, error) {
	engine, _ := ParseEngine(conversation.Engine)
	if engine == EngineAssistant {
		return s.launchAgent(ctx, run)
	}
	return s.launchDispatch(ctx, run, engine)
}

func (s *Service) launchAgent(ctx context.Context, run *AgentRun) (*AgentRun, error) {
	if s.agent == nil {
		if err := s.finishWithoutDispatch(ctx, run, "not_configured", "The built-in AI provider is not configured"); err != nil {
			return nil, err
		}
		return run, nil
	}
	started := s.now().UTC()
	run.Status = string(RunRunning)
	run.StartedAt = &started
	if _, err := s.db.NewUpdate().Model(run).
		Column("status", "started_at").
		Where("id = ?", run.ID).
		Exec(ctx); err != nil {
		return nil, fmt.Errorf("record agent run: %w", err)
	}
	go s.executeAgent(run.ID)
	return run, nil
}

func (s *Service) launchDispatch(ctx context.Context, run *AgentRun, engine Engine) (*AgentRun, error) {
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
	s.appendEvent(ctx, run.ID, "dispatch", "dispatch", "Dispatched to GitHub Actions")
	return run, nil
}

func (s *Service) finishWithoutDispatch(ctx context.Context, run *AgentRun, code, message string) error {
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
	s.appendEvent(ctx, run.ID, code, "error", message)
	return nil
}

func (s *Service) CancelRun(ctx context.Context, runID string) (*AgentRun, error) {
	run := new(AgentRun)
	if err := s.db.NewSelect().Model(run).Where("id = ?", runID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("load admin agent run: %w", err)
	}
	if RunStatus(run.Status).Terminal() {
		return run, ErrRunTerminal
	}
	if Engine(run.Engine) != EngineAssistant {
		if s.dispatcher == nil {
			return nil, errors.New("GitHub Actions cancellation is not configured")
		}
		if err := s.dispatcher.Cancel(ctx, *run); err != nil {
			return nil, fmt.Errorf("cancel GitHub Actions run: %w", err)
		}
	}
	now := s.now().UTC()
	run.Status = string(RunCanceled)
	run.FinishedAt = &now
	result, err := s.db.NewUpdate().Model(run).
		Column("status", "finished_at").
		Where("id = ? AND status IN (?)", run.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cancel admin agent run: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected == 0 {
		return nil, ErrRunTerminal
	}
	s.appendEvent(ctx, run.ID, "canceled", "status", "Canceled by an administrator")
	s.cancelRunContext(run.ID)
	return run, nil
}

func (s *Service) ApplyCallback(ctx context.Context, callback AgentCallback) (*AgentRun, error) {
	status, err := validateAgentCallback(&callback)
	if err != nil {
		return nil, err
	}
	run, err := s.loadAgentRun(ctx, callback.RunID)
	if err != nil {
		return nil, err
	}
	if RunStatus(run.Status) == RunCanceled {
		s.appendEvent(ctx, run.ID, "late-result", "error",
			"The engine reported a result after the run was canceled")
		return run, nil
	}
	if callbackBlocked(run, status) {
		return run, nil
	}
	now := s.now().UTC()
	applyAgentCallbackFields(run, status, callback, now)
	if err := s.persistAgentCallback(ctx, run, callback, now); err != nil {
		return nil, err
	}
	return run, nil
}

func validateAgentCallback(callback *AgentCallback) (RunStatus, error) {
	callback.RunID = strings.TrimSpace(callback.RunID)
	callback.Status = strings.TrimSpace(callback.Status)
	if callback.RunID == "" {
		return "", ErrInvalidCallback
	}
	status := RunStatus(callback.Status)
	if status != RunRunning && status != RunSucceeded && status != RunFailed {
		return "", ErrInvalidCallback
	}
	return status, nil
}

func (s *Service) loadAgentRun(ctx context.Context, runID string) (*AgentRun, error) {
	run := new(AgentRun)
	if err := s.db.NewSelect().Model(run).Where("id = ?", runID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, fmt.Errorf("load admin agent run: %w", err)
	}
	return run, nil
}

// callbackBlocked keeps terminal runs immutable except for one case: an engine
// that finished after a restart may correct an interrupted run's unknown state.
func callbackBlocked(run *AgentRun, status RunStatus) bool {
	if !RunStatus(run.Status).Terminal() {
		return false
	}
	return run.Status != string(RunFailed) || run.FailureCode != "interrupted" || !status.Terminal()
}

func applyAgentCallbackFields(run *AgentRun, status RunStatus, callback AgentCallback, now time.Time) {
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
}

func (s *Service) persistAgentCallback(ctx context.Context, run *AgentRun, callback AgentCallback, now time.Time) error {
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
			if err := appendEventTx(ctx, tx, run.ID, key, event.Kind, event.AgentMessage, event.Data, now); err != nil {
				return err
			}
		}
		if RunStatus(run.Status).Terminal() && run.Summary != "" {
			return upsertAssistantMessageTx(ctx, tx, run, now)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("apply admin agent callback: %w", err)
	}
	return nil
}

func (s *Service) Events(ctx context.Context, runID string, afterSeq int64) ([]AgentEvent, error) {
	if _, err := s.runExists(ctx, runID); err != nil {
		return nil, err
	}
	events := make([]AgentEvent, 0)
	if err := s.db.NewSelect().Model(&events).
		Where("run_id = ? AND seq > ?", runID, afterSeq).
		OrderExpr("seq ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list admin agent events: %w", err)
	}
	decodeEventData(events)
	return events, nil
}

func (s *Service) GetRun(ctx context.Context, runID string) (*AgentRun, error) {
	run := new(AgentRun)
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
	runs := make([]AgentRun, 0)
	if err := s.db.NewSelect().Model(&runs).
		Where("status IN (?)", bun.List([]string{
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
			run.FailureMessage)
	}
	return len(runs), nil
}

func (s *Service) runExists(ctx context.Context, runID string) (bool, error) {
	exists, err := s.db.NewSelect().Model((*AgentRun)(nil)).Where("id = ?", runID).Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("load admin agent run: %w", err)
	}
	if !exists {
		return false, ErrRunNotFound
	}
	return true, nil
}

func (s *Service) appendEvent(ctx context.Context, runID, key, kind, message string) {
	_ = appendEventTx(ctx, s.db, runID, key, kind, message, nil, s.now().UTC())
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
		exists, err := db.NewSelect().Model((*AgentEvent)(nil)).
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
	if err := db.NewSelect().Model((*AgentEvent)(nil)).
		ColumnExpr("COALESCE(MAX(seq), 0)").
		Where("run_id = ?", runID).
		Scan(ctx, &maxSeq); err != nil {
		return err
	}
	encoded := "{}"
	if len(data) > 0 {
		encoded = string(data)
	}
	event := &AgentEvent{
		ID:           uuid.NewString(),
		RunID:        runID,
		Seq:          maxSeq + 1,
		EventKey:     key,
		Kind:         kind,
		AgentMessage: strings.TrimSpace(message),
		DataJSON:     encoded,
		CreatedAt:    now,
	}
	_, err := db.NewInsert().Model(event).Exec(ctx)
	return err
}

func upsertAssistantMessageTx(ctx context.Context, db execer, run *AgentRun, now time.Time) error {
	existing := new(AgentMessage)
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
	_, err = db.NewInsert().Model(&AgentMessage{
		ID:             uuid.NewString(),
		ConversationID: run.ConversationID,
		Role:           "assistant",
		Content:        run.Summary,
		RunID:          run.ID,
		CreatedAt:      now,
	}).Exec(ctx)
	return err
}
