package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	chatUserAgent      = "openpost-agent/1.0 (+https://github.com/jerryboganda/openpost)"
	chatMaxErrorBody   = 4 << 10
	chatMaxResponse    = 16 << 20
	chatMaxAttempts    = 3
	chatIdleTimeout    = 3 * time.Minute
	chatRetryBackoff   = 2 * time.Second
	chatMaxToolNameLen = 64
)

// ChatRole identifies the author of one chat message.
type ChatRole string

const (
	ChatRoleSystem    ChatRole = "system"
	ChatRoleUser      ChatRole = "user"
	ChatRoleAssistant ChatRole = "assistant"
	ChatRoleTool      ChatRole = "tool"
)

// ToolDefinition describes one function the model may call. Parameters is a
// JSON Schema object.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is one function call requested by the model. Arguments holds the
// raw JSON object text exactly as the model produced it.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatMessage is one provider-neutral message in a tool-calling conversation.
type ChatMessage struct {
	Role    ChatRole
	Content string
	// Images are sent with user messages only.
	Images []Image
	// ToolCalls are set on assistant messages that requested tools.
	ToolCalls []ToolCall
	// ToolCallID binds a tool message to the call it answers.
	ToolCallID string
	// Reasoning echoes provider reasoning text on assistant tool-call messages.
	// ReasoningField names the field the provider used, such as
	// reasoning_content, so it is returned the way it arrived.
	Reasoning      string
	ReasoningField string
}

// ChatRequest is one model turn in a tool-calling conversation.
type ChatRequest struct {
	Model           string
	Messages        []ChatMessage
	Tools           []ToolDefinition
	MaxOutputTokens int64
	// ReasoningEffort is omitted from the request when empty.
	ReasoningEffort ReasoningEffort
}

// ChatResult is the assembled answer for one model turn.
type ChatResult struct {
	Text           string
	ToolCalls      []ToolCall
	FinishReason   string
	Model          string
	RequestID      string
	Usage          Usage
	Reasoning      string
	ReasoningField string
}

// ChatGenerator runs one streamed tool-calling turn. onText receives text
// deltas as they arrive and may be nil.
type ChatGenerator interface {
	Chat(ctx context.Context, request ChatRequest, onText func(delta string)) (ChatResult, error)
}

// ChatError carries a bounded provider message for administrator diagnostics.
// It unwraps to the sanitized ProviderError used by the other AI features.
type ChatError struct {
	ProviderError
	Detail string
}

func (e *ChatError) Error() string {
	if e.Detail == "" {
		return e.ProviderError.Error()
	}
	return e.ProviderError.Error() + ": " + e.Detail
}

func (e *ChatError) Unwrap() error { return &e.ProviderError }

// ChatClient talks to OpenAI-compatible chat completion endpoints with
// streaming, tool calls, and image input.
type ChatClient struct {
	mode       ProviderMode
	apiKey     string
	baseURL    string
	label      string
	headers    map[string]string
	provider   string
	requireZDR bool
	client     HTTPClient
	idle       time.Duration
	backoff    time.Duration
}

var _ ChatGenerator = (*ChatClient)(nil)

// NewChatClient reuses the provider configuration of the other AI features so
// the agent follows the same endpoint, key, and routing headers.
func NewChatClient(config OpenRouterConfig) (*ChatClient, error) {
	mode, err := resolveProviderMode(config.Mode)
	if err != nil {
		return nil, err
	}
	apiKey, err := resolveProviderAPIKey(config.APIKey, mode)
	if err != nil {
		return nil, err
	}
	baseURL, err := resolveProviderBaseURL(config.BaseURL, mode)
	if err != nil {
		return nil, err
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 4 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		}}
	}
	headers := make(map[string]string, len(config.Headers)+2)
	for key, value := range config.Headers {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			headers[key] = value
		}
	}
	if mode == ProviderModeOpenRouter {
		referer := strings.TrimSpace(config.HTTPReferer)
		if referer == "" {
			referer = defaultOpenRouterHTTPReferer
		}
		title := strings.TrimSpace(config.XTitle)
		if title == "" {
			title = defaultOpenRouterTitle
		}
		headers["HTTP-Referer"] = referer
		headers["X-Title"] = title
	}
	return &ChatClient{
		mode:       mode,
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(baseURL, "/"),
		label:      resolveProviderLabel(config.ProviderLabel, mode),
		headers:    headers,
		provider:   strings.TrimSpace(config.Provider),
		requireZDR: config.RequireZDR,
		client:     client,
		idle:       chatIdleTimeout,
		backoff:    chatRetryBackoff,
	}, nil
}

// Chat sends one turn and streams its text. It retries only failures that
// happen before the provider starts answering, so a retry never repeats text
// the caller already received.
func (c *ChatClient) Chat(ctx context.Context, request ChatRequest, onText func(string)) (ChatResult, error) {
	body, err := c.requestBody(request)
	if err != nil {
		return ChatResult{}, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, errors.New("AI chat request could not be encoded")
	}
	var lastErr error
	for attempt := 1; attempt <= chatMaxAttempts; attempt++ {
		result, retry, err := c.attempt(ctx, payload, onText)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retry || attempt == chatMaxAttempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return ChatResult{}, ctx.Err()
		case <-time.After(c.backoff * time.Duration(attempt)):
		}
	}
	return ChatResult{}, lastErr
}

func (c *ChatClient) attempt(ctx context.Context, payload []byte, onText func(string)) (ChatResult, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ChatResult{}, false, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream, application/json")
	request.Header.Set("User-Agent", chatUserAgent)
	for key, value := range c.headers {
		request.Header.Set(key, value)
	}
	response, err := c.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ChatResult{}, false, ctx.Err()
		}
		return ChatResult{}, true, &ProviderError{Provider: c.label}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ChatResult{}, retryableChatStatus(response.StatusCode), c.statusError(response)
	}
	var result ChatResult
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		result, err = c.readStream(ctx, response.Body, onText)
	} else {
		result, err = c.readJSON(response.Body, onText)
	}
	return result, false, err
}

func retryableChatStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (c *ChatClient) statusError(response *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, chatMaxErrorBody))
	return &ChatError{
		ProviderError: ProviderError{Provider: c.label, StatusCode: response.StatusCode},
		Detail:        providerErrorDetail(raw),
	}
}

// providerErrorDetail extracts a short message from the common error shapes:
// {"error":{"message":"..."}}, {"error":"..."}, and {"message":"..."}.
func providerErrorDetail(raw []byte) string {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return ""
	}
	if message := strings.TrimSpace(envelope.Message); message != "" {
		return shortDetail(message)
	}
	var nested struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.Unmarshal(envelope.Error, &nested) == nil {
		if message := strings.TrimSpace(nested.Message); message != "" {
			return shortDetail(message)
		}
		return shortDetail(nested.Type)
	}
	var text string
	if json.Unmarshal(envelope.Error, &text) == nil {
		return shortDetail(text)
	}
	return ""
}

func shortDetail(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 300 {
		value = value[:300]
	}
	return value
}

func (c *ChatClient) readJSON(body io.Reader, onText func(string)) (ChatResult, error) {
	raw, err := io.ReadAll(io.LimitReader(body, chatMaxResponse))
	if err != nil {
		return ChatResult{}, &ProviderError{Provider: c.label}
	}
	result, err := parseChatCompletion(raw)
	if err != nil {
		return ChatResult{}, c.streamError(err)
	}
	if onText != nil && result.Text != "" {
		onText(result.Text)
	}
	return result, nil
}

func (c *ChatClient) readStream(ctx context.Context, body io.ReadCloser, onText func(string)) (ChatResult, error) {
	watchdog := time.AfterFunc(c.idle, func() { _ = body.Close() })
	defer watchdog.Stop()
	accumulator := newChatAccumulator(onText)
	reader := bufio.NewReaderSize(body, 64<<10)
	for {
		line, readErr := reader.ReadString('\n')
		watchdog.Reset(c.idle)
		if len(line) > chatMaxResponse {
			return ChatResult{}, &ProviderError{Provider: c.label}
		}
		if line != "" {
			done, err := accumulator.consume(line)
			if err != nil {
				return ChatResult{}, c.streamError(err)
			}
			if done {
				break
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return ChatResult{}, ctx.Err()
			}
			return ChatResult{}, &ProviderError{Provider: c.label}
		}
	}
	return accumulator.result(), nil
}

func (c *ChatClient) streamError(err error) error {
	var providerErr *chatStreamError
	if errors.As(err, &providerErr) {
		return &ChatError{ProviderError: ProviderError{Provider: c.label, StatusCode: providerErr.status}, Detail: providerErr.message}
	}
	return &ProviderError{Provider: c.label}
}

type chatStreamError struct {
	status  int
	message string
}

func (e *chatStreamError) Error() string { return e.message }

type chatDelta struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ToolCalls        []chatToolDelta `json:"tool_calls"`
}

type chatToolDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	Cost             *float64 `json:"cost"`
}

type chatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta        chatDelta `json:"delta"`
		Message      chatDelta `json:"message"`
		FinishReason *string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

type partialToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

type chatAccumulator struct {
	onText         func(string)
	text           strings.Builder
	reasoning      strings.Builder
	reasoningField string
	calls          map[int]*partialToolCall
	order          []int
	finish         string
	model          string
	id             string
	usage          Usage
}

func newChatAccumulator(onText func(string)) *chatAccumulator {
	return &chatAccumulator{onText: onText, calls: make(map[int]*partialToolCall)}
}

// consume handles one server-sent-event line. It reports done on [DONE].
func (a *chatAccumulator) consume(line string) (bool, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return false, nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return false, nil
	}
	if payload == "[DONE]" {
		return true, nil
	}
	var chunk chatChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return false, nil
	}
	if chunk.Error != nil {
		return false, &chatStreamError{status: numericCode(chunk.Error.Code), message: shortDetail(chunk.Error.Message)}
	}
	a.apply(&chunk)
	return false, nil
}

func numericCode(raw json.RawMessage) int {
	var code int
	if json.Unmarshal(raw, &code) == nil {
		return code
	}
	return 0
}

func (a *chatAccumulator) apply(chunk *chatChunk) {
	if chunk.ID != "" {
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usage = Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
			CostUSD:      chunk.Usage.Cost,
		}
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if len(delta.Content) == 0 && len(delta.ToolCalls) == 0 && choice.Message.hasContent() {
			delta = choice.Message
		}
		a.applyDelta(delta)
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			a.finish = *choice.FinishReason
		}
	}
}

func (d chatDelta) hasContent() bool {
	return len(d.Content) > 0 || len(d.ToolCalls) > 0 || d.ReasoningContent != "" || d.Reasoning != ""
}

func (a *chatAccumulator) applyDelta(delta chatDelta) {
	if text := flexibleChatText(delta.Content); text != "" {
		a.text.WriteString(text)
		if a.onText != nil {
			a.onText(text)
		}
	}
	switch {
	case delta.ReasoningContent != "":
		a.reasoningField = "reasoning_content"
		a.reasoning.WriteString(delta.ReasoningContent)
	case delta.Reasoning != "":
		if a.reasoningField == "" {
			a.reasoningField = "reasoning"
		}
		a.reasoning.WriteString(delta.Reasoning)
	}
	for position, tool := range delta.ToolCalls {
		index := position
		if tool.Index != nil {
			index = *tool.Index
		}
		call := a.calls[index]
		if call == nil {
			call = &partialToolCall{}
			a.calls[index] = call
			a.order = append(a.order, index)
		}
		if tool.ID != "" {
			call.id = tool.ID
		}
		if tool.Function.Name != "" && call.name == "" {
			call.name = tool.Function.Name
		}
		call.arguments.WriteString(tool.Function.Arguments)
	}
}

func (a *chatAccumulator) result() ChatResult {
	calls := make([]ToolCall, 0, len(a.order))
	for position, index := range a.order {
		call := a.calls[index]
		if strings.TrimSpace(call.name) == "" {
			continue
		}
		id := call.id
		if id == "" {
			id = fmt.Sprintf("call_%d", position+1)
		}
		arguments := strings.TrimSpace(call.arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		calls = append(calls, ToolCall{ID: id, Name: call.name, Arguments: arguments})
	}
	return ChatResult{
		Text:           a.text.String(),
		ToolCalls:      calls,
		FinishReason:   a.finish,
		Model:          a.model,
		RequestID:      a.id,
		Usage:          a.usage,
		Reasoning:      a.reasoning.String(),
		ReasoningField: a.reasoningField,
	}
}

// flexibleChatText reads content that is either a string or an array of typed
// text parts.
func flexibleChatText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" || part.Type == "" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// parseChatCompletion reads a non-streamed completion for providers that
// ignore stream=true.
func parseChatCompletion(raw []byte) (ChatResult, error) {
	var chunk chatChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return ChatResult{}, err
	}
	if chunk.Error != nil {
		return ChatResult{}, &chatStreamError{status: numericCode(chunk.Error.Code), message: shortDetail(chunk.Error.Message)}
	}
	accumulator := newChatAccumulator(nil)
	accumulator.apply(&chunk)
	return accumulator.result(), nil
}

func (c *ChatClient) requestBody(request ChatRequest) (map[string]any, error) {
	model := strings.TrimSpace(request.Model)
	if model == "" {
		return nil, errors.New("AI model is required")
	}
	if len(request.Messages) == 0 {
		return nil, errors.New("AI chat messages are required")
	}
	messages := make([]map[string]any, 0, len(request.Messages))
	for index, message := range request.Messages {
		encoded, err := encodeChatMessage(message)
		if err != nil {
			return nil, fmt.Errorf("AI chat message %d: %w", index+1, err)
		}
		messages = append(messages, encoded)
	}
	body := map[string]any{
		"model":          model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if tools := encodeChatTools(request.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	if request.MaxOutputTokens > 0 {
		body["max_completion_tokens"] = request.MaxOutputTokens
	}
	effort, err := chatReasoningEffort(request.ReasoningEffort)
	if err != nil {
		return nil, err
	}
	if effort != "" {
		body["reasoning_effort"] = effort
	}
	if c.mode == ProviderModeOpenRouter {
		preferences := map[string]any{"data_collection": "deny", "require_parameters": true}
		if c.provider != "" {
			preferences["only"] = []string{c.provider}
			preferences["allow_fallbacks"] = false
		}
		if c.requireZDR {
			preferences["zdr"] = true
		}
		body["provider"] = preferences
	}
	return body, nil
}

// chatReasoningEffort returns the request value, or an empty string when the
// provider default should apply.
func chatReasoningEffort(effort ReasoningEffort) (string, error) {
	switch effort {
	case "", ReasoningEffortNone:
		return "", nil
	case ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		return string(effort), nil
	default:
		return "", fmt.Errorf("unsupported AI reasoning effort %q", effort)
	}
}

func encodeChatTools(tools []ToolDefinition) []map[string]any {
	encoded := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || len(name) > chatMaxToolNameLen {
			continue
		}
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		encoded = append(encoded, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": tool.Description,
				"parameters":  parameters,
			},
		})
	}
	return encoded
}

func encodeChatMessage(message ChatMessage) (map[string]any, error) {
	switch message.Role {
	case ChatRoleSystem:
		return map[string]any{"role": "system", "content": message.Content}, nil
	case ChatRoleUser:
		return encodeUserMessage(message)
	case ChatRoleAssistant:
		return encodeAssistantMessage(message), nil
	case ChatRoleTool:
		if strings.TrimSpace(message.ToolCallID) == "" {
			return nil, errors.New("tool message requires a tool call id")
		}
		content := message.Content
		if strings.TrimSpace(content) == "" {
			content = "(no output)"
		}
		return map[string]any{"role": "tool", "tool_call_id": message.ToolCallID, "content": content}, nil
	default:
		return nil, fmt.Errorf("unsupported chat role %q", message.Role)
	}
}

func encodeUserMessage(message ChatMessage) (map[string]any, error) {
	if len(message.Images) == 0 {
		return map[string]any{"role": "user", "content": message.Content}, nil
	}
	parts := make([]map[string]any, 0, len(message.Images)+1)
	if strings.TrimSpace(message.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": message.Content})
	}
	for index, image := range message.Images {
		if len(image.Data) == 0 {
			return nil, fmt.Errorf("image %d: data is required", index+1)
		}
		mediaType, err := normalizedMIMEType(image.MIMEType)
		if err != nil || !strings.HasPrefix(mediaType, "image/") {
			return nil, fmt.Errorf("image %d: valid image MIME type is required", index+1)
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": encodeDataURL(mediaType, image.Data)},
		})
	}
	return map[string]any{"role": "user", "content": parts}, nil
}

func encodeAssistantMessage(message ChatMessage) map[string]any {
	encoded := map[string]any{"role": "assistant"}
	switch {
	case message.Content != "":
		encoded["content"] = message.Content
	case len(message.ToolCalls) > 0:
		encoded["content"] = nil
	default:
		encoded["content"] = ""
	}
	if len(message.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			arguments := call.Arguments
			if strings.TrimSpace(arguments) == "" {
				arguments = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       call.ID,
				"type":     "function",
				"function": map[string]any{"name": call.Name, "arguments": arguments},
			})
		}
		encoded["tool_calls"] = calls
		if message.Reasoning != "" && message.ReasoningField != "" {
			encoded[message.ReasoningField] = message.Reasoning
		}
	}
	return encoded
}
