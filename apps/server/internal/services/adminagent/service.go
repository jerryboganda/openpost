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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
)

const (
	MaxPromptCharacters       = 20_000
	MaxSummaryCharacters      = 20_000
	maxAuditTextBytes         = 512
	maxEventsPerRun           = 128
	maxCallbackEvents         = 16
	maxConversationTitle      = 120
	maxContextHistoryMessages = 19 // Plus the current prompt: 20 turns, as in the built-in assistant.
	maxContextHistoryBytes    = 16 << 10
	maxContextMessageBytes    = 4 << 10
)

var auditSensitive = regexp.MustCompile(`(?i)(?:bearer\s+\S+|(?:authorization|cookie|password|secret|token|api[_-]?key|private[_-]?key)\s*[:=]\s*(?:bearer\s+)?\S+|\bsk-[A-Za-z0-9_-]{8,})`)

func safeAuditText(value string) string {
	value = auditSensitive.ReplaceAllString(strings.TrimSpace(value), "[redacted]")
	if len(value) > maxAuditTextBytes {
		value = string([]rune(value)[:min(len([]rune(value)), maxAuditTextBytes/4)]) + "..."
	}
	return value
}

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
	ErrConversationNotFound  = errors.New("conversation not found")
	ErrRunNotFound           = errors.New("run not found")
	ErrRunActive             = errors.New("conversation already has an active run")
	ErrRunTerminal           = errors.New("run has already finished")
	ErrInvalidEngine         = errors.New("unsupported engine")
	ErrInvalidPrompt         = errors.New("prompt must contain between 1 and 20000 characters")
	ErrInvalidCallback       = errors.New("invalid agent callback")
	ErrGitHubRunIDPending    = errors.New("GitHub Actions has not reported its run ID yet; retry cancellation")
	ErrGitHubRunUnidentified = errors.New("GitHub Actions run was not identified by its exact title")
	ErrGitHubRunAmbiguous    = errors.New("multiple GitHub Actions runs have the same agent title")
	ErrGitHubRunSearchLimit  = errors.New("GitHub Actions run search exceeded its page limit")
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
	GitHubRunID    int64      `bun:"github_run_id,notnull" json:"-"`
	FailureCode    string     `bun:"failure_code,notnull" json:"failure_code,omitempty"`
	FailureMessage string     `bun:"failure_message,notnull" json:"failure_message,omitempty"`
	Summary        string     `bun:"summary,notnull" json:"summary,omitempty"`
	CreatedAt      time.Time  `bun:"created_at,notnull" json:"created_at"`
	StartedAt      *time.Time `bun:"started_at" json:"started_at,omitempty"`
	FinishedAt     *time.Time `bun:"finished_at" json:"finished_at,omitempty"`
}

type AgentContextMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type AgentRunContext struct {
	RunID          string                `json:"run_id"`
	ConversationID string                `json:"conversation_id"`
	Engine         string                `json:"engine"`
	Prompt         string                `json:"prompt"`
	BaseRevision   string                `json:"base_revision"`
	WorkspaceID    string                `json:"workspace_id"`
	History        []AgentContextMessage `json:"history"`
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
	ID                  Engine     `json:"id"`
	Label               string     `json:"label"`
	Available           bool       `json:"available"`
	Reason              string     `json:"reason,omitempty"`
	DispatchConfigured  bool       `json:"dispatch_configured"`
	CredentialStatus    string     `json:"credential_status"`
	CredentialCheckedAt *time.Time `json:"credential_checked_at,omitempty"`
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
	GitHubRunID    *int64          `json:"github_run_id,omitempty"`
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

type GitHubRunState struct {
	ID           int64  `json:"id"`
	DisplayTitle string `json:"display_title"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
}

type Dispatcher interface {
	Dispatch(ctx context.Context, request DispatchRequest) (DispatchResult, error)
	Cancel(ctx context.Context, run AgentRun) error
	LookupRun(ctx context.Context, run AgentRun) (GitHubRunState, error)
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
	// finished holds one channel per run that is still working. It closes when
	// the run's goroutine has made its last write.
	finished sync.Map
	now      func() time.Time
}

func NewService(db *bun.DB, dispatcher Dispatcher) *Service {
	return &Service{db: db, dispatcher: dispatcher, now: time.Now}
}

// Engines reports dispatch readiness. Engine credentials live in GitHub Actions
// secrets, so a run reports a missing credential as a failed run rather than
// claiming the engine is available.
func (s *Service) Engines() []EngineStatus {
	// Context-free consumers keep the readiness view; the API also checks signed
	// credential reports through EnginesWithCredentials.
	return s.engineStatuses()
}

func (s *Service) engineStatuses() []EngineStatus {
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
		ID:               EngineAssistant,
		Label:            "OpenPost Agent",
		Available:        agentReason == "",
		Reason:           agentReason,
		CredentialStatus: "not_applicable",
	})
	for _, engine := range []struct {
		id    Engine
		label string
	}{
		{EngineOpenCode, "OpenCode on GitHub Actions"},
		{EngineCodex, "Codex on GitHub Actions"},
		{EngineClaude, "Claude Code on GitHub Actions"},
	} {
		status := EngineStatus{
			ID:                 engine.id,
			Label:              engine.label,
			Available:          dispatchReason == "",
			Reason:             dispatchReason,
			DispatchConfigured: dispatchReason == "",
			CredentialStatus:   "unchecked",
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// EnginesWithCredentials adds the latest signed presence report. Presence
// cannot establish validity until the engine executes successfully.
func (s *Service) EnginesWithCredentials(ctx context.Context) ([]EngineStatus, error) {
	statuses := s.engineStatuses()
	for i := range statuses {
		status := &statuses[i]
		if status.ID == EngineAssistant {
			continue
		}
		var check struct {
			EventKey  string    `bun:"event_key"`
			CreatedAt time.Time `bun:"created_at"`
		}
		err := s.db.NewSelect().TableExpr("admin_agent_events AS e").
			ColumnExpr("e.event_key, e.created_at").
			Join("JOIN admin_agent_runs AS r ON r.id = e.run_id").
			Where("r.engine = ? AND e.kind = ?", status.ID, "credential").
			Where("e.event_key IN (?)", bun.List([]string{"credentials-present", "credentials-missing"})).
			OrderExpr("e.created_at DESC, e.seq DESC").Limit(1).Scan(ctx, &check)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("read %s credential check: %w", status.ID, err)
		}
		if err == nil {
			status.CredentialStatus = "present"
			if check.EventKey == "credentials-missing" {
				status.CredentialStatus = "missing"
			}
			status.CredentialCheckedAt = &check.CreatedAt
		}
	}
	return statuses, nil
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
	s.awaitRunsEnded(ctx, id)
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

// awaitRunsEnded gives the runs of a conversation a moment to make their last
// writes. A stopped run reports as canceled at once, but its goroutine ends a
// little later, and deleting rows underneath it would make those writes fail.
func (s *Service) awaitRunsEnded(ctx context.Context, conversationID string) {
	var runIDs []string
	if err := s.db.NewSelect().Model((*AgentRun)(nil)).Column("id").
		Where("conversation_id = ?", conversationID).Scan(ctx, &runIDs); err != nil {
		return
	}
	grace := time.NewTimer(stopGrace)
	defer grace.Stop()
	for _, runID := range runIDs {
		value, working := s.finished.Load(runID)
		if !working {
			continue
		}
		done, isChannel := value.(chan struct{})
		if !isChannel {
			continue
		}
		select {
		case <-done:
		case <-grace.C:
			return
		case <-ctx.Done():
			return
		}
	}
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
	options, err := s.runOptions(ctx, conversation, input)
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
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "idx_admin_agent_runs_active_conversation") ||
			strings.Contains(message, "unique constraint failed: admin_agent_runs.conversation_id") {
			return nil, ErrRunActive
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
func (s *Service) runOptions(ctx context.Context, conversation *AgentConversation, input StartRunInput) (runSettings, error) {
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
	effort, err := normalizeEffort(input.Effort)
	if err != nil {
		return runSettings{}, err
	}
	if profile := s.models.profile(ctx, model); !profile.Supports(effort) {
		return runSettings{}, &UnsupportedEffortError{
			Model: model.Label, Effort: string(effort), Supported: profile.Names(),
		}
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
	result, err := s.db.NewUpdate().Model(run).
		Column("status", "started_at").
		Where("id = ? AND status = ?", run.ID, RunQueued).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("record agent run: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check agent run start: %w", err)
	}
	if affected == 0 {
		return s.loadAgentRun(ctx, run.ID)
	}
	done := make(chan struct{})
	s.finished.Store(run.ID, done)
	go s.executeAgent(run.ID, done)
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
	resultUpdate, err := s.db.NewUpdate().Model(run).
		Column("status", "dispatch_ref", "workflow_url", "started_at").
		Where("id = ? AND status = ?", run.ID, RunQueued).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("record admin agent dispatch: %w", err)
	}
	affected, err := resultUpdate.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("check admin agent dispatch: %w", err)
	}
	if affected == 0 {
		if _, err := s.db.NewUpdate().Model((*AgentRun)(nil)).
			Set("dispatch_ref = ?", result.Ref).
			Set("workflow_url = ?", result.URL).
			Where("id = ?", run.ID).
			Exec(ctx); err != nil {
			return nil, fmt.Errorf("record admin agent dispatch reference: %w", err)
		}
		return s.loadAgentRun(ctx, run.ID)
	}
	_ = s.appendEvent(ctx, run.ID, "dispatch", "dispatch", "Dispatched to GitHub Actions")
	return run, nil
}

func (s *Service) finishWithoutDispatch(ctx context.Context, run *AgentRun, code, message string) error {
	now := s.now().UTC()
	run.Status = string(RunFailed)
	run.FailureCode = code
	run.FailureMessage = message
	run.FinishedAt = &now
	applied := false
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().Model(run).
			Column("status", "failure_code", "failure_message", "finished_at").
			Where("id = ? AND status = ?", run.ID, RunQueued).
			Exec(ctx)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected == 0 {
			return err
		}
		applied = true
		return appendEventTx(ctx, tx, run.ID, code, "error", message, nil, now)
	}); err != nil {
		return fmt.Errorf("record admin agent dispatch failure: %w", err)
	}
	if !applied {
		current, err := s.loadAgentRun(ctx, run.ID)
		if err != nil {
			return err
		}
		*run = *current
		return nil
	}
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
		if run.GitHubRunID == 0 {
			return nil, ErrGitHubRunIDPending
		}
		if err := s.dispatcher.Cancel(ctx, *run); err != nil {
			return nil, fmt.Errorf("cancel GitHub Actions run: %w", err)
		}
	}
	now := s.now().UTC()
	run.Status = string(RunCanceled)
	run.FinishedAt = &now
	applied := false
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().Model(run).
			Column("status", "finished_at").
			Where("id = ? AND status IN (?)", run.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
			Exec(ctx)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected == 0 {
			return err
		}
		applied = true
		return appendEventTx(ctx, tx, run.ID, "canceled", "status", "Canceled by an administrator", nil, now)
	}); err != nil {
		return nil, fmt.Errorf("cancel admin agent run: %w", err)
	}
	if !applied {
		return nil, ErrRunTerminal
	}
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
	if callback.GitHubRunID != nil {
		if Engine(run.Engine) == EngineAssistant {
			return nil, ErrInvalidCallback
		}
		// Bind a signed Actions identity once; duplicates are harmless, conflicting IDs are rejected.
		result, err := s.db.NewUpdate().Model((*AgentRun)(nil)).
			Set("github_run_id = ?", *callback.GitHubRunID).
			Where("id = ? AND github_run_id IN (0, ?)", run.ID, *callback.GitHubRunID).
			Exec(ctx)
		if err != nil {
			message := strings.ToLower(err.Error())
			if strings.Contains(message, "idx_admin_agent_runs_github_run_id") ||
				strings.Contains(message, "unique constraint failed: admin_agent_runs.github_run_id") {
				return nil, ErrInvalidCallback
			}
			return nil, fmt.Errorf("record GitHub Actions run ID: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("check GitHub Actions run ID: %w", err)
		}
		if affected == 0 {
			return nil, ErrInvalidCallback
		}
		run.GitHubRunID = *callback.GitHubRunID
	}
	if RunStatus(run.Status) == RunCanceled {
		_ = s.appendEvent(ctx, run.ID, "late-result", "error",
			"The engine reported a result after the run was canceled")
		return run, nil
	}
	if callbackBlocked(run, status, callback) {
		return run, nil
	}
	now := s.now().UTC()
	applyAgentCallbackFields(run, status, callback, now)
	applied, err := s.persistAgentCallback(ctx, run, status, callback, now)
	if err != nil {
		return nil, err
	}
	if !applied {
		current, err := s.loadAgentRun(ctx, callback.RunID)
		if err != nil {
			return nil, err
		}
		if RunStatus(current.Status) == RunCanceled {
			_ = s.appendEvent(ctx, current.ID, "late-result", "error",
				"The engine reported a result after the run was canceled")
		}
		return current, nil
	}
	return run, nil
}

func validateAgentCallback(callback *AgentCallback) (RunStatus, error) {
	callback.RunID = strings.TrimSpace(callback.RunID)
	callback.Status = strings.TrimSpace(callback.Status)
	if callback.RunID == "" {
		return "", ErrInvalidCallback
	}
	if callback.GitHubRunID != nil && *callback.GitHubRunID <= 0 {
		return "", ErrInvalidCallback
	}
	status := RunStatus(callback.Status)
	if status != RunRunning && status != RunSucceeded && status != RunFailed {
		return "", ErrInvalidCallback
	}
	if len(callback.Events) > maxCallbackEvents {
		return "", ErrInvalidCallback
	}
	for _, event := range callback.Events {
		if len(event.Key) > 80 || len(event.Kind) > 32 || len(event.AgentMessage) > maxAuditTextBytes || len(event.Data) > 0 {
			return "", ErrInvalidCallback
		}
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

// callbackBlocked allows a late signed credential failure to refine a generic
// GitHub recovery failure without reopening canceled or successful runs.
func callbackBlocked(run *AgentRun, status RunStatus, callback AgentCallback) bool {
	if !RunStatus(run.Status).Terminal() {
		return false
	}
	if run.Status == string(RunFailed) && run.FailureCode == "github_failure" && status == RunFailed &&
		callback.FailureCode == "missing_"+run.Engine+"_credentials" {
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
		run.Summary = safeAuditText(summary)
	}
	if branch := strings.TrimSpace(callback.Branch); branch != "" {
		run.Branch = branch
	}
	if revision := strings.TrimSpace(callback.BaseRevision); revision != "" {
		run.BaseRevision = revision
	}
	switch status {
	case RunFailed:
		run.FailureCode = safeAuditText(callback.FailureCode)
		// Only startup reconciliation may set the recoverable interrupted marker.
		if run.FailureCode == "" || run.FailureCode == "interrupted" {
			run.FailureCode = "engine_failed"
		}
		run.FailureMessage = safeAuditText(callback.FailureMessage)
		if run.FailureMessage == "" {
			run.FailureMessage = "The engine reported a failure"
		}
	case RunSucceeded:
		run.FailureCode = ""
		run.FailureMessage = ""
	}
}

func (s *Service) persistAgentCallback(ctx context.Context, run *AgentRun, status RunStatus, callback AgentCallback, now time.Time) (bool, error) {
	applied := false
	if err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		update := tx.NewUpdate().Model(run).
			Column("status", "branch", "base_revision", "summary", "failure_code", "failure_message", "started_at", "finished_at").
			Where("id = ? AND status IN (?)", run.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)}))
		if status.Terminal() {
			update.WhereOr("id = ? AND status = ? AND failure_code = ?", run.ID, RunFailed, "interrupted")
		}
		if status == RunFailed && callback.FailureCode == "missing_"+run.Engine+"_credentials" {
			update.WhereOr("id = ? AND status = ? AND failure_code = ?", run.ID, RunFailed, "github_failure")
		}
		result, err := update.Exec(ctx)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
		applied = true
		for index, event := range callback.Events {
			key := strings.TrimSpace(event.Key)
			if key == "" {
				key = fmt.Sprintf("callback-%d", index)
			}
			if err := appendEventTx(ctx, tx, run.ID, key, event.Kind, event.AgentMessage, nil, now); err != nil {
				return err
			}
		}
		if RunStatus(run.Status).Terminal() && run.Summary != "" {
			return upsertAssistantMessageTx(ctx, tx, run, now)
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("apply admin agent callback: %w", err)
	}
	return applied, nil
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

// GetRunContext reads only messages before this run, scoped to its conversation.
// The Workspace ID is descriptive; runner authorization remains independent.
func (s *Service) GetRunContext(ctx context.Context, runID string) (*AgentRunContext, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	conversation := new(AgentConversation)
	if err := s.db.NewSelect().Model(conversation).
		Where("id = ?", run.ConversationID).Scan(ctx); err != nil {
		return nil, fmt.Errorf("load admin agent run conversation: %w", err)
	}
	previous := make([]AgentMessage, 0)
	if err := s.db.NewSelect().Model(&previous).
		Where("conversation_id = ? AND created_at < ?", run.ConversationID, run.CreatedAt).
		Where("role IN (?)", bun.List([]string{"user", "assistant"})).
		OrderExpr("created_at DESC, id DESC").
		Limit(maxContextHistoryMessages).
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("load admin agent run history: %w", err)
	}
	result := &AgentRunContext{
		RunID: run.ID, ConversationID: run.ConversationID,
		Engine: run.Engine, Prompt: run.Prompt, BaseRevision: run.BaseRevision,
		History: make([]AgentContextMessage, 0, len(previous)),
	}
	if conversation.WorkspaceID != nil {
		result.WorkspaceID = *conversation.WorkspaceID
	}
	remaining := maxContextHistoryBytes
	for _, message := range previous {
		entry, size := boundedContextMessage(message.Role, message.Content, min(maxContextMessageBytes, remaining-1))
		if size+1 > remaining {
			break
		}
		result.History = append(result.History, entry)
		remaining -= size + 1 // Each JSON array entry also needs a separator.
	}
	for i, j := 0, len(result.History)-1; i < j; i, j = i+1, j-1 {
		result.History[i], result.History[j] = result.History[j], result.History[i]
	}
	return result, nil
}

func boundedContextMessage(role, content string, maxBytes int) (AgentContextMessage, int) {
	entry := AgentContextMessage{Role: role, Content: content}
	encoded, _ := json.Marshal(entry)
	if len(encoded) <= maxBytes {
		return entry, len(encoded)
	}
	runes := []rune(content)
	const suffix = " [truncated]"
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		entry.Content = string(runes[:mid]) + suffix
		encoded, _ = json.Marshal(entry)
		if len(encoded) <= maxBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	entry.Content = string(runes[:low]) + suffix
	encoded, _ = json.Marshal(entry)
	return entry, len(encoded)
}

// ReconcileInterrupted fails in-process assistant work and checks coding work
// against GitHub before changing its status. Unverifiable coding runs remain active.
func (s *Service) ReconcileInterrupted(ctx context.Context) (int, error) {
	runs := make([]AgentRun, 0)
	if err := s.db.NewSelect().Model(&runs).
		Where("status IN (?)", bun.List([]string{
			string(RunQueued), string(RunDispatched), string(RunRunning),
		})).
		Scan(ctx); err != nil {
		return 0, fmt.Errorf("list interrupted admin agent runs: %w", err)
	}
	count := 0
	var failures []error
	for i := range runs {
		run := &runs[i]
		var changed bool
		var err error
		if Engine(run.Engine) == EngineAssistant {
			changed, err = s.interruptAssistant(ctx, run)
		} else {
			changed, err = s.reconcileGitHubRun(ctx, run)
			if err != nil {
				message := "GitHub Actions could not be verified after restart; this run's outcome is unknown."
				if eventErr := s.recoveryEvent(ctx, run.ID, "recovery-unknown", "progress", message); eventErr != nil {
					err = errors.Join(err, eventErr)
				}
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile admin agent run %s: %w", run.ID, err))
		}
		if changed {
			count++
		}
	}
	return count, errors.Join(failures...)
}

func (s *Service) interruptAssistant(ctx context.Context, run *AgentRun) (bool, error) {
	now := s.now().UTC()
	run.Status = string(RunFailed)
	run.FailureCode = "interrupted"
	run.FailureMessage = "OpenPost restarted before the built-in assistant finished. Its generation cannot resume."
	run.FinishedAt = &now
	applied := false
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().Model(run).
			Column("status", "failure_code", "failure_message", "finished_at").
			Where("id = ? AND status IN (?)", run.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
			Exec(ctx)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected == 0 {
			return err
		}
		applied = true
		return appendEventTx(ctx, tx, run.ID, "assistant-interrupted", "error", run.FailureMessage, nil, now)
	})
	return applied, err
}

func (s *Service) reconcileGitHubRun(ctx context.Context, run *AgentRun) (bool, error) {
	if s.dispatcher == nil {
		return false, errors.New("GitHub Actions is not configured")
	}
	remote, err := s.dispatcher.LookupRun(ctx, *run)
	if err != nil {
		return false, err
	}
	if remote.ID <= 0 {
		return false, ErrGitHubRunUnidentified
	}
	if run.GitHubRunID == 0 {
		result, err := s.db.NewUpdate().Model((*AgentRun)(nil)).
			Set("github_run_id = ?", remote.ID).
			Where("id = ? AND github_run_id = 0 AND status IN (?)", run.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
			Exec(ctx)
		if err != nil {
			return false, fmt.Errorf("bind recovered GitHub Actions run ID: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if affected == 0 {
			current, err := s.loadAgentRun(ctx, run.ID)
			if err != nil {
				return false, err
			}
			if RunStatus(current.Status).Terminal() {
				return false, nil // A callback or cancellation won the race.
			}
			if current.GitHubRunID != remote.ID {
				return false, ErrGitHubRunAmbiguous
			}
			*run = *current
		} else {
			run.GitHubRunID = remote.ID
		}
	}

	switch remote.Status {
	case "queued", "requested", "pending", "waiting":
		return true, s.recoveryEvent(ctx, run.ID, "recovery-"+remote.Status, "progress",
			"GitHub Actions is still "+remote.Status+" after the server restart.")
	case "in_progress":
		if _, err := s.db.NewUpdate().Model((*AgentRun)(nil)).
			Set("status = ?", RunRunning).
			Set("started_at = COALESCE(started_at, ?)", s.now().UTC()).
			Where("id = ? AND github_run_id = ? AND status IN (?)", run.ID, remote.ID, bun.List([]string{string(RunQueued), string(RunDispatched)})).
			Exec(ctx); err != nil {
			return false, err
		}
		return true, s.recoveryEvent(ctx, run.ID, "recovery-in-progress", "progress",
			"GitHub Actions is still running after the server restart.")
	case "completed":
		status, code, message, ok := githubConclusion(remote.Conclusion)
		if !ok {
			return false, fmt.Errorf("unknown GitHub Actions conclusion %q", remote.Conclusion)
		}
		now := s.now().UTC()
		applied := false
		err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			result, err := tx.NewUpdate().Model((*AgentRun)(nil)).
				Set("status = ?", status).
				Set("failure_code = ?", code).
				Set("failure_message = ?", message).
				Set("finished_at = ?", now).
				Where("id = ? AND github_run_id = ? AND status IN (?)", run.ID, remote.ID, bun.List([]string{string(RunQueued), string(RunDispatched), string(RunRunning)})).
				Exec(ctx)
			if err != nil {
				return err
			}
			affected, err := result.RowsAffected()
			if err != nil || affected == 0 {
				return err
			}
			applied = true
			return appendEventTx(ctx, tx, run.ID, "recovery-completed", "status",
				"GitHub Actions completed with conclusion "+remote.Conclusion+"; no final agent summary was received.", nil, now)
		})
		return applied, err
	default:
		return false, fmt.Errorf("unknown GitHub Actions status %q", remote.Status)
	}
}

func githubConclusion(conclusion string) (RunStatus, string, string, bool) {
	switch conclusion {
	case "success":
		return RunSucceeded, "", "", true
	case "cancelled":
		return RunCanceled, "", "", true
	case "failure", "timed_out", "action_required", "neutral", "skipped", "stale", "startup_failure":
		return RunFailed, "github_" + conclusion, "GitHub Actions completed with conclusion " + conclusion + ".", true
	default:
		return "", "", "", false
	}
}

func (s *Service) recoveryEvent(ctx context.Context, runID, key, kind, message string) error {
	return s.appendEvent(ctx, runID, key, kind, message)
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

func (s *Service) appendEvent(ctx context.Context, runID, key, kind, message string) error {
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Serialize per-run sequence allocation with callback transactions.
		if _, err := tx.NewUpdate().Model((*AgentRun)(nil)).
			Set("status = status").Where("id = ?", runID).Exec(ctx); err != nil {
			return err
		}
		return appendEventTx(ctx, tx, runID, key, kind, message, nil, s.now().UTC())
	})
}

func (s *Service) RecordWorkflowAction(ctx context.Context, runID, key, state, outcome string) error {
	return s.appendEvent(ctx, runID, "workflow-write-"+key, "action", state+": "+outcome)
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
	var count int
	if err := db.NewSelect().Model((*AgentEvent)(nil)).ColumnExpr("COUNT(*)").Where("run_id = ?", runID).Scan(ctx, &count); err != nil {
		return err
	}
	if count >= maxEventsPerRun {
		return nil
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
		Kind:         safeAuditText(kind),
		AgentMessage: safeAuditText(message),
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
