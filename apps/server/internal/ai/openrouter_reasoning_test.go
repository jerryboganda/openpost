package ai

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openRouterModelList mirrors the shapes OpenRouter's public model list uses for
// reasoning: efforts in descending order, "none" only when thinking can be
// turned off, and models with no effort control at all.
const openRouterModelList = `{"data":[
  {"id":"openai/gpt-6-luna","reasoning":{"mandatory":false,"default_enabled":true,"supported_efforts":["max","xhigh","high","medium","low","none"],"default_effort":"medium"}},
  {"id":"z-ai/glm-5.3","reasoning":{"mandatory":true,"default_enabled":true,"supported_efforts":["max","high","low"],"default_effort":"max"}},
  {"id":"google/gemini-3.5-flash","reasoning":{"mandatory":true,"supported_efforts":["high","medium","low","minimal"]}},
  {"id":"minimax/minimax-m3","reasoning":{"mandatory":false}},
  {"id":"moonshotai/kimi-k2.7-code","reasoning":{"mandatory":true,"default_enabled":true}},
  {"id":"example/anything","reasoning":{"mandatory":false,"supported_efforts":null}},
  {"id":"example/always-thinks","reasoning":{"mandatory":true,"supported_efforts":null}},
  {"id":"openai/gpt-4o"}
]}`

func newOpenRouterReasoningFixture(t *testing.T, status int, body string) (*OpenRouterReasoning, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var requests atomic.Int32
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		seen.Store(r.URL.Path + "|" + r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, err := w.Write([]byte(body))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	source := NewOpenRouterReasoning(OpenRouterConfig{APIKey: "router-key", BaseURL: server.URL + "/api/v1/"})
	return source, &requests, &seen
}

func TestOpenRouterReasoningOffersEachListedModelItsOwnEfforts(t *testing.T) {
	source, requests, seen := newOpenRouterReasoningFixture(t, http.StatusOK, openRouterModelList)

	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"openai/gpt-6-luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"OpenAI/GPT-6-Luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"openai/gpt-6-luna:batch", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"z-ai/glm-5.3", []string{"low", "high", "max"}},
		{"google/gemini-3.5-flash", []string{"minimal", "low", "medium", "high"}},
		{"minimax/minimax-m3", []string{}},
		{"moonshotai/kimi-k2.7-code", []string{}},
		{"openai/gpt-4o", []string{}},
		{"example/anything", []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{"example/always-thinks", []string{"minimal", "low", "medium", "high", "xhigh", "max"}},
	} {
		profile, ok := source.ReasoningProfile(t.Context(), tc.model)
		require.True(t, ok, tc.model)
		require.Equal(t, tc.want, profile.Names(), tc.model)
	}

	_, ok := source.ReasoningProfile(t.Context(), "nobody/unlisted-model")
	require.False(t, ok, "models OpenRouter does not list are left to the next source")

	require.EqualValues(t, 1, requests.Load(), "the list is fetched once and then served from the cache")
	require.Equal(t, "/api/v1/models|Bearer router-key", seen.Load())
}

func TestOpenRouterReasoningKeepsTheLastListWhenARefreshFailsAndBacksOff(t *testing.T) {
	var requests atomic.Int32
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, err := w.Write([]byte(openRouterModelList))
		require.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	source := NewOpenRouterReasoning(OpenRouterConfig{BaseURL: server.URL})
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	source.now = func() time.Time { return clock }

	profile, ok := source.ReasoningProfile(t.Context(), "z-ai/glm-5.3")
	require.True(t, ok)
	require.Equal(t, []string{"low", "high", "max"}, profile.Names())
	require.EqualValues(t, 1, requests.Load())

	healthy.Store(false)
	clock = clock.Add(openRouterReasoningTTL + time.Minute)
	profile, ok = source.ReasoningProfile(t.Context(), "z-ai/glm-5.3")
	require.True(t, ok, "the stale list still answers after a failed refresh")
	require.Equal(t, []string{"low", "high", "max"}, profile.Names())
	require.EqualValues(t, 2, requests.Load())

	clock = clock.Add(time.Minute)
	_, ok = source.ReasoningProfile(t.Context(), "z-ai/glm-5.3")
	require.True(t, ok)
	require.EqualValues(t, 2, requests.Load(), "a failed refresh is not retried until the back-off has passed")

	healthy.Store(true)
	clock = clock.Add(openRouterReasoningBackoff)
	_, ok = source.ReasoningProfile(t.Context(), "z-ai/glm-5.3")
	require.True(t, ok)
	require.EqualValues(t, 3, requests.Load())
}

func TestOpenRouterReasoningLeavesEverythingToTheNextSourceWhenTheListIsUnusable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"server error", http.StatusInternalServerError, `{"error":"down"}`},
		{"not json", http.StatusOK, `<html>maintenance</html>`},
		{"empty list", http.StatusOK, `{"data":[]}`},
		{"malformed list", http.StatusOK, `{"data":[{"id":"a/b","reasoning":{"supported_efforts":"high"}}]}`},
	} {
		source, requests, _ := newOpenRouterReasoningFixture(t, tc.status, tc.body)
		_, ok := source.ReasoningProfile(t.Context(), "openai/gpt-6-luna")
		require.False(t, ok, tc.name)
		require.EqualValues(t, 1, requests.Load(), tc.name)
	}
}
