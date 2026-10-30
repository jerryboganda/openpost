package adminagent

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/stretchr/testify/require"
)

// These checks call the real OpenCode Go endpoint. They run only when
// OPENPOST_LIVE_AI_KEY is set, which the Fork Agent Live Check workflow does on
// GitHub Actions. Mocked tests cannot show that a real model streams, calls
// tools, and reads images through this client, so this is the evidence.
const (
	liveEndpoint   = "https://opencode.ai/inference/go/openai/v1"
	liveRunTimeout = 4 * time.Minute
)

func liveAgent(t *testing.T) *Service {
	t.Helper()
	key := strings.TrimSpace(os.Getenv("OPENPOST_LIVE_AI_KEY"))
	if key == "" {
		t.Skip("set OPENPOST_LIVE_AI_KEY to run the live model checks")
	}
	chat, err := ai.NewChatClient(ai.OpenRouterConfig{
		Mode:          ai.ProviderModeOpenAICompatible,
		APIKey:        key,
		BaseURL:       liveEndpoint,
		ProviderLabel: "OpenCode",
		Headers:       map[string]string{"x-opencode-session": "openpost-live-check"},
	})
	require.NoError(t, err)
	service := NewService(adminAgentTestDB(t), nil)
	service.SetAgent(chat, "deepseek-v4.1-flash", OpenCodeModels())
	service.SetEnvironment(Environment{
		PublicURL: "https://post.example.test", Version: "live", Revision: "live-check", Edition: "selfhost",
		DatabaseDriver: "sqlite", AIProvider: "opencode", Repository: "owner/repo", DefaultBranch: "main",
		StartedAt: time.Now().Add(-time.Hour),
	})
	return service
}

// liveModels returns the catalog, or the models named in OPENPOST_LIVE_AI_MODELS.
func liveModels(t *testing.T) []ModelOption {
	t.Helper()
	all := OpenCodeModels()
	wanted := strings.TrimSpace(os.Getenv("OPENPOST_LIVE_AI_MODELS"))
	if wanted == "" {
		return all
	}
	chosen := make([]ModelOption, 0, len(all))
	for _, id := range strings.Split(wanted, ",") {
		for _, option := range all {
			if option.ID == strings.TrimSpace(id) {
				chosen = append(chosen, option)
			}
		}
	}
	require.NotEmpty(t, chosen, "OPENPOST_LIVE_AI_MODELS names no known model")
	return chosen
}

func livePNG(t *testing.T) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for x := 0; x < 96; x++ {
		for y := 0; y < 96; y++ {
			canvas.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}
	var buffer bytes.Buffer
	require.NoError(t, png.Encode(&buffer, canvas))
	return buffer.Bytes()
}

type liveResult struct {
	run    *AgentRun
	answer string
	tools  []string
	failed []string
}

func finishedLiveRun(t *testing.T, service *Service, runID string) *AgentRun {
	t.Helper()
	deadline := time.Now().Add(liveRunTimeout)
	for time.Now().Before(deadline) {
		run, err := service.GetRun(context.Background(), runID)
		require.NoError(t, err)
		if RunStatus(run.Status).Terminal() {
			return run
		}
		time.Sleep(500 * time.Millisecond)
	}
	_, _ = service.CancelRun(context.Background(), runID)
	t.Fatalf("the run did not finish within %s", liveRunTimeout)
	return nil
}

func liveRun(t *testing.T, service *Service, model, prompt string, images ...string) liveResult {
	t.Helper()
	conversation := newAgentConversation(t, service)
	run, err := service.StartRun(context.Background(), StartRunInput{
		ConversationID: conversation.ID, UserID: "admin", Prompt: prompt, AttachmentIDs: images, Model: model,
	})
	require.NoError(t, err)
	finished := finishedLiveRun(t, service, run.ID)
	if finished.Status != string(RunSucceeded) {
		t.Fatalf("run %s: %s: %s", finished.Status, finished.FailureCode, finished.FailureMessage)
	}
	result := liveResult{run: finished, answer: finished.Summary}
	for _, event := range eventsOf(t, service, run.ID) {
		if event.Kind != "tool_result" {
			continue
		}
		name, _ := event.Data["name"].(string)
		if ok, _ := event.Data["ok"].(bool); ok {
			result.tools = append(result.tools, name)
		} else {
			result.failed = append(result.failed, name)
		}
	}
	t.Logf("model %s answered in %d input and %d output tokens after tools %v (failed %v): %q",
		finished.Model, finished.InputTokens, finished.OutputTokens, result.tools, result.failed, result.answer)
	require.NotContains(t, result.answer, "<think", "reasoning that a model writes into its answer never reaches the reader")
	return result
}

func TestLiveModelsCallToolsAcrossSeveralSteps(t *testing.T) {
	service := liveAgent(t)
	for _, option := range liveModels(t) {
		t.Run(option.ID, func(t *testing.T) {
			result := liveRun(t, service, option.ID,
				"First call prod_status. Then call db_query with the SQL `SELECT COUNT(*) AS total FROM users`. "+
					"Finish with one sentence that states the number of users.")
			require.Contains(t, result.tools, "prod_status")
			require.Contains(t, result.tools, "db_query")
			require.Empty(t, result.failed, "no tool call may fail")
			answer := strings.ToLower(result.answer)
			require.True(t, strings.Contains(answer, "2") || strings.Contains(answer, "two"),
				"the answer states the user count that the query returned: %q", result.answer)
		})
	}
}

// askColor shows the model a red picture. A small model can rarely misread even
// a plain swatch, so it gets a few tries with a fresh upload each time. A model
// that cannot receive the image at all fails every try.
func askColor(t *testing.T, service *Service, model string) liveResult {
	t.Helper()
	const attempts = 3
	var result liveResult
	for attempt := 1; attempt <= attempts; attempt++ {
		attachment, err := service.SaveAttachment(context.Background(), "admin", "red.png", livePNG(t))
		require.NoError(t, err)
		result = liveRun(t, service, model, "What single color fills this image? Answer with one word.", attachment.ID)
		if strings.Contains(strings.ToLower(result.answer), "red") {
			return result
		}
		t.Logf("attempt %d of %d read the picture as %q", attempt, attempts, result.answer)
	}
	require.Contains(t, strings.ToLower(result.answer), "red", "the model never described the picture")
	return result
}

func TestLiveVisionModelsReadImages(t *testing.T) {
	service := liveAgent(t)
	for _, option := range liveModels(t) {
		if !option.Vision {
			continue
		}
		t.Run(option.ID, func(t *testing.T) {
			result := askColor(t, service, option.ID)
			require.Equal(t, option.ID, result.run.Model, "a model that reads images answers itself")
		})
	}
}

func TestLiveTextOnlyModelsHandOffImages(t *testing.T) {
	service := liveAgent(t)
	for _, option := range liveModels(t) {
		if option.Vision {
			continue
		}
		t.Run(option.ID, func(t *testing.T) {
			result := askColor(t, service, option.ID)
			require.NotEqual(t, option.ID, result.run.Model, "a text-only model hands the image to one that can read it")
		})
	}
}
