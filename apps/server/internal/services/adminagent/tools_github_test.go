package adminagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/stretchr/testify/require"
)

type recordedRequest struct {
	method string
	path   string
	query  string
	accept string
	auth   string
	body   []byte
}

type fakeRoute func(w http.ResponseWriter, r *http.Request)

// fakeGitHub answers a fixed set of GitHub REST routes and records every call.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (f *fakeGitHub) calls(method, path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []recordedRequest
	for _, request := range f.requests {
		if request.method == method && request.path == path {
			matched = append(matched, request)
		}
	}
	return matched
}

func (f *fakeGitHub) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// githubService wires a service to a fake GitHub API. Routes are keyed by
// "METHOD /path"; an unrouted request fails the test.
func githubService(t *testing.T, routes map[string]fakeRoute) (*Service, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.requests = append(fake.requests, recordedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			accept: r.Header.Get("Accept"), auth: r.Header.Get("Authorization"), body: body,
		})
		fake.mu.Unlock()
		route, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.Error(w, `{"message":"unexpected route"}`, http.StatusTeapot)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		route(w, r)
	}))
	t.Cleanup(server.Close)
	service, _ := newAgentService(t)
	require.NoError(t, service.SetGitHub("test-token", "owner/repo", "main"))
	service.github.base = server.URL
	return service, fake
}

func jsonRoute(status int, body string) fakeRoute {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func runTool(t *testing.T, service *Service, access, name string, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	require.NoError(t, err)
	box := newToolbox(service.buildTools(), access)
	runtime := &toolRuntime{service: service, runID: "run", userID: "admin", access: access}
	defer runtime.close()
	return box.execute(context.Background(), runtime, ai.ToolCall{ID: "call", Name: name, Arguments: string(raw)})
}

func encodedFile(sha, content string) string {
	return fmt.Sprintf(`{"type":"file","sha":%q,"size":%d,"encoding":"base64","content":%q}`,
		sha, len(content), base64.StdEncoding.EncodeToString([]byte(content)))
}

func TestRepoEditCreatesTheBranchEditsOneFileAndCommitsAsTheAgent(t *testing.T) {
	var branchExists atomic.Bool
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/git/ref/heads/agent/fix": func(w http.ResponseWriter, r *http.Request) {
			if branchExists.Load() {
				jsonRoute(200, `{"object":{"sha":"base1234"}}`)(w, r)
				return
			}
			jsonRoute(404, `{"message":"Not Found"}`)(w, r)
		},
		"GET /repos/owner/repo/git/ref/heads/main": jsonRoute(200, `{"object":{"sha":"base1234"}}`),
		"POST /repos/owner/repo/git/refs": func(w http.ResponseWriter, r *http.Request) {
			branchExists.Store(true)
			jsonRoute(201, `{}`)(w, r)
		},
		"GET /repos/owner/repo/contents/apps/a.txt": jsonRoute(200, encodedFile("blob1", "hello world\nhello world\n")),
		"PUT /repos/owner/repo/contents/apps/a.txt": jsonRoute(200, `{"commit":{"sha":"c0ffee1234567"}}`),
	})

	output, ok := runTool(t, service, AccessFull, "repo_edit", map[string]any{
		"branch": "agent/fix", "path": "apps/a.txt", "old_string": "hello world", "new_string": "goodbye",
	})
	require.False(t, ok)
	require.Contains(t, output, "appears 2 times")
	require.Empty(t, fake.calls("PUT", "/repos/owner/repo/contents/apps/a.txt"), "an ambiguous edit changes nothing")

	output, ok = runTool(t, service, AccessFull, "repo_edit", map[string]any{
		"branch": "agent/fix", "path": "apps/a.txt", "old_string": "hello world\nhello world", "new_string": "goodbye",
	})
	require.True(t, ok, output)
	require.Contains(t, output, "Commit c0ffee1")

	created := fake.calls("POST", "/repos/owner/repo/git/refs")
	require.Len(t, created, 1)
	require.JSONEq(t, `{"ref":"refs/heads/agent/fix","sha":"base1234"}`, string(created[0].body))

	puts := fake.calls("PUT", "/repos/owner/repo/contents/apps/a.txt")
	require.Len(t, puts, 1)
	require.Equal(t, "Bearer test-token", puts[0].auth)
	var payload struct {
		Message   string            `json:"message"`
		Content   string            `json:"content"`
		Branch    string            `json:"branch"`
		SHA       string            `json:"sha"`
		Committer map[string]string `json:"committer"`
	}
	require.NoError(t, json.Unmarshal(puts[0].body, &payload))
	decoded, err := base64.StdEncoding.DecodeString(payload.Content)
	require.NoError(t, err)
	require.Equal(t, "goodbye\n", string(decoded))
	require.Equal(t, "agent/fix", payload.Branch)
	require.Equal(t, "blob1", payload.SHA, "the commit is based on the file version that was read")
	require.Equal(t, "agent: edit apps/a.txt", payload.Message)
	require.Equal(t, "OpenPost Agent", payload.Committer["name"])
}

func TestRepoEditHandlesCRLFFilesAndReplaceAll(t *testing.T) {
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/git/ref/heads/agent/crlf": jsonRoute(200, `{"object":{"sha":"x"}}`),
		"GET /repos/owner/repo/contents/a.txt":           jsonRoute(200, encodedFile("blob2", "one\r\ntwo\r\none\r\n")),
		"PUT /repos/owner/repo/contents/a.txt":           jsonRoute(200, `{"commit":{"sha":"abcdef1234"}}`),
	})
	output, ok := runTool(t, service, AccessFull, "repo_edit", map[string]any{
		"branch": "agent/crlf", "path": "a.txt", "old_string": "one\n", "new_string": "ONE\n", "replace_all": true,
	})
	require.True(t, ok, output)
	require.Contains(t, output, "2 replacement(s)")
	var payload struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(fake.calls("PUT", "/repos/owner/repo/contents/a.txt")[0].body, &payload))
	decoded, err := base64.StdEncoding.DecodeString(payload.Content)
	require.NoError(t, err)
	require.Equal(t, "ONE\r\ntwo\r\nONE\r\n", string(decoded), "the file keeps its line endings")
}

func TestWritesNeverReachTheDefaultBranchSecretsOrReadOnlyRuns(t *testing.T) {
	service, fake := githubService(t, nil)
	cases := []struct {
		name     string
		tool     string
		args     map[string]any
		contains string
	}{
		{"default branch", "repo_write", map[string]any{"branch": "main", "path": "a.txt", "content": "x"}, "pull request"},
		{"environment file", "repo_write", map[string]any{"branch": "agent/x", "path": "apps/.env.production", "content": "A=1"}, "never committed"},
		{"private key file", "repo_write", map[string]any{"branch": "agent/x", "path": "deploy/id_rsa", "content": "x"}, "private key"},
		{"workflow file", "repo_write", map[string]any{"branch": "agent/x", "path": ".github/workflows/exfiltrate.yml", "content": "on: push"}, "run with the repository's secrets"},
		{"workflow file in another case", "repo_edit", map[string]any{"branch": "agent/x", "path": ".GitHub/Workflows/ci.yml", "old_string": "a", "new_string": "b"}, "run with the repository's secrets"},
		{"deployment script", "repo_delete", map[string]any{"branch": "agent/x", "path": "deploy/vps/deploy.sh"}, "run with the repository's secrets"},
		{"git internals", "repo_delete", map[string]any{"branch": "agent/x", "path": ".git/config"}, ".git"},
		{"path traversal", "repo_write", map[string]any{"branch": "agent/x", "path": "../etc/passwd", "content": "x"}, "segments"},
		{"bad branch name", "repo_write", map[string]any{"branch": "agent/..x", "path": "a.txt", "content": "x"}, "branch name"},
		{"token in new file", "repo_write", map[string]any{"branch": "agent/x", "path": "a.txt", "content": "token = gho_abcdefghijklmnopqrstuvwxyz0123456789"}, "credential"},
		{"token in an edit", "repo_edit", map[string]any{"branch": "agent/x", "path": "a.txt", "old_string": "a", "new_string": "sk-abcdefghijklmnopqrstuvwxyz0123456789abcd"}, "credential"},
		{"identical edit", "repo_edit", map[string]any{"branch": "agent/x", "path": "a.txt", "old_string": "a", "new_string": "a"}, "identical"},
		{"oversized file", "repo_write", map[string]any{"branch": "agent/x", "path": "a.txt", "content": strings.Repeat("x", maxWriteFileBytes+1)}, "larger than 1 MB"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			output, ok := runTool(t, service, AccessFull, testCase.tool, testCase.args)
			require.False(t, ok)
			require.Contains(t, output, testCase.contains)
		})
	}
	require.Zero(t, fake.total(), "refused writes never reach GitHub")

	output, ok := runTool(t, service, AccessReadOnly, "repo_write", map[string]any{"branch": "agent/x", "path": "a.txt", "content": "x"})
	require.False(t, ok)
	require.Contains(t, output, "unknown tool", "read-only runs are not even offered write tools")
}

func TestRepoReadNumbersLinesAndPagesThroughLongFiles(t *testing.T) {
	var lines []string
	for number := 1; number <= 1000; number++ {
		lines = append(lines, fmt.Sprintf("line %d", number))
	}
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/contents/docs/long.md": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.github.raw+json; charset=utf-8")
			_, _ = io.WriteString(w, strings.Join(lines, "\n")+"\n")
		},
		"GET /repos/owner/repo/contents/docs":     jsonRoute(200, `[{"name":"long.md","type":"file","size":10}]`),
		"GET /repos/owner/repo/contents/bin/blob": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte{0x89, 0x00, 0x01}) },
	})

	output, ok := runTool(t, service, AccessFull, "repo_read", map[string]any{"path": "docs/long.md", "start_line": 5, "end_line": 7})
	require.True(t, ok, output)
	require.Contains(t, output, "lines 5-7 of 1000")
	require.Contains(t, output, "5\tline 5\n6\tline 6\n7\tline 7\n")
	require.Equal(t, rawContentMimeType, fake.calls("GET", "/repos/owner/repo/contents/docs/long.md")[0].accept)

	output, _ = runTool(t, service, AccessFull, "repo_read", map[string]any{"path": "docs/long.md"})
	require.Contains(t, output, "lines 1-400 of 1000")
	require.Contains(t, output, "600 more lines. Call again with start_line=401")

	output, ok = runTool(t, service, AccessFull, "repo_read", map[string]any{"path": "docs"})
	require.False(t, ok)
	require.Contains(t, output, "is a directory")
	output, _ = runTool(t, service, AccessFull, "repo_read", map[string]any{"path": "bin/blob"})
	require.Contains(t, output, "binary file")
	output, _ = runTool(t, service, AccessFull, "repo_read", map[string]any{"path": "docs/long.md", "start_line": 2000})
	require.Contains(t, output, "past the end")
}

func TestRepoTreeListsDirectoriesAndFindsFilesWithACachedListing(t *testing.T) {
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/contents/apps":      jsonRoute(200, `[{"name":"web","type":"dir","size":0},{"name":"README.md","type":"file","size":42},{"name":"server","type":"dir","size":0}]`),
		"GET /repos/owner/repo/git/ref/heads/main": jsonRoute(200, `{"object":{"sha":"abc123"}}`),
		"GET /repos/owner/repo/git/trees/abc123": jsonRoute(200, `{"truncated":false,"tree":[
			{"path":"apps/web/src/routes/agent/+page.svelte","type":"blob"},
			{"path":"apps/web/src/lib/agent-card.svelte","type":"blob"},
			{"path":"apps/server/internal/ai/chat.go","type":"blob"},
			{"path":"apps/web","type":"tree"}]}`),
	})

	output, ok := runTool(t, service, AccessFull, "repo_tree", map[string]any{"path": "apps"})
	require.True(t, ok, output)
	require.Less(t, strings.Index(output, "server/"), strings.Index(output, "README.md"), "directories are listed first")

	output, _ = runTool(t, service, AccessFull, "repo_tree", map[string]any{"pattern": "*.svelte"})
	require.Contains(t, output, "2 file(s) match")
	require.Contains(t, output, "apps/web/src/routes/agent/+page.svelte")
	require.NotContains(t, output, "chat.go")
	output, _ = runTool(t, service, AccessFull, "repo_tree", map[string]any{"pattern": "internal/**/*.go"})
	require.Contains(t, output, "apps/server/internal/ai/chat.go")
	output, _ = runTool(t, service, AccessFull, "repo_tree", map[string]any{"pattern": "nothing-like-this"})
	require.Contains(t, output, "No file paths match")
	require.Len(t, fake.calls("GET", "/repos/owner/repo/git/trees/abc123"), 1, "the listing is cached per commit")
}

func TestRepoSearchFormatsCodeSearchMatches(t *testing.T) {
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /search/code": jsonRoute(200, `{"total_count":2,"items":[{"path":"apps/a.go","text_matches":[{"fragment":"func   Foo()\n{}"}]},{"path":"apps/b.go","text_matches":[]}]}`),
	})
	output, ok := runTool(t, service, AccessFull, "repo_search", map[string]any{"query": "Foo", "path": "apps"})
	require.True(t, ok, output)
	require.Contains(t, output, "2 match(es)")
	require.Contains(t, output, "apps/a.go\n  func Foo() {}")
	request := fake.calls("GET", "/search/code")[0]
	require.Contains(t, request.query, "repo%3Aowner%2Frepo")
	require.Contains(t, request.query, "path%3Aapps")
	require.Equal(t, "application/vnd.github.text-match+json", request.accept)
}

func TestPullRequestToolsGateMergesOnChecks(t *testing.T) {
	failing := `{"check_runs":[{"name":"Backend","status":"completed","conclusion":"success"},{"name":"Lint","status":"completed","conclusion":"failure"},{"name":"E2E","status":"in_progress","conclusion":null}]}`
	passing := `{"check_runs":[{"name":"Backend","status":"completed","conclusion":"success"}]}`
	var checks atomic.Value
	checks.Store(failing)
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/pulls/7":       jsonRoute(200, `{"number":7,"title":"Fix login","state":"open","draft":false,"merged":false,"mergeable":true,"mergeable_state":"clean","html_url":"https://github.com/owner/repo/pull/7","additions":5,"deletions":2,"changed_files":1,"user":{"login":"agent"},"head":{"ref":"agent/fix","sha":"deadbeefcafe0000"},"base":{"ref":"main"}}`),
		"GET /repos/owner/repo/pulls/7/files": jsonRoute(200, `[{"filename":"apps/a.go","status":"modified","additions":5,"deletions":2}]`),
		"GET /repos/owner/repo/commits/deadbeefcafe0000/check-runs": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			body, _ := checks.Load().(string)
			_, _ = io.WriteString(w, body)
		},
		"PUT /repos/owner/repo/pulls/7/merge": jsonRoute(200, `{"merged":true,"sha":"mergedsha1234"}`),
		"POST /repos/owner/repo/pulls":        jsonRoute(201, `{"number":8,"html_url":"https://github.com/owner/repo/pull/8"}`),
	})

	output, ok := runTool(t, service, AccessFull, "pr_get", map[string]any{"number": 7})
	require.True(t, ok, output)
	for _, expected := range []string{"#7 Fix login", "Mergeable: true (clean)", "modified apps/a.go (+5 −2)", "1 passed, 1 failed, 1 pending", "failed: Lint (failure)", "pending: E2E"} {
		require.Contains(t, output, expected)
	}

	output, ok = runTool(t, service, AccessFull, "pr_merge", map[string]any{"number": 7})
	require.False(t, ok)
	require.Contains(t, output, "not merged: 1 passed, 1 failed, 1 pending")
	require.Empty(t, fake.calls("PUT", "/repos/owner/repo/pulls/7/merge"), "failing checks block the merge")

	output, ok = runTool(t, service, AccessFull, "pr_merge", map[string]any{"number": 7, "ignore_checks": true, "method": "rebase"})
	require.True(t, ok, output)
	require.Contains(t, output, "Merged pull request #7 into main")
	require.Contains(t, output, "fork-release.yml")
	require.JSONEq(t, `{"merge_method":"rebase","sha":"deadbeefcafe0000"}`, string(fake.calls("PUT", "/repos/owner/repo/pulls/7/merge")[0].body))

	checks.Store(passing)
	_, ok = runTool(t, service, AccessFull, "pr_merge", map[string]any{"number": 7})
	require.True(t, ok, "passing checks merge without an override")

	output, ok = runTool(t, service, AccessFull, "pr_open", map[string]any{"branch": "agent/fix", "title": "fix: login", "body": "Why and how."})
	require.True(t, ok, output)
	require.Contains(t, output, "pull request #8 from agent/fix into main")
	require.JSONEq(t, `{"title":"fix: login","head":"agent/fix","base":"main","body":"Why and how.","draft":false}`,
		string(fake.calls("POST", "/repos/owner/repo/pulls")[0].body))
	_, ok = runTool(t, service, AccessFull, "pr_open", map[string]any{"branch": "main", "title": "x"})
	require.False(t, ok, "a pull request cannot start from the default branch")
}

func TestActionsToolsReadRunsFailureLogsAndStartWorkflows(t *testing.T) {
	previous := dispatchLookupDelay
	dispatchLookupDelay = time.Millisecond
	t.Cleanup(func() { dispatchLookupDelay = previous })
	now := time.Now().UTC().Format(time.RFC3339)
	log := "2026-10-30T09:00:00.1234567Z ##[group]Run go test\n" +
		"2026-10-30T09:00:01.0000000Z \x1b[36;1mgo test ./...\x1b[0m\n" +
		"2026-10-30T09:00:02.0000000Z --- FAIL: TestThing (0.01s)\n" +
		"2026-10-30T09:00:03.0000000Z ##[error]Process completed with exit code 1.\n" +
		"2026-10-30T09:00:04.0000000Z cleanup done\n"
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/actions/workflows/fork-release.yml/runs": jsonRoute(200, `{"workflow_runs":[{"id":41,"name":"Fork Release","display_title":"Merge pull request #7","head_branch":"main","head_sha":"abcdef1234567","event":"push","status":"completed","conclusion":"success","html_url":"https://github.com/owner/repo/actions/runs/41","created_at":"`+now+`"}]}`),
		"GET /repos/owner/repo/actions/runs/50":                         jsonRoute(200, `{"id":50,"name":"CI","display_title":"t","head_branch":"agent/fix","head_sha":"abc","event":"pull_request","status":"completed","conclusion":"failure","html_url":"https://github.com/owner/repo/actions/runs/50","created_at":"`+now+`"}`),
		"GET /repos/owner/repo/actions/runs/50/jobs":                    jsonRoute(200, `{"jobs":[{"id":99,"name":"Backend","status":"completed","conclusion":"failure","steps":[{"name":"Set up","conclusion":"success"},{"name":"Run tests","conclusion":"failure"}]}]}`),
		"GET /repos/owner/repo/actions/jobs/99/logs": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, log)
		},
		"POST /repos/owner/repo/actions/workflows/fork-regenerate.yml/dispatches": jsonRoute(204, ``),
		"GET /repos/owner/repo/actions/workflows/fork-regenerate.yml/runs":        jsonRoute(200, `{"workflow_runs":[{"id":77,"name":"Fork Regenerate","display_title":"Fork Regenerate","head_branch":"agent/fix","head_sha":"abc","event":"workflow_dispatch","status":"queued","html_url":"https://github.com/owner/repo/actions/runs/77","created_at":"`+now+`"}]}`),
		"POST /repos/owner/repo/actions/runs/50/cancel":                           jsonRoute(202, `{}`),
	})

	output, ok := runTool(t, service, AccessFull, "ci_status", map[string]any{"workflow": "fork-release.yml", "limit": 3})
	require.True(t, ok, output)
	require.Contains(t, output, "#41 Fork Release | main | push | success")
	require.Contains(t, fake.calls("GET", "/repos/owner/repo/actions/workflows/fork-release.yml/runs")[0].query, "per_page=3")

	output, _ = runTool(t, service, AccessFull, "ci_status", map[string]any{"run_id": 50})
	require.Contains(t, output, "job 99 Backend: failure")
	require.Contains(t, output, "failed step: Run tests")

	output, ok = runTool(t, service, AccessFull, "ci_logs", map[string]any{"job_id": 99})
	require.True(t, ok, output)
	require.Contains(t, output, "Error lines found in the log:")
	require.Contains(t, output, "--- FAIL: TestThing")
	require.Contains(t, output, "##[error]Process completed with exit code 1.")
	require.NotContains(t, output, "\x1b", "terminal colors are stripped")
	require.NotContains(t, output, "2026-10-30T09", "timestamps are stripped")
	output, _ = runTool(t, service, AccessFull, "ci_logs", map[string]any{"job_id": 99, "grep": "cleanup"})
	require.Contains(t, output, "1 line(s) contain")
	require.Contains(t, output, "cleanup done")

	output, ok = runTool(t, service, AccessFull, "ci_dispatch", map[string]any{
		"workflow": "fork-regenerate.yml", "ref": "agent/fix", "inputs": map[string]any{"branch": "agent/fix"},
	})
	require.True(t, ok, output)
	require.Contains(t, output, "#77 Fork Regenerate")
	require.Contains(t, output, "https://github.com/owner/repo/actions/runs/77")
	require.JSONEq(t, `{"ref":"agent/fix","inputs":{"branch":"agent/fix"}}`,
		string(fake.calls("POST", "/repos/owner/repo/actions/workflows/fork-regenerate.yml/dispatches")[0].body))
	_, ok = runTool(t, service, AccessFull, "ci_dispatch", map[string]any{"workflow": "../../etc/passwd"})
	require.False(t, ok)

	output, ok = runTool(t, service, AccessFull, "ci_control", map[string]any{"run_id": 50, "action": "cancel"})
	require.True(t, ok, output)
	_, ok = runTool(t, service, AccessFull, "ci_control", map[string]any{"run_id": 50, "action": "explode"})
	require.False(t, ok)
}

func TestPipelineFilesAreEditableOnlyWhenTheOwnerAllowsIt(t *testing.T) {
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/git/ref/heads/agent/ci":          jsonRoute(200, `{"object":{"sha":"base1234"}}`),
		"GET /repos/owner/repo/contents/.github/workflows/a.yml": jsonRoute(404, `{"message":"Not Found"}`),
		"PUT /repos/owner/repo/contents/.github/workflows/a.yml": jsonRoute(201, `{"commit":{"sha":"abcdef1234"}}`),
	})
	args := map[string]any{"branch": "agent/ci", "path": ".github/workflows/a.yml", "content": "name: a\n"}

	output, ok := runTool(t, service, AccessFull, "repo_write", args)
	require.False(t, ok)
	require.Contains(t, output, "OPENPOST_AGENT_ALLOW_PIPELINE_EDITS")
	require.Zero(t, fake.total(), "a refused write never reaches GitHub")

	service.AllowPipelineEdits(true)
	output, ok = runTool(t, service, AccessFull, "repo_write", args)
	require.True(t, ok, output)
	require.Len(t, fake.calls("PUT", "/repos/owner/repo/contents/.github/workflows/a.yml"), 1)

	_, ok = runTool(t, service, AccessFull, "repo_write", map[string]any{"branch": "agent/ci", "path": ".github/.env", "content": "A=1"})
	require.False(t, ok, "allowing pipeline edits never allows credentials")
}

func TestGenericGitHubToolGuardsTheRepositoryDefaultBranchAndSecrets(t *testing.T) {
	service, fake := githubService(t, map[string]fakeRoute{
		"GET /repos/owner/repo/issues":                                  jsonRoute(200, `[{"number":1}]`),
		"POST /repos/owner/repo/issues":                                 jsonRoute(201, `{"number":2}`),
		"DELETE /repos/owner/repo/git/refs/heads/agent/keys-cleanup":    jsonRoute(204, ``),
		"GET /repos/owner/repo/contents/a.txt":                          jsonRoute(200, `{}`),
		"PUT /repos/owner/repo/pulls/7/merge":                           jsonRoute(200, `{"merged":true}`),
		"GET /repos/owner/repo/git/refs/heads/main":                     jsonRoute(200, `{}`),
		"POST /repos/owner/repo/git/refs":                               jsonRoute(201, `{}`),
		"PATCH /repos/owner/repo/git/refs/heads/maintenance-fix":        jsonRoute(200, `{}`),
	})

	output, ok := runTool(t, service, AccessReadOnly, "github_api", map[string]any{"path": "/repos/{repo}/issues"})
	require.True(t, ok, output)
	require.Contains(t, output, `HTTP 200`)
	output, ok = runTool(t, service, AccessFull, "github_api", map[string]any{
		"method": "POST", "path": "/repos/owner/repo/issues", "body": map[string]any{"title": "Track the login bug"},
	})
	require.True(t, ok, output)
	require.JSONEq(t, `{"title":"Track the login bug"}`, string(fake.calls("POST", "/repos/owner/repo/issues")[0].body))

	allowedCalls := []struct {
		name string
		args map[string]any
	}{
		{"reading files and refs", map[string]any{"path": "/repos/owner/repo/contents/a.txt"}},
		{"reading the default branch ref", map[string]any{"path": "/repos/owner/repo/git/refs/heads/main"}},
		{"merging a pull request", map[string]any{"method": "PUT", "path": "/repos/owner/repo/pulls/7/merge"}},
		{"creating a branch", map[string]any{"method": "POST", "path": "/repos/owner/repo/git/refs", "body": map[string]any{"ref": "refs/heads/agent/x", "sha": "abc"}}},
		{"a branch named after a protected section", map[string]any{"method": "DELETE", "path": "/repos/owner/repo/git/refs/heads/agent/keys-cleanup"}},
		{"a branch that only starts like the default branch", map[string]any{"method": "PATCH", "path": "/repos/owner/repo/git/refs/heads/maintenance-fix", "body": map[string]any{"force": true}}},
	}
	for _, testCase := range allowedCalls {
		t.Run("allows "+testCase.name, func(t *testing.T) {
			output, ok := runTool(t, service, AccessFull, "github_api", testCase.args)
			require.True(t, ok, output)
			require.Contains(t, output, "HTTP 2")
		})
	}
	allowed := fake.total()

	blocked := []struct {
		name   string
		access string
		args   map[string]any
		reason string
	}{
		{"read-only write", AccessReadOnly, map[string]any{"method": "POST", "path": "/repos/owner/repo/issues", "body": map[string]any{"title": "x"}}, "read-only"},
		{"repository settings", AccessFull, map[string]any{"method": "PATCH", "path": "/repos/owner/repo", "body": map[string]any{"private": true}}, "repository itself"},
		{"repository deletion", AccessFull, map[string]any{"method": "DELETE", "path": "/repos/owner/repo"}, "repository itself"},
		{"secrets", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/actions/secrets/KEY", "body": map[string]any{"encrypted_value": "x"}}, "secrets"},
		{"variables", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/actions/variables", "body": map[string]any{"name": "A", "value": "b"}}, "secrets and variables"},
		{"environment secrets", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/environments/prod/secrets/KEY"}, "environments"},
		{"webhooks", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/hooks", "body": map[string]any{"config": map[string]any{"url": "https://example.com"}}}, "webhooks"},
		{"deploy keys", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/keys", "body": map[string]any{"key": "ssh-ed25519 AAAA"}}, "deploy keys"},
		{"collaborators", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/collaborators/someone"}, "collaborators"},
		{"workflow permissions", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/actions/permissions", "body": map[string]any{"enabled": true}}, "managed by the administrator"},
		{"default branch ref", AccessFull, map[string]any{"method": "PATCH", "path": "/repos/owner/repo/git/refs/heads/main", "body": map[string]any{"force": true}}, "pull requests"},
		{"default branch protection", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/branches/main/protection"}, "pull requests"},
		{"default branch rename", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/branches/main/rename", "body": map[string]any{"new_name": "x"}}, "pull requests"},
		{"merging into the default branch directly", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/merges", "body": map[string]any{"base": "main", "head": "agent/x"}}, "pull requests"},
		{"syncing the default branch from upstream", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/merge-upstream", "body": map[string]any{"branch": "main"}}, "pull requests"},
		{"file contents on a feature branch", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/contents/.github/workflows/x.yml", "body": map[string]any{"message": "m", "content": "eA==", "branch": "agent/x"}}, "repo_write"},
		{"file contents on the default branch", AccessFull, map[string]any{"method": "DELETE", "path": "/repos/owner/repo/contents/a.txt", "body": map[string]any{"branch": "main", "sha": "x", "message": "m"}}, "repo_write"},
		{"raw git blobs", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/git/blobs", "body": map[string]any{"content": "x"}}, "repo_write"},
		{"raw git trees", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/git/trees", "body": map[string]any{"tree": []any{}}}, "repo_write"},
		{"raw git commits", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/git/commits", "body": map[string]any{"message": "m"}}, "repo_write"},
		{"an encoded path", AccessFull, map[string]any{"method": "PUT", "path": "/repos/owner/repo/%63ontents/a.txt", "body": map[string]any{"branch": "agent/x"}}, "plain endpoint path"},
		{"a path that climbs out of its section", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/issues/../hooks"}, "plain endpoint path"},
		{"a query inside the path", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/issues?x=/../hooks"}, "plain endpoint path"},
		{"another repository", AccessFull, map[string]any{"path": "/repos/someone/else/issues"}, "only /repos/owner/repo"},
		{"account endpoints", AccessFull, map[string]any{"path": "/user/keys"}, "only /repos/owner/repo"},
		{"credential in a body", AccessFull, map[string]any{"method": "POST", "path": "/repos/owner/repo/issues", "body": map[string]any{"body": "ghp_abcdefghijklmnopqrstuvwxyz0123456789"}}, "credential"},
	}
	for _, testCase := range blocked {
		t.Run(testCase.name, func(t *testing.T) {
			output, ok := runTool(t, service, testCase.access, "github_api", testCase.args)
			require.False(t, ok)
			require.Contains(t, output, testCase.reason)
		})
	}
	require.Equal(t, allowed, fake.total(), "blocked requests never reach GitHub")
}

func TestPathMatcher(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		match   bool
	}{
		{"agent", "apps/web/src/routes/agent/+page.svelte", true},
		{"AGENT", "apps/web/src/routes/agent/+page.svelte", true},
		{"*.svelte", "apps/web/src/lib/a.svelte", true},
		{"*.svelte", "apps/web/src/lib/a.ts", false},
		{"*.svelte", "a.svelte.d.ts", false},
		{"internal/**/*.go", "apps/server/internal/ai/chat.go", true},
		{"internal/*.go", "apps/server/internal/ai/chat.go", false},
		{"chat.?o", "apps/server/internal/ai/chat.go", true},
		{"a+b.go", "dir/a+b.go", true},
	}
	for _, testCase := range cases {
		match, err := pathMatcher(testCase.pattern)
		require.NoError(t, err)
		require.Equal(t, testCase.match, match(testCase.path), "%s against %s", testCase.pattern, testCase.path)
	}
}
