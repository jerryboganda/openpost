package adminagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/stretchr/testify/require"
)

type chatTurn func(ctx context.Context, request ai.ChatRequest, onText func(string)) (ai.ChatResult, error)

// scriptedChat plays a fixed conversation with the agent and records every
// request, so tests can assert what the model was sent.
type scriptedChat struct {
	mu       sync.Mutex
	turns    []chatTurn
	requests []ai.ChatRequest
}

func (c *scriptedChat) Chat(ctx context.Context, request ai.ChatRequest, onText func(string)) (ai.ChatResult, error) {
	request.Messages = append([]ai.ChatMessage(nil), request.Messages...)
	c.mu.Lock()
	index := len(c.requests)
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	if index >= len(c.turns) {
		return ai.ChatResult{}, errors.New("unexpected extra model turn")
	}
	return c.turns[index](ctx, request, onText)
}

func (c *scriptedChat) snapshot() []ai.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ai.ChatRequest(nil), c.requests...)
}

var scriptedUsage = ai.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}

func answer(text string) chatTurn {
	return func(_ context.Context, _ ai.ChatRequest, onText func(string)) (ai.ChatResult, error) {
		if onText != nil && text != "" {
			onText(text)
		}
		return ai.ChatResult{Text: text, FinishReason: "stop", Usage: scriptedUsage}, nil
	}
}

func callTools(calls ...ai.ToolCall) chatTurn {
	return func(context.Context, ai.ChatRequest, func(string)) (ai.ChatResult, error) {
		return ai.ChatResult{ToolCalls: calls, FinishReason: "tool_calls", Usage: scriptedUsage}, nil
	}
}

func failWith(err error) chatTurn {
	return func(context.Context, ai.ChatRequest, func(string)) (ai.ChatResult, error) {
		return ai.ChatResult{}, err
	}
}

func newAgentService(t *testing.T, turns ...chatTurn) (*Service, *scriptedChat) {
	t.Helper()
	chat := &scriptedChat{turns: turns}
	service := NewService(adminAgentTestDB(t), nil)
	service.SetAgent(chat, "deepseek-v4.1-flash", OpenCodeModels())
	service.SetEnvironment(Environment{
		PublicURL: "https://post.example.test", Version: "v1.2.3", Revision: "abc1234", Edition: "selfhost",
		DatabaseDriver: "sqlite", AIProvider: "opencode", Repository: "owner/repo", DefaultBranch: "main",
		StartedAt: time.Now().Add(-time.Hour), Secrets: []string{"super-secret-value"},
	})
	return service, chat
}

func newAgentConversation(t *testing.T, service *Service) *AgentConversation {
	t.Helper()
	conversation, err := service.CreateConversation(context.Background(), "admin", "", "", EngineAssistant)
	require.NoError(t, err)
	return conversation
}

func waitForTerminalRun(t *testing.T, service *Service, runID string) *AgentRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.GetRun(context.Background(), runID)
		require.NoError(t, err)
		if RunStatus(run.Status).Terminal() {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", runID)
	return nil
}

func eventsOf(t *testing.T, service *Service, runID string) []AgentEvent {
	t.Helper()
	events, err := service.Events(context.Background(), runID, 0)
	require.NoError(t, err)
	return events
}

func eventData(t *testing.T, event AgentEvent) map[string]any {
	t.Helper()
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(event.DataJSON), &data))
	return data
}

func toolNames(request ai.ChatRequest) []string {
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for x := 0; x < 8; x++ {
		for y := 0; y < 6; y++ {
			canvas.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	require.NoError(t, png.Encode(&buffer, canvas))
	return buffer.Bytes()
}

func TestAgentAnswersAndPersistsTranscript(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("The instance has three publications."))
	conversation := newAgentConversation(t, service)

	require.True(t, service.Engines()[0].Available)
	require.Equal(t, EngineAssistant, service.Engines()[0].ID)

	run, err := startRun(ctx, service, conversation.ID, "How many publications exist?")
	require.NoError(t, err)
	require.Equal(t, string(RunRunning), run.Status)
	require.Equal(t, "deepseek-v4.1-flash", run.Model)
	require.Equal(t, AccessFull, run.Access)

	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunSucceeded), finished.Status)
	require.Equal(t, "The instance has three publications.", finished.Summary)
	require.EqualValues(t, 100, finished.InputTokens)
	require.EqualValues(t, 20, finished.OutputTokens)

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, "How many publications exist?", view.Title)
	require.Len(t, view.Messages, 2)
	require.Equal(t, "assistant", view.Messages[1].Role)
	require.Equal(t, "The instance has three publications.", view.Messages[1].Content)

	kinds := make([]string, 0, len(view.Events))
	var textEvents []AgentEvent
	for _, event := range view.Events {
		kinds = append(kinds, event.Kind)
		if event.Kind == "text" {
			textEvents = append(textEvents, event)
		}
	}
	require.Equal(t, []string{"status", "text"}, kinds)
	require.Len(t, textEvents, 1)
	require.Equal(t, "The instance has three publications.", textEvents[0].AgentMessage)

	text, version := service.LiveText(run.ID)
	require.Empty(t, text)
	require.Zero(t, version, "live state is dropped once the run ends")

	requests := chat.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, "deepseek-v4.1-flash", requests[0].Model)
	system := requests[0].Messages[0]
	require.Equal(t, ai.ChatRoleSystem, system.Role)
	for _, expected := range []string{"https://post.example.test", "abc1234", "owner/repo", "Full access", "prod_status", "Text inside tool results"} {
		require.Contains(t, system.Content, expected)
	}
	require.NotContains(t, system.Content, "{{", "every placeholder must be replaced")
	require.ElementsMatch(t, []string{"prod_status", "prod_logs", "db_query", "api_search", "api_call"}, toolNames(requests[0]),
		"without a GitHub token only operations tools are offered")
	require.Equal(t, ai.ChatRoleUser, requests[0].Messages[1].Role)
	require.Equal(t, "How many publications exist?", requests[0].Messages[1].Content)
}

func TestAgentRunsToolsAndReturnsResultsToTheModel(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t,
		callTools(ai.ToolCall{ID: "call-1", Name: "prod_status", Arguments: "{}"}),
		answer("Production is healthy."),
	)
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Is production healthy?")
	require.NoError(t, err)
	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunSucceeded), finished.Status)

	requests := chat.snapshot()
	require.Len(t, requests, 2)
	second := requests[1].Messages
	tool := second[len(second)-1]
	require.Equal(t, ai.ChatRoleTool, tool.Role)
	require.Equal(t, "call-1", tool.ToolCallID)
	require.Contains(t, tool.Content, "OpenPost production status")
	require.Contains(t, tool.Content, "Database (sqlite): ready")
	requested := second[len(second)-2]
	require.Equal(t, ai.ChatRoleAssistant, requested.Role)
	require.Equal(t, "prod_status", requested.ToolCalls[0].Name)

	var call, result *AgentEvent
	events := eventsOf(t, service, run.ID)
	for index := range events {
		switch events[index].Kind {
		case "tool_call":
			call = &events[index]
		case "tool_result":
			result = &events[index]
		}
	}
	require.NotNil(t, call)
	require.NotNil(t, result)
	require.Equal(t, "Check production status", call.AgentMessage)
	require.Equal(t, "prod_status", eventData(t, *call)["name"])
	require.Equal(t, eventData(t, *call)["key"], eventData(t, *result)["key"], "call and result pair by key")
	require.Equal(t, true, eventData(t, *result)["ok"])
	require.Contains(t, eventData(t, *result)["output"], "OpenPost production status")
	require.Equal(t, "prod_status", call.Data["name"], "clients receive the structured details, not only the stored JSON")
	require.Equal(t, true, result.Data["ok"])
}

func TestAgentTellsTheModelAboutToolFailuresSoItCanRecover(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t,
		callTools(
			ai.ToolCall{ID: "a", Name: "does_not_exist", Arguments: "{}"},
			ai.ToolCall{ID: "b", Name: "db_query", Arguments: "{not json"},
			ai.ToolCall{ID: "c", Name: "db_query", Arguments: `{"sql":"DELETE FROM users"}`},
		),
		answer("I could not run those, so I will stop here."),
	)
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Do something odd.")
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status,
		"a failed tool call is information for the model, not a failed run")

	second := chat.snapshot()[1].Messages
	outputs := map[string]string{}
	for _, message := range second {
		if message.Role == ai.ChatRoleTool {
			outputs[message.ToolCallID] = message.Content
		}
	}
	require.Contains(t, outputs["a"], "unknown tool")
	require.Contains(t, outputs["b"], "not a valid JSON object")
	require.Contains(t, outputs["c"], "only SELECT")

	var failures int
	for _, event := range eventsOf(t, service, run.ID) {
		if event.Kind == "tool_result" && eventData(t, event)["ok"] == false {
			failures++
		}
	}
	require.Equal(t, 3, failures)
}

func TestAccessModeControlsWhichToolsAreOffered(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("Looked."), answer("Changed."))
	require.NoError(t, service.SetGitHub("token", "owner/repo", "main"))
	conversation := newAgentConversation(t, service)

	readOnly, err := service.StartRun(ctx, StartRunInput{ConversationID: conversation.ID, UserID: "admin", Prompt: "Look around.", Access: AccessReadOnly})
	require.NoError(t, err)
	require.Equal(t, AccessReadOnly, readOnly.Access)
	waitForTerminalRun(t, service, readOnly.ID)
	full, err := service.StartRun(ctx, StartRunInput{ConversationID: conversation.ID, UserID: "admin", Prompt: "Now change it."})
	require.NoError(t, err)
	waitForTerminalRun(t, service, full.ID)

	requests := chat.snapshot()
	require.Len(t, requests, 2)
	readOnlyTools, fullTools := toolNames(requests[0]), toolNames(requests[1])
	for _, name := range []string{"repo_edit", "repo_write", "repo_delete", "pr_open", "pr_merge", "ci_dispatch", "ci_control"} {
		require.NotContains(t, readOnlyTools, name, "read-only runs never see write tools")
		require.Contains(t, fullTools, name)
	}
	for _, name := range []string{"repo_tree", "repo_read", "repo_search", "pr_get", "ci_status", "ci_logs", "github_api", "prod_status", "prod_logs", "db_query", "api_search", "api_call"} {
		require.Contains(t, readOnlyTools, name)
		require.Contains(t, fullTools, name)
	}
	require.Contains(t, requests[0].Messages[0].Content, "Read-only")
	require.Contains(t, requests[1].Messages[0].Content, "Full access")
}

func TestProviderFailuresAreTruthfulAndNeverLeakSecrets(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     string
		contains string
	}{
		{"credentials", &ai.ProviderError{Provider: "OpenCode", StatusCode: 401}, "invalid_credentials", "credentials"},
		{"rate limit", &ai.ProviderError{Provider: "OpenCode", StatusCode: 429}, "rate_limited", "rate limited"},
		{
			"rejected request",
			&ai.ChatError{ProviderError: ai.ProviderError{Provider: "OpenCode", StatusCode: 400}, Detail: "Model is unavailable for super-secret-value"},
			"provider_rejected", "Model is unavailable",
		},
		{"anything else", errors.New("connection reset by peer"), "provider_error", "request failed"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service, _ := newAgentService(t, failWith(testCase.err))
			conversation := newAgentConversation(t, service)
			run, err := startRun(context.Background(), service, conversation.ID, "Hello")
			require.NoError(t, err)
			finished := waitForTerminalRun(t, service, run.ID)
			require.Equal(t, string(RunFailed), finished.Status)
			require.Equal(t, testCase.code, finished.FailureCode)
			require.Contains(t, finished.FailureMessage, testCase.contains)
			require.NotContains(t, finished.FailureMessage, "super-secret-value")
			require.NotContains(t, finished.FailureMessage, "connection reset")

			var failed bool
			for _, event := range eventsOf(t, service, run.ID) {
				failed = failed || (event.Kind == "error" && event.AgentMessage == finished.FailureMessage)
			}
			require.True(t, failed, "the failure is part of the inspectable action log")
			view, err := service.GetConversation(context.Background(), conversation.ID)
			require.NoError(t, err)
			require.Len(t, view.Messages, 1, "a failed run adds no assistant message")
		})
	}
}

func TestCancelInterruptsARunningAgent(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	service, _ := newAgentService(t, func(ctx context.Context, _ ai.ChatRequest, _ func(string)) (ai.ChatResult, error) {
		close(started)
		<-ctx.Done()
		return ai.ChatResult{}, ctx.Err()
	})
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Think for a long time.")
	require.NoError(t, err)
	<-started

	canceled, err := service.CancelRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunCanceled), canceled.Status)
	require.Eventually(t, func() bool {
		_, running := service.cancels.Load(run.ID)
		return !running
	}, 5*time.Second, 10*time.Millisecond, "the agent goroutine must stop promptly")

	stored, err := service.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, string(RunCanceled), stored.Status, "a canceled run is never overwritten")
	require.Empty(t, stored.FailureCode)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Messages, 1)
}

func TestOnlyOneRunMayBeActivePerConversation(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	service, _ := newAgentService(t,
		func(ctx context.Context, _ ai.ChatRequest, _ func(string)) (ai.ChatResult, error) {
			close(started)
			<-ctx.Done()
			return ai.ChatResult{}, ctx.Err()
		},
		answer("Second task done."),
	)
	conversation := newAgentConversation(t, service)
	first, err := startRun(ctx, service, conversation.ID, "First task.")
	require.NoError(t, err)
	<-started

	_, err = startRun(ctx, service, conversation.ID, "Second task.")
	require.ErrorIs(t, err, ErrRunActive)

	_, err = service.CancelRun(ctx, first.ID)
	require.NoError(t, err)
	second, err := startRun(ctx, service, conversation.ID, "Second task.")
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, second.ID).Status)
}

func TestLiveTextStreamsWhileATurnIsRunning(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	service, _ := newAgentService(t, func(_ context.Context, _ ai.ChatRequest, onText func(string)) (ai.ChatResult, error) {
		onText("Hel")
		<-release
		onText("lo")
		return ai.ChatResult{Text: "Hello", FinishReason: "stop"}, nil
	})
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Say hello.")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		text, _ := service.LiveText(run.ID)
		return text == "Hel"
	}, 5*time.Second, 5*time.Millisecond)
	close(release)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)
	text, _ := service.LiveText(run.ID)
	require.Empty(t, text)
}

func TestAgentStopsAtTheStepLimitInsteadOfLoopingForever(t *testing.T) {
	ctx := context.Background()
	turns := make([]chatTurn, maxAgentSteps)
	for index := range turns {
		turns[index] = callTools(ai.ToolCall{ID: "loop", Name: "prod_status", Arguments: "{}"})
	}
	service, chat := newAgentService(t, turns...)
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Keep checking.")
	require.NoError(t, err)

	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunFailed), finished.Status)
	require.Equal(t, "step_limit", finished.FailureCode)
	require.Len(t, chat.snapshot(), maxAgentSteps)
}

func TestEmptyAnswersGetOneNudgeBeforeFailing(t *testing.T) {
	ctx := context.Background()
	t.Run("recovers", func(t *testing.T) {
		service, chat := newAgentService(t, answer(""), answer("Here is the answer."))
		run, err := startRun(ctx, service, newAgentConversation(t, service).ID, "Question")
		require.NoError(t, err)
		require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)
		last := chat.snapshot()[1].Messages
		require.Equal(t, ai.ChatRoleUser, last[len(last)-1].Role)
	})
	t.Run("gives up", func(t *testing.T) {
		service, _ := newAgentService(t, answer(""), answer(""))
		run, err := startRun(ctx, service, newAgentConversation(t, service).ID, "Question")
		require.NoError(t, err)
		finished := waitForTerminalRun(t, service, run.ID)
		require.Equal(t, string(RunFailed), finished.Status)
		require.Equal(t, "empty_response", finished.FailureCode)
	})
}

func TestEarlierFailedRunsDoNotBreakTheNextRequest(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, failWith(errors.New("boom")), answer("Second answer."))
	conversation := newAgentConversation(t, service)
	first, err := startRun(ctx, service, conversation.ID, "First question")
	require.NoError(t, err)
	waitForTerminalRun(t, service, first.ID)
	second, err := startRun(ctx, service, conversation.ID, "Second question")
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, second.ID).Status)

	messages := chat.snapshot()[1].Messages
	require.Len(t, messages, 2, "consecutive user messages are joined for providers that require alternation")
	require.Equal(t, "First question\n\nSecond question", messages[1].Content)
}

func TestImagesReachTheModelAndFallBackToAVisionModel(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("It is a red rectangle."), answer("Still red."))
	conversation := newAgentConversation(t, service)
	picture := testPNG(t)
	attachment, err := service.SaveAttachment(ctx, "admin", "shot.png", picture)
	require.NoError(t, err)
	require.Equal(t, "image/png", attachment.ContentType)
	require.Equal(t, 8, attachment.Width)

	run, err := service.StartRun(ctx, StartRunInput{
		ConversationID: conversation.ID, UserID: "admin", Prompt: "What is in this screenshot?",
		AttachmentIDs: []string{attachment.ID}, Model: "glm-5.3",
	})
	require.NoError(t, err)
	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunSucceeded), finished.Status)
	require.Equal(t, "deepseek-v4.1-flash", finished.Model, "the run records the model that actually answered")

	first := chat.snapshot()[0]
	require.Equal(t, "deepseek-v4.1-flash", first.Model, "a text-only model is swapped for one that reads images")
	user := first.Messages[len(first.Messages)-1]
	require.Equal(t, "What is in this screenshot?", user.Content)
	require.Len(t, user.Images, 1)
	require.Equal(t, "image/png", user.Images[0].MIMEType)
	require.Equal(t, picture, user.Images[0].Data)
	var switched bool
	for _, event := range eventsOf(t, service, run.ID) {
		switched = switched || (event.Kind == "status" && strings.Contains(event.AgentMessage, "cannot read images"))
	}
	require.True(t, switched, "the swap is visible in the transcript")

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Len(t, view.Messages[0].Attachments, 1)
	require.Equal(t, "shot.png", view.Messages[0].Attachments[0].Filename)

	followUp, err := startRun(ctx, service, conversation.ID, "And the color?")
	require.NoError(t, err)
	waitForTerminalRun(t, service, followUp.ID)
	history := chat.snapshot()[1].Messages
	require.Contains(t, history[1].Content, "1 image(s) were attached to this message earlier")
	for _, message := range history {
		require.Empty(t, message.Images, "earlier images are not resent")
	}
}

func TestImageOnlyMessagesAreAccepted(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("A red rectangle."))
	conversation := newAgentConversation(t, service)
	attachment, err := service.SaveAttachment(ctx, "admin", "", testPNG(t))
	require.NoError(t, err)
	require.Equal(t, "image", attachment.Filename)

	run, err := service.StartRun(ctx, StartRunInput{ConversationID: conversation.ID, UserID: "admin", AttachmentIDs: []string{attachment.ID}})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)
	user := chat.snapshot()[0].Messages[1]
	require.Contains(t, user.Content, "attached image")
	require.Len(t, user.Images, 1)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, "Image", view.Title)
}

func TestCompactMessagesDropsOldToolOutputAndReasoning(t *testing.T) {
	var messages []ai.ChatMessage
	messages = append(messages, ai.ChatMessage{Role: ai.ChatRoleSystem, Content: "system"})
	for index := 0; index < 12; index++ {
		messages = append(messages,
			ai.ChatMessage{Role: ai.ChatRoleAssistant, ToolCalls: []ai.ToolCall{{ID: "c", Name: "repo_read", Arguments: "{}"}}, Reasoning: "thinking", ReasoningField: "reasoning_content"},
			ai.ChatMessage{Role: ai.ChatRoleTool, ToolCallID: "c", Content: strings.Repeat("x", 100_000)},
		)
	}
	compactMessages(messages)

	var tools []ai.ChatMessage
	var assistants []ai.ChatMessage
	for _, message := range messages {
		switch message.Role {
		case ai.ChatRoleTool:
			tools = append(tools, message)
		case ai.ChatRoleAssistant:
			assistants = append(assistants, message)
		}
	}
	require.Len(t, tools, 12)
	for index, message := range tools {
		if index < 12-keepRecentToolOutputs {
			require.Equal(t, omittedToolOutput, message.Content, "the oldest outputs are replaced")
		} else {
			require.Len(t, message.Content, 100_000, "recent outputs stay intact")
		}
	}
	for index, message := range assistants {
		if index < len(assistants)-keepRecentReasoning {
			require.Empty(t, message.Reasoning)
		} else {
			require.Equal(t, "thinking", message.Reasoning)
		}
	}
}

func TestRedactionMasksConfiguredSecretsAndCredentialShapes(t *testing.T) {
	service, _ := newAgentService(t)
	input := strings.Join([]string{
		"literal super-secret-value here",
		"token gho_abcdefghijklmnopqrstuvwxyz0123456789",
		"key sk-abcdefghijklmnopqrstuvwxyz012345",
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJ1c2VyIjoiYWRtaW4ifQ.c2lnbmF0dXJlMTIz",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789",
		"plain text stays",
	}, "\n")
	redacted := service.redact(input)
	for _, leaked := range []string{"super-secret-value", "gho_abcdefghijklmnopqrstuvwxyz0123456789", "sk-abcdefghijklmnopqrstuvwxyz012345", "eyJhbGciOiJIUzI1NiJ9", "abcdefghijklmnopqrstuvwxyz0123456789"} {
		require.NotContains(t, redacted, leaked)
	}
	require.Contains(t, redacted, "plain text stays")
}

func TestReasoningWrittenIntoAnAnswerNeverReachesTheReader(t *testing.T) {
	ctx := context.Background()
	streamed := make(chan struct{})
	release := make(chan struct{})
	service, _ := newAgentService(t, func(_ context.Context, _ ai.ChatRequest, onText func(string)) (ai.ChatResult, error) {
		onText("<think>Check the")
		onText(" status first.</think>Pro")
		close(streamed)
		<-release
		onText("duction is healthy.")
		return ai.ChatResult{
			Text:         "<think>Check the status first.</think>Production is healthy.",
			FinishReason: "stop",
			Usage:        scriptedUsage,
		}, nil
	})
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Is production healthy?")
	require.NoError(t, err)

	select {
	case <-streamed:
	case <-time.After(10 * time.Second):
		t.Fatal("the model never streamed")
	}
	text, _ := service.LiveText(run.ID)
	require.Equal(t, "Pro", text, "the reasoning stays hidden while the answer streams")
	close(release)

	finished := waitForTerminalRun(t, service, run.ID)
	require.Equal(t, string(RunSucceeded), finished.Status)
	require.Equal(t, "Production is healthy.", finished.Summary)
	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Equal(t, "Production is healthy.", view.Messages[len(view.Messages)-1].Content)
	for _, event := range eventsOf(t, service, run.ID) {
		require.NotContains(t, event.AgentMessage, "<think>", "no event carries the reasoning")
	}
}

func TestTheModelGetsItsOwnReasoningBackInTheHistory(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t,
		func(context.Context, ai.ChatRequest, func(string)) (ai.ChatResult, error) {
			return ai.ChatResult{
				Text:         "<think>I need the status.</think>",
				ToolCalls:    []ai.ToolCall{{ID: "call-1", Name: "prod_status", Arguments: "{}"}},
				FinishReason: "tool_calls",
				Usage:        scriptedUsage,
			}, nil
		},
		answer("Healthy."),
	)
	conversation := newAgentConversation(t, service)
	run, err := startRun(ctx, service, conversation.ID, "Is production healthy?")
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)

	requests := chat.snapshot()
	require.Len(t, requests, 2)
	echoed := ""
	for _, message := range requests[1].Messages {
		if message.Role == ai.ChatRoleAssistant && len(message.ToolCalls) > 0 {
			echoed = message.Content
		}
	}
	require.Equal(t, "<think>I need the status.</think>", echoed,
		"models that reason in their answer work better when they get that reasoning back")
	texts := 0
	for _, event := range eventsOf(t, service, run.ID) {
		if event.Kind == "text" {
			texts++
		}
	}
	require.Equal(t, 1, texts, "a turn that holds only reasoning shows no text, so only the answer is a text event")
}