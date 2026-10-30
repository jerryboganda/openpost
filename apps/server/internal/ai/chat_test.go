package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestChatClient(t *testing.T, handler http.HandlerFunc, headers map[string]string) *ChatClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewChatClient(OpenRouterConfig{
		Mode:          ProviderModeOpenAICompatible,
		APIKey:        "test-key",
		BaseURL:       server.URL + "/v1/",
		ProviderLabel: "OpenCode",
		Headers:       headers,
	})
	require.NoError(t, err)
	client.backoff = time.Millisecond
	return client
}

func writeSSE(t *testing.T, w http.ResponseWriter, lines ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, line := range lines {
		_, err := fmt.Fprintf(w, "data: %s\n\n", line)
		require.NoError(t, err)
	}
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	require.NoError(t, err)
}

func userTurn(text string) ChatRequest {
	return ChatRequest{Model: "deepseek-v4.1-flash", Messages: []ChatMessage{{Role: ChatRoleUser, Content: text}}}
}

func TestChatStreamsTextAndAssemblesToolCallsAcrossChunks(t *testing.T) {
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			`{"id":"gw-1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"I'll check "}}]}`,
			`{"id":"gw-1","choices":[{"index":0,"finish_reason":null,"delta":{"content":"the time."}}]}`,
			`{"id":"gw-1","choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
			`{"id":"gw-1","choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"time"}}]}}]}`,
			`{"id":"gw-1","choices":[{"index":0,"finish_reason":null,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"zone\":\"UTC\"}"}}]}}]}`,
			`{"id":"gw-1","choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}],"usage":{"prompt_tokens":307,"completion_tokens":23,"total_tokens":330}}`,
		)
	}, nil)

	var deltas []string
	result, err := client.Chat(t.Context(), userTurn("time?"), func(delta string) { deltas = append(deltas, delta) })
	require.NoError(t, err)
	require.Equal(t, []string{"I'll check ", "the time."}, deltas)
	require.Equal(t, "I'll check the time.", result.Text)
	require.Equal(t, "tool_calls", result.FinishReason)
	require.Equal(t, "deepseek-v4.1-flash", result.Model)
	require.Equal(t, int64(307), result.Usage.InputTokens)
	require.Equal(t, int64(23), result.Usage.OutputTokens)
	require.Len(t, result.ToolCalls, 1)
	require.Equal(t, ToolCall{ID: "call_a", Name: "get_time", Arguments: `{"timezone":"UTC"}`}, result.ToolCalls[0])
}

func TestChatHandlesInterleavedParallelCallsAndProviderQuirks(t *testing.T) {
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			// Kimi style: reasoning arrives in a separate field before the calls.
			`{"id":"k-1","model":"kimi-k3","choices":[{"index":0,"finish_reason":null,"delta":{"role":"assistant","content":"","reasoning":"Need two reads."}}]}`,
			// Two calls whose argument pieces interleave by index.
			`{"id":"k-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"repo_read","arguments":"{\"path\":"}},{"index":1,"id":"call-2","type":"function","function":{"name":"repo_tree","arguments":"{}"}}]}}]}`,
			`{"id":"k-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
			// A call with no id and empty arguments still becomes usable.
			`{"id":"k-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"function":{"name":"prod_status","arguments":""}}]}}]}`,
			`{"id":"k-1","choices":[{"index":0,"finish_reason":"tool_calls","delta":{"role":"assistant","content":""}}]}`,
			// Gemini style: usage arrives in a chunk with no choices.
			`{"id":"k-1","choices":[],"usage":{"prompt_tokens":98,"completion_tokens":22,"total_tokens":120,"cost":0}}`,
		)
	}, nil)

	result, err := client.Chat(t.Context(), userTurn("go"), nil)
	require.NoError(t, err)
	require.Equal(t, "Need two reads.", result.Reasoning)
	require.Equal(t, "reasoning", result.ReasoningField)
	require.Equal(t, int64(120), result.Usage.TotalTokens)
	require.Len(t, result.ToolCalls, 3)
	require.Equal(t, ToolCall{ID: "call-1", Name: "repo_read", Arguments: `{"path":"a.go"}`}, result.ToolCalls[0])
	require.Equal(t, ToolCall{ID: "call-2", Name: "repo_tree", Arguments: `{}`}, result.ToolCalls[1])
	require.Equal(t, ToolCall{ID: "call_3", Name: "prod_status", Arguments: `{}`}, result.ToolCalls[2])
}

func TestChatEncodesMessagesToolsImagesAndRoutingHeaders(t *testing.T) {
	var captured map[string]any
	var header http.Header
	client := newTestChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
		writeSSE(t, w, `{"choices":[{"index":0,"finish_reason":"stop","delta":{"content":"ok"}}]}`)
	}, map[string]string{"x-opencode-session": "session-1"})

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	_, err := client.Chat(t.Context(), ChatRequest{
		Model: "kimi-k3",
		Messages: []ChatMessage{
			{Role: ChatRoleSystem, Content: "system prompt"},
			{Role: ChatRoleUser, Content: "what is this?", Images: []Image{{Data: png, MIMEType: "image/png"}}},
			{
				Role:      ChatRoleAssistant,
				ToolCalls: []ToolCall{{ID: "call_1", Name: "repo_read", Arguments: `{"path":"a.go"}`}},
				Reasoning: "thinking", ReasoningField: "reasoning_content",
			},
			{Role: ChatRoleTool, ToolCallID: "call_1", Content: ""},
		},
		Tools:           []ToolDefinition{{Name: "repo_read", Description: "Read a file", Parameters: map[string]any{"type": "object"}}},
		MaxOutputTokens: 1234,
		ReasoningEffort: ReasoningEffortHigh,
	}, nil)
	require.NoError(t, err)

	require.Equal(t, "Bearer test-key", header.Get("Authorization"))
	require.Contains(t, header.Get("User-Agent"), "openpost-agent/")
	require.Equal(t, "session-1", header.Get("X-Opencode-Session"))
	require.Equal(t, true, captured["stream"])
	require.Equal(t, map[string]any{"include_usage": true}, captured["stream_options"])
	require.EqualValues(t, 1234, captured["max_completion_tokens"])
	require.Equal(t, "high", captured["reasoning_effort"])
	require.NotContains(t, captured, "provider", "OpenRouter routing fields must stay out of compatible requests")

	messages, ok := captured["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 4)
	user := messages[1].(map[string]any)
	parts := user["content"].([]any)
	require.Len(t, parts, 2)
	require.Equal(t, "text", parts[0].(map[string]any)["type"])
	image := parts[1].(map[string]any)
	require.Equal(t, "image_url", image["type"])
	require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png), image["image_url"].(map[string]any)["url"])

	assistant := messages[2].(map[string]any)
	require.Nil(t, assistant["content"])
	require.Equal(t, "thinking", assistant["reasoning_content"])
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	require.Equal(t, "call_1", call["id"])
	require.Equal(t, "repo_read", call["function"].(map[string]any)["name"])
	require.Equal(t, `{"path":"a.go"}`, call["function"].(map[string]any)["arguments"])

	tool := messages[3].(map[string]any)
	require.Equal(t, "call_1", tool["tool_call_id"])
	require.Equal(t, "(no output)", tool["content"], "providers reject empty tool results")

	tools := captured["tools"].([]any)
	require.Len(t, tools, 1)
	require.Equal(t, "repo_read", tools[0].(map[string]any)["function"].(map[string]any)["name"])
}

func TestChatReportsProviderDetailWithoutRetryingClientErrors(t *testing.T) {
	var requests atomic.Int32
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`))
		require.NoError(t, err)
	}, nil)

	_, err := client.Chat(t.Context(), userTurn("hi"), nil)
	var chatErr *ChatError
	require.ErrorAs(t, err, &chatErr)
	require.Equal(t, http.StatusBadRequest, chatErr.StatusCode)
	require.Contains(t, chatErr.Detail, "Model is unavailable")
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	require.Equal(t, "OpenCode", providerErr.Provider)
	require.EqualValues(t, 1, requests.Load())
}

func TestChatRetriesTransientFailuresBeforeTheStreamStarts(t *testing.T) {
	var requests atomic.Int32
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeSSE(t, w, `{"choices":[{"index":0,"finish_reason":"stop","delta":{"content":"recovered"}}]}`)
	}, nil)

	result, err := client.Chat(t.Context(), userTurn("hi"), nil)
	require.NoError(t, err)
	require.Equal(t, "recovered", result.Text)
	require.EqualValues(t, 2, requests.Load())
}

func TestChatAcceptsProvidersThatIgnoreStreaming(t *testing.T) {
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"id":"x","model":"m","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
		require.NoError(t, err)
	}, nil)

	var streamed string
	result, err := client.Chat(t.Context(), userTurn("hi"), func(delta string) { streamed += delta })
	require.NoError(t, err)
	require.Equal(t, "hello", result.Text)
	require.Equal(t, "hello", streamed)
	require.Equal(t, []ToolCall{{ID: "c1", Name: "get_time", Arguments: "{}"}}, result.ToolCalls)
	require.EqualValues(t, 3, result.Usage.TotalTokens)
}

func TestChatSurfacesErrorsSentInsideTheStream(t *testing.T) {
	client := newTestChatClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w,
			`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`,
			`{"error":{"message":"overloaded","code":529}}`,
		)
	}, nil)

	_, err := client.Chat(t.Context(), userTurn("hi"), nil)
	var chatErr *ChatError
	require.ErrorAs(t, err, &chatErr)
	require.Equal(t, 529, chatErr.StatusCode)
	require.Equal(t, "overloaded", chatErr.Detail)
}

func TestChatStopsWhenTheCallerCancels(t *testing.T) {
	started := make(chan struct{})
	client := newTestChatClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n")
		require.NoError(t, err)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()
	_, err := client.Chat(ctx, userTurn("hi"), nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled), "cancellation must be distinguishable from provider failures, got %v", err)
}

func TestChatRejectsMalformedRequestsBeforeAnyNetworkCall(t *testing.T) {
	client := newTestChatClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("no request may be sent")
	}, nil)

	_, err := client.Chat(t.Context(), ChatRequest{Messages: []ChatMessage{{Role: ChatRoleUser, Content: "x"}}}, nil)
	require.ErrorContains(t, err, "model is required")
	_, err = client.Chat(t.Context(), ChatRequest{Model: "m"}, nil)
	require.ErrorContains(t, err, "messages are required")
	_, err = client.Chat(t.Context(), ChatRequest{Model: "m", Messages: []ChatMessage{{Role: ChatRoleTool, Content: "x"}}}, nil)
	require.ErrorContains(t, err, "tool call id")
	_, err = client.Chat(t.Context(), ChatRequest{Model: "m", Messages: []ChatMessage{{
		Role: ChatRoleUser, Content: "x", Images: []Image{{Data: []byte("x"), MIMEType: "text/html"}},
	}}}, nil)
	require.ErrorContains(t, err, "image MIME type")
}
