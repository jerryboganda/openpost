package adminagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/openpost/backend/internal/ai"
)

const (
	maxToolOutput   = 24_000
	maxEventOutput  = 4_000
	maxEventArgs    = 6_000
	agentSessionTTL = 2 * time.Hour
)

var (
	errReadOnlyAccess = errors.New("this run has read-only access; switch the access mode to full access to make changes")
	errToolNotReady   = errors.New("this tool is not configured on this instance")
)

// SessionIssuer opens short-lived administrator sessions for the agent's own
// calls to the OpenPost API. The returned revoke function ends the session.
type SessionIssuer interface {
	IssueSession(ctx context.Context, userID string, ttl time.Duration) (token string, revoke func(context.Context), err error)
}

// LogSource exposes recent structured log lines.
type LogSource interface {
	Snapshot(limit int, keep func(string) bool) []string
}

// Environment describes the running instance for the agent.
type Environment struct {
	PublicURL      string
	Version        string
	Revision       string
	Edition        string
	DatabaseDriver string
	AIProvider     string
	StartedAt      time.Time
	Repository     string
	DefaultBranch  string
	// Secrets are literal values that must never reach the model or the log.
	Secrets []string
}

// toolSpec is one capability offered to the model.
type toolSpec struct {
	name        string
	description string
	parameters  map[string]any
	// write marks tools that change something. Read-only runs never offer them.
	write    bool
	describe func(args toolArgs) string
	run      func(ctx context.Context, rt *toolRuntime, args toolArgs) (string, error)
}

// toolArgs holds decoded tool arguments. Models sometimes send numbers and
// booleans as strings, so the getters accept both forms.
type toolArgs map[string]any

func (a toolArgs) text(key string) string {
	return scalarString(a[key])
}

// scalarString renders a decoded JSON scalar. JSON numbers decode as float64,
// which fmt prints in scientific notation above a million, so integers such as
// workflow run ids are formatted explicitly.
func scalarString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func (a toolArgs) trimmed(key string) string {
	return strings.TrimSpace(a.text(key))
}

func (a toolArgs) number(key string, fallback int) int {
	switch value := a[key].(type) {
	case float64:
		return int(value)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return fallback
}

func (a toolArgs) flag(key string) bool {
	switch value := a[key].(type) {
	case bool:
		return value
	case string:
		return strings.EqualFold(strings.TrimSpace(value), "true")
	}
	return false
}

func (a toolArgs) object(key string) map[string]any {
	switch value := a[key].(type) {
	case map[string]any:
		return value
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(value), &parsed) == nil {
			return parsed
		}
	}
	return nil
}

func parseToolArgs(raw string) (toolArgs, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return toolArgs{}, nil
	}
	var args toolArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, fmt.Errorf("the arguments are not a valid JSON object (%v); send smaller arguments", err)
	}
	if args == nil {
		args = toolArgs{}
	}
	return args, nil
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func integerSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func booleanSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func objectProperty(description string) map[string]any {
	return map[string]any{"type": "object", "description": description}
}

func enumSchema(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

// clipText cuts text to at most limit bytes on a character boundary and says
// how much was removed.
func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n…[%d more bytes not shown]", len(text)-cut)
}

func firstLine(text string, limit int) string {
	text = strings.TrimSpace(text)
	if newline := strings.IndexByte(text, '\n'); newline >= 0 {
		text = text[:newline]
	}
	if len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{24,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
	// Password hashes. A column that is renamed in a query would slip past the
	// column-name mask, so the values themselves are recognized as well.
	regexp.MustCompile(`\$2[aby]?\$\d{2}\$[./A-Za-z0-9]{53}`),
	regexp.MustCompile(`\$argon2(?:id|i|d)\$[^\s"'<>]{20,}`),
	regexp.MustCompile(`\$pbkdf2[-_a-z0-9]*\$[^\s"'<>]{20,}`),
}

// committedSecretPatterns are the shapes that must never be committed. They
// are narrower than secretPatterns so test fixtures and documentation that show
// short example tokens are not blocked.
var committedSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{32,}`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

func containsCommittableSecret(text string) bool {
	for _, pattern := range committedSecretPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func (s *Service) redact(text string) string {
	for _, literal := range s.env.Secrets {
		if len(literal) >= 8 {
			text = strings.ReplaceAll(text, literal, "[REDACTED]")
		}
	}
	for _, pattern := range secretPatterns {
		text = pattern.ReplaceAllString(text, "[REDACTED]")
	}
	return text
}

// toolbox is the set of tools offered for one run.
type toolbox struct {
	order []*toolSpec
	byKey map[string]*toolSpec
}

func newToolbox(specs []*toolSpec, access string) *toolbox {
	box := &toolbox{byKey: make(map[string]*toolSpec, len(specs))}
	for _, spec := range specs {
		if access == AccessReadOnly && spec.write {
			continue
		}
		box.order = append(box.order, spec)
		box.byKey[spec.name] = spec
	}
	return box
}

func (b *toolbox) definitions() []ai.ToolDefinition {
	definitions := make([]ai.ToolDefinition, 0, len(b.order))
	for _, spec := range b.order {
		definitions = append(definitions, ai.ToolDefinition{
			Name: spec.name, Description: spec.description, Parameters: spec.parameters,
		})
	}
	return definitions
}

// toolRuntime carries per-run state shared by tools.
type toolRuntime struct {
	service *Service
	runID   string
	userID  string
	access  string

	mu     sync.Mutex
	token  string
	revoke func(context.Context)
}

// bearer opens the agent's own administrator session on first use. Product API
// calls then pass through the same authentication and authorization as a
// browser, and the session ends with the run.
func (rt *toolRuntime) bearer(ctx context.Context) (string, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.token != "" {
		return rt.token, nil
	}
	issuer := rt.service.sessions
	if issuer == nil {
		return "", errToolNotReady
	}
	isAdmin, err := rt.service.userIsAdmin(ctx, rt.userID)
	if err != nil {
		return "", err
	}
	if !isAdmin {
		return "", errors.New("the administrator who started this conversation no longer has the admin role")
	}
	token, revoke, err := issuer.IssueSession(ctx, rt.userID, agentSessionTTL)
	if err != nil {
		return "", fmt.Errorf("could not open an agent session: %w", err)
	}
	rt.token, rt.revoke = token, revoke
	return token, nil
}

func (rt *toolRuntime) close() {
	rt.mu.Lock()
	revoke := rt.revoke
	rt.token, rt.revoke = "", nil
	rt.mu.Unlock()
	if revoke == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	revoke(ctx)
}

func (s *Service) userIsAdmin(ctx context.Context, userID string) (bool, error) {
	var isAdmin bool
	if err := s.db.NewSelect().Table("users").Column("is_admin").Where("id = ?", userID).Scan(ctx, &isAdmin); err != nil {
		return false, fmt.Errorf("check administrator role: %w", err)
	}
	return isAdmin, nil
}

// execute runs one tool call and returns text for the model. Failures become
// text too, so the model can read the reason and recover.
func (b *toolbox) execute(ctx context.Context, rt *toolRuntime, call ai.ToolCall) (string, bool) {
	spec, found := b.byKey[call.Name]
	if !found {
		return fmt.Sprintf("Error: unknown tool %q.", call.Name), false
	}
	parsed, err := parseToolArgs(call.Arguments)
	if err != nil {
		return "Error: " + err.Error(), false
	}
	if spec.write && rt.access == AccessReadOnly {
		return "Error: " + errReadOnlyAccess.Error(), false
	}
	text, runErr := safeToolRun(ctx, spec, rt, parsed)
	if runErr != nil {
		return "Error: " + runErr.Error(), false
	}
	return text, true
}

func safeToolRun(ctx context.Context, spec *toolSpec, rt *toolRuntime, args toolArgs) (text string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The model gets a generic message; the details stay in the server log.
			log.Printf("admin agent tool %s panicked: %v", spec.name, recovered)
			err = fmt.Errorf("the %s tool failed unexpectedly", spec.name)
		}
	}()
	return spec.run(ctx, rt, args)
}

// buildTools lists every tool this instance can offer. GitHub tools appear only
// when a token is configured, so the model never sees a tool that cannot work.
func (s *Service) buildTools() []*toolSpec {
	specs := make([]*toolSpec, 0, 24)
	if s.github != nil {
		specs = append(specs, s.repoTools()...)
		specs = append(specs, s.pullRequestTools()...)
		specs = append(specs, s.actionsTools()...)
	}
	specs = append(specs, s.operationsTools()...)
	return specs
}

// SetInternalAPI sets the router used for the agent's calls to the OpenPost API.
func (s *Service) SetInternalAPI(handler http.Handler) {
	s.internalAPI = handler
}
