package adminagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/stretchr/testify/require"
)

type fakeLogs struct{ lines []string }

func (f fakeLogs) Snapshot(limit int, keep func(string) bool) []string {
	var kept []string
	for _, line := range f.lines {
		if keep == nil || keep(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	return kept
}

func TestDBQueryIsReadOnlyAndMasksSecrets(t *testing.T) {
	service, _ := newAgentService(t)
	_, err := service.db.Exec(`CREATE TABLE things (id TEXT, name TEXT, api_token TEXT, password_hash TEXT, created_at DATETIME)`)
	require.NoError(t, err)
	_, err = service.db.Exec(`INSERT INTO things VALUES
		('1', 'alpha', 'tok-1', 'hash-1', '2026-10-30 10:00:00'),
		('2', 'beta',  'tok-2', 'hash-2', '2026-10-30 11:00:00'),
		('3', 'gamma', 'tok-3', 'hash-3', '2026-10-30 12:00:00')`)
	require.NoError(t, err)

	output, ok := runTool(t, service, AccessFull, "db_query", map[string]any{
		"sql": "SELECT id, name, api_token, password_hash, created_at FROM things ORDER BY id;", "limit": 2,
	})
	require.True(t, ok, output)
	require.Contains(t, output, "id\tname\tapi_token\tpassword_hash\tcreated_at")
	require.Contains(t, output, "1\talpha\t[masked]\t[masked]\t2026-10-30T10:00:00Z")
	require.NotContains(t, output, "tok-")
	require.NotContains(t, output, "hash-")
	require.NotContains(t, output, "gamma", "the limit bounds the rows")
	require.Contains(t, output, "only the first 2 are shown")

	output, ok = runTool(t, service, AccessReadOnly, "db_query", map[string]any{
		"sql": "WITH recent AS (SELECT name FROM things WHERE id > '1') SELECT COUNT(*) AS total FROM recent",
	})
	require.True(t, ok, output)
	require.Contains(t, output, "total\n2")

	for _, statement := range []string{
		"DROP TABLE things",
		"UPDATE things SET name = 'x'",
		"SELECT 1; DROP TABLE things",
		"SELECT * FROM things INTO OUTFILE '/tmp/x'",
		"  ",
	} {
		output, ok := runTool(t, service, AccessFull, "db_query", map[string]any{"sql": statement})
		require.False(t, ok, statement)
		require.Contains(t, output, "Error:")
	}
	var remaining int
	require.NoError(t, service.db.NewSelect().Table("things").ColumnExpr("COUNT(*)").Scan(context.Background(), &remaining))
	require.Equal(t, 3, remaining, "refused statements change nothing")

	output, ok = runTool(t, service, AccessFull, "db_query", map[string]any{"sql": "SELECT * FROM missing_table"})
	require.False(t, ok)
	require.Contains(t, output, "query failed")
}

func TestProductionLogsFilterProblemsAndAnswerQuestions(t *testing.T) {
	service, _ := newAgentService(t)
	service.SetLogSource(fakeLogs{lines: []string{
		`{"severity":"INFO","event":"http_request","path":"/api/v1/posts","status":200}`,
		`{"severity":"ERROR","event":"publish_failed","message":"provider timeout"}`,
		`{"severity":"INFO","event":"http_request","path":"/api/v1/ready","status":200}`,
		`{"severity":"INFO","event":"http_request","path":"/api/v1/media","status":502}`,
		`{"severity":"INFO","event":"legacy_log","message":"worker error: boom"}`,
		`{"severity":"INFO","event":"legacy_log","message":"Starting worker"}`,
		`{"severity":"WARN","event":"slow_query"}`,
		`{"severity":"ERROR","event":"http_request","path":"/api/v1/ready","status":503}`,
	}})

	output, ok := runTool(t, service, AccessReadOnly, "prod_logs", map[string]any{})
	require.True(t, ok, output)
	for _, expected := range []string{"publish_failed", `"status":502`, "worker error: boom", "slow_query", `"status":503`} {
		require.Contains(t, output, expected)
	}
	require.NotContains(t, output, `"status":200`, "healthy requests are not problems")
	require.NotContains(t, output, "Starting worker")
	require.Less(t, strings.Index(output, "publish_failed"), strings.Index(output, "slow_query"), "oldest first")

	output, _ = runTool(t, service, AccessFull, "prod_logs", map[string]any{"kind": "all", "contains": "READY"})
	require.Contains(t, output, "2 log line(s)")
	output, _ = runTool(t, service, AccessFull, "prod_logs", map[string]any{"contains": "provider"})
	require.Contains(t, output, "1 log line(s)")
	output, _ = runTool(t, service, AccessFull, "prod_logs", map[string]any{"limit": 1})
	require.Contains(t, output, `"status":503`)
	require.NotContains(t, output, "slow_query")
	output, _ = runTool(t, service, AccessFull, "prod_logs", map[string]any{"contains": "no such text"})
	require.Contains(t, output, "No matching log lines")

	status := service.productionStatus(context.Background())
	require.Contains(t, status, "Recent problems in the log buffer: 5")
	require.Contains(t, status, `Latest: {"severity":"ERROR","event":"http_request","path":"/api/v1/ready","status":503}`)

	bare, _ := newAgentService(t)
	output, ok = runTool(t, bare, AccessFull, "prod_logs", map[string]any{})
	require.False(t, ok)
	require.Contains(t, output, "not configured")
}

type fakeIssuer struct {
	mu      sync.Mutex
	issued  int
	revoked int
}

func (f *fakeIssuer) IssueSession(_ context.Context, userID string, ttl time.Duration) (string, func(context.Context), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	return "agent-token-for-" + userID, func(context.Context) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.revoked++
	}, nil
}

func (f *fakeIssuer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issued, f.revoked
}

type routedRequest struct {
	method string
	path   string
	query  string
	auth   string
	host   string
	proto  string
	body   string
}

const testSpec = `{"openapi":"3.1.0",
 "paths":{
  "/workspaces":{"get":{"operationId":"list-workspaces","summary":"List workspaces","tags":["Workspaces"]}},
  "/workspaces/{workspace_id}/publications":{"post":{"operationId":"create-publication","summary":"Create a publication",
    "parameters":[{"name":"workspace_id","in":"path","required":true,"schema":{"type":"string"}}],
    "requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/CreatePublication"}}}}}},
  "/admin/agent/conversations":{"get":{"operationId":"list-admin-agent-conversations","summary":"List administrator agent conversations"}}
 },
 "components":{"schemas":{
  "CreatePublication":{"type":"object","required":["title","renditions"],"properties":{
    "$schema":{"type":"string"},
    "title":{"type":"string","description":"Post title"},
    "status":{"type":"string","enum":["draft","scheduled"]},
    "renditions":{"type":"array","items":{"$ref":"#/components/schemas/Rendition"}}}},
  "Rendition":{"type":"object","required":["account_id"],"properties":{"account_id":{"type":"string"},"text":{"type":"string"}}}}}}`

type apiHarness struct {
	t       *testing.T
	service *Service
	issuer  *fakeIssuer
	runtime *toolRuntime
	box     *toolbox
	mu      sync.Mutex
	routed  []routedRequest
}

func newAPIHarness(t *testing.T, access, userID string) *apiHarness {
	t.Helper()
	service, _ := newAgentService(t)
	harness := &apiHarness{t: t, service: service, issuer: &fakeIssuer{}}
	service.SetSessionIssuer(harness.issuer)
	service.SetInternalAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		harness.mu.Lock()
		harness.routed = append(harness.routed, routedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
			host: r.Host, proto: r.Header.Get("X-Forwarded-Proto"), body: string(body),
		})
		harness.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/openapi.json" {
			_, _ = io.WriteString(w, testSpec)
			return
		}
		_, _ = io.WriteString(w, `{"workspaces":[]}`)
	}))
	_, err := service.db.Exec(`INSERT INTO users (id, email, is_admin) VALUES ('plain', 'plain@example.com', 0)`)
	require.NoError(t, err)
	harness.runtime = &toolRuntime{service: service, runID: "run", userID: userID, access: access}
	harness.box = newToolbox(service.buildTools(), access)
	t.Cleanup(harness.runtime.close)
	return harness
}

func (h *apiHarness) call(name string, args map[string]any) (string, bool) {
	h.t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(h.t, err)
	return h.box.execute(context.Background(), h.runtime, ai.ToolCall{ID: "call", Name: name, Arguments: string(raw)})
}

func (h *apiHarness) requests(path string) []routedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var matched []routedRequest
	for _, request := range h.routed {
		if request.path == path {
			matched = append(matched, request)
		}
	}
	return matched
}

func TestAPISearchFindsOperationsAndExplainsTheirRequestBodies(t *testing.T) {
	harness := newAPIHarness(t, AccessFull, "admin")

	output, ok := harness.call("api_search", map[string]any{"query": "list workspaces"})
	require.True(t, ok, output)
	require.Contains(t, output, "GET /api/v1/workspaces | list-workspaces | List workspaces")
	require.Less(t, strings.Index(output, "list-workspaces"), strings.Index(output, "list-admin-agent-conversations"), "better matches rank first")

	output, ok = harness.call("api_search", map[string]any{"operation_id": "create-publication"})
	require.True(t, ok, output)
	for _, expected := range []string{
		"POST /api/v1/workspaces/{workspace_id}/publications (operationId create-publication)",
		"path workspace_id: string (required)",
		"title: string  // Post title",
		"status?: string draft|scheduled",
		"renditions: [{",
		"account_id: string",
		"text?: string",
	} {
		require.Contains(t, output, expected)
	}
	require.NotContains(t, output, "$schema", "generated schema markers are not request fields")

	output, ok = harness.call("api_search", map[string]any{"operation_id": "nope"})
	require.False(t, ok)
	require.Contains(t, output, "no operation has the id")
	output, _ = harness.call("api_search", map[string]any{"query": "zebra"})
	require.Contains(t, output, "No operation matches")
	require.Len(t, harness.requests("/openapi.json"), 1, "the API description is fetched once")
}

func TestAPICallRunsThroughTheRouterAsTheAdministratorAndEndsItsSession(t *testing.T) {
	harness := newAPIHarness(t, AccessFull, "admin")

	output, ok := harness.call("api_call", map[string]any{"path": "/workspaces", "query": map[string]any{"limit": 5}})
	require.True(t, ok, output)
	require.Equal(t, "HTTP 200\n{\"workspaces\":[]}", output)
	_, ok = harness.call("api_call", map[string]any{
		"method": "POST", "path": "/api/v1/workspaces/ws-1/publications", "body": map[string]any{"title": "Launch"},
	})
	require.True(t, ok)

	requests := harness.requests("/api/v1/workspaces")
	require.Len(t, requests, 1)
	require.Equal(t, "Bearer agent-token-for-admin", requests[0].auth, "the call carries the agent's own session")
	require.Equal(t, "limit=5", requests[0].query)
	require.Equal(t, "post.example.test", requests[0].host)
	require.Equal(t, "https", requests[0].proto)
	posted := harness.requests("/api/v1/workspaces/ws-1/publications")
	require.Len(t, posted, 1)
	require.Equal(t, http.MethodPost, posted[0].method)
	require.JSONEq(t, `{"title":"Launch"}`, posted[0].body)

	issued, revoked := harness.issuer.counts()
	require.Equal(t, 1, issued, "one session serves the whole run")
	require.Zero(t, revoked)
	harness.runtime.close()
	_, revoked = harness.issuer.counts()
	require.Equal(t, 1, revoked, "the session ends with the run")
	harness.runtime.close()
	_, revoked = harness.issuer.counts()
	require.Equal(t, 1, revoked, "closing twice is harmless")
}

func TestAPICallRefusesControlEndpointsSecurityChangesAndReadOnlyWrites(t *testing.T) {
	harness := newAPIHarness(t, AccessFull, "admin")
	cases := []struct {
		name   string
		args   map[string]any
		reason string
	}{
		{"the agent's own endpoints", map[string]any{"path": "/admin/agent/conversations"}, "own control endpoints"},
		{"the agent's own endpoints with prefix", map[string]any{"path": "/api/v1/admin/agent/runs/x/cancel", "method": "POST"}, "own control endpoints"},
		{"password changes", map[string]any{"method": "POST", "path": "/auth/password"}, "cannot be changed"},
		{"backup restore", map[string]any{"method": "POST", "path": "/admin/backup/restore"}, "instance settings"},
		{"full URLs", map[string]any{"path": "https://evil.example/x"}, "not a full URL"},
		{"path traversal", map[string]any{"path": "/workspaces/../admin"}, "not valid"},
		{"empty path", map[string]any{"path": " "}, "path is required"},
		{"unknown method", map[string]any{"method": "TRACE", "path": "/workspaces"}, "method must be"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output, ok := harness.call("api_call", testCase.args)
			require.False(t, ok)
			require.Contains(t, output, testCase.reason)
		})
	}
	require.Empty(t, harness.requests("/api/v1/admin/agent/conversations"))
	issued, _ := harness.issuer.counts()
	require.Zero(t, issued, "refused calls never open a session")

	readOnly := newAPIHarness(t, AccessReadOnly, "admin")
	output, ok := readOnly.call("api_call", map[string]any{"method": "DELETE", "path": "/workspaces/ws-1"})
	require.False(t, ok)
	require.Contains(t, output, "read-only")
	_, ok = readOnly.call("api_call", map[string]any{"path": "/workspaces"})
	require.True(t, ok, "reading stays available")
}

func TestAPICallRequiresThatTheConversationOwnerIsStillAnAdministrator(t *testing.T) {
	harness := newAPIHarness(t, AccessFull, "plain")
	output, ok := harness.call("api_call", map[string]any{"path": "/workspaces"})
	require.False(t, ok)
	require.Contains(t, output, "no longer has the admin role")
	issued, _ := harness.issuer.counts()
	require.Zero(t, issued)

	bare, _ := newAgentService(t)
	output, ok = runTool(t, bare, AccessFull, "api_call", map[string]any{"path": "/workspaces"})
	require.False(t, ok)
	require.Contains(t, output, "not configured")
}
