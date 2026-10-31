package adminagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type agentTransport func(*http.Request) (*http.Response, error)

func (transport agentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestGitHubDispatcherCancelsExactRunID(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: agentTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "https://api.github.com/repos/owner/repo/actions/runs/12345/cancel", request.URL.String())
		return &http.Response{StatusCode: http.StatusAccepted, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	dispatcher, err := NewGitHubDispatcher(GitHubConfig{
		Repository: "owner/repo", Workflow: "agent-run.yml", Ref: "main",
		Token: "token", CallbackURL: "https://example.test/api/v1/admin/agent/callback",
		CallbackSecret: "secret", HTTPClient: client,
	})
	require.NoError(t, err)
	require.ErrorIs(t, dispatcher.Cancel(context.Background(), AgentRun{ID: "run"}), ErrGitHubRunIDPending)
	require.Zero(t, requests)
	require.NoError(t, dispatcher.Cancel(context.Background(), AgentRun{ID: "run", GitHubRunID: 12345}))
	require.Equal(t, 1, requests)
}

func lookupTestDispatcher(t *testing.T, transport agentTransport) *GitHubDispatcher {
	t.Helper()
	dispatcher, err := NewGitHubDispatcher(GitHubConfig{
		Repository: "owner/repo", Workflow: "agent-run.yml", Ref: "main", Token: "token",
		CallbackURL: "https://example.test/api/v1/admin/agent/callback", CallbackSecret: "secret",
		HTTPClient: &http.Client{Transport: transport},
	})
	require.NoError(t, err)
	return dispatcher
}

func githubTestResponse(t *testing.T, status int, data any) (*http.Response, error) {
	t.Helper()
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
}

func TestLookupGitHubRunByStoredIDVerifiesTitle(t *testing.T) {
	requests := 0
	dispatcher := lookupTestDispatcher(t, func(request *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, "/repos/owner/repo/actions/runs/12345", request.URL.Path)
		return githubTestResponse(t, http.StatusOK, GitHubRunState{
			ID: 12345, DisplayTitle: "openpost-agent-run-1", Status: "completed", Conclusion: "success",
		})
	})
	remote, err := dispatcher.LookupRun(context.Background(), AgentRun{ID: "run-1", GitHubRunID: 12345})
	require.NoError(t, err)
	require.Equal(t, int64(12345), remote.ID)
	require.Equal(t, "success", remote.Conclusion)
	require.Equal(t, 1, requests)

	_, err = dispatcher.LookupRun(context.Background(), AgentRun{ID: "different-run", GitHubRunID: 12345})
	require.ErrorIs(t, err, ErrGitHubRunUnidentified)
}

func TestLookupGitHubRunByExactTitlePaginates(t *testing.T) {
	pages := 0
	dispatcher := lookupTestDispatcher(t, func(request *http.Request) (*http.Response, error) {
		pages++
		require.Equal(t, "/repos/owner/repo/actions/workflows/agent-run.yml/runs", request.URL.Path)
		require.Equal(t, "workflow_dispatch", request.URL.Query().Get("event"))
		require.Equal(t, "100", request.URL.Query().Get("per_page"))
		require.Equal(t, ">=2026-10-31T11:55:00Z", request.URL.Query().Get("created"))
		if pages == 1 {
			other := make([]GitHubRunState, 100)
			for i := range other {
				other[i] = GitHubRunState{ID: int64(i + 1), DisplayTitle: "unrelated"}
			}
			return githubTestResponse(t, http.StatusOK, map[string]any{"total_count": 101, "workflow_runs": other})
		}
		return githubTestResponse(t, http.StatusOK, map[string]any{"total_count": 101,
			"workflow_runs": []GitHubRunState{{ID: 321, DisplayTitle: "openpost-agent-run-2", Status: "in_progress"}}})
	})
	run := AgentRun{ID: "run-2", CreatedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	remote, err := dispatcher.LookupRun(context.Background(), run)
	require.NoError(t, err)
	require.Equal(t, int64(321), remote.ID)
	require.Equal(t, 2, pages)
}

func TestLookupGitHubRunRejectsAmbiguityAndUnboundedResults(t *testing.T) {
	run := AgentRun{ID: "run-3", CreatedAt: time.Now().UTC()}
	for _, test := range []struct {
		name   string
		result any
		err    error
	}{
		{"ambiguous", map[string]any{"total_count": 2, "workflow_runs": []GitHubRunState{
			{ID: 1, DisplayTitle: "openpost-agent-run-3"}, {ID: 2, DisplayTitle: "openpost-agent-run-3"},
		}}, ErrGitHubRunAmbiguous},
		{"over-page-limit", map[string]any{"total_count": 501, "workflow_runs": []GitHubRunState{}}, ErrGitHubRunSearchLimit},
		{"not-found", map[string]any{"total_count": 0, "workflow_runs": []GitHubRunState{}}, ErrGitHubRunUnidentified},
	} {
		t.Run(test.name, func(t *testing.T) {
			dispatcher := lookupTestDispatcher(t, func(*http.Request) (*http.Response, error) {
				return githubTestResponse(t, http.StatusOK, test.result)
			})
			_, err := dispatcher.LookupRun(context.Background(), run)
			require.ErrorIs(t, err, test.err)
		})
	}
}

func TestLookupGitHubRunHTTPError(t *testing.T) {
	dispatcher := lookupTestDispatcher(t, func(*http.Request) (*http.Response, error) {
		return githubTestResponse(t, http.StatusUnauthorized, map[string]string{"message": "Unauthorized"})
	})
	_, err := dispatcher.LookupRun(context.Background(), AgentRun{ID: "run-4", GitHubRunID: 10})
	require.ErrorContains(t, err, "HTTP 401")
}
