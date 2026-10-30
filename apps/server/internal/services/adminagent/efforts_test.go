package adminagent

import (
	"context"
	"strings"
	"testing"

	"github.com/openpost/backend/internal/ai"
	"github.com/stretchr/testify/require"
)

// fixedSource answers for the models it lists, like a provider's own model list.
type fixedSource map[string]ai.ReasoningProfile

func (s fixedSource) ReasoningProfile(_ context.Context, model string) (ai.ReasoningProfile, bool) {
	profile, ok := s[model]
	return profile, ok
}

func TestTheEffortAndAnOutputBudgetThatFitsItReachTheModel(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		model  string
		effort string
		want   ai.ReasoningEffort
		budget int64
	}{
		{"deepseek-v4.1-flash", "", "", 24_000},
		{"deepseek-v4.1-flash", "auto", "", 24_000},
		{"deepseek-v4.1-flash", "none", ai.ReasoningEffortNone, 24_000},
		{"gemini-4-argon", "high", ai.ReasoningEffortHigh, 32_000},
		{"gpt-6-luna", "xhigh", ai.ReasoningEffortXHigh, 48_000},
		{"deepseek-v4.1-flash", "max", ai.ReasoningEffortMax, 64_000},
	}
	turns := make([]chatTurn, len(cases))
	for index := range turns {
		turns[index] = answer("Done.")
	}
	service, chat := newAgentService(t, turns...)
	conversation := newAgentConversation(t, service)

	for index, tc := range cases {
		label := tc.model + " " + tc.effort
		run, err := service.StartRun(ctx, StartRunInput{
			ConversationID: conversation.ID, UserID: "admin", Prompt: "Think.", Model: tc.model, Effort: tc.effort,
		})
		require.NoError(t, err, label)
		require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status, label)
		requests := chat.snapshot()
		require.Len(t, requests, index+1, label)
		require.Equal(t, tc.model, requests[index].Model, label)
		require.Equal(t, tc.want, requests[index].ReasoningEffort, label)
		require.Equal(t, tc.budget, requests[index].MaxOutputTokens, label)
	}
}

func TestAnEffortTheModelDoesNotOfferIsRefusedBeforeAnyRunStarts(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t)
	conversation := newAgentConversation(t, service)

	_, err := service.StartRun(ctx, StartRunInput{
		ConversationID: conversation.ID, UserID: "admin", Prompt: "Think.", Model: "glm-5.3", Effort: "none",
	})
	var unsupported *UnsupportedEffortError
	require.ErrorAs(t, err, &unsupported)
	require.ErrorIs(t, err, ErrInvalidEffort, "callers that reject an invalid effort reject this one too")
	require.Equal(t, "GLM 5.3", unsupported.Model)
	require.Equal(t, []string{"low", "high", "max"}, unsupported.Supported)
	require.Empty(t, chat.snapshot(), "a refused run never reaches the model")

	view, err := service.GetConversation(ctx, conversation.ID)
	require.NoError(t, err)
	require.Empty(t, view.Runs, "a refused run leaves nothing behind")
}

func TestAnImageFallbackKeepsTheEffortWithinWhatTheAnsweringModelOffers(t *testing.T) {
	ctx := context.Background()
	service, chat := newAgentService(t, answer("A red rectangle."))
	service.SetAgent(chat, "see", []ModelOption{
		{ID: "see", Label: "See", Description: "Reads images.", Vision: true, Efforts: effortList(effortLow, effortHigh)},
		{ID: "think", Label: "Think", Description: "Text only.", Efforts: effortList(effortOff, effortHigh, effortMax)},
	})
	conversation := newAgentConversation(t, service)
	attachment, err := service.SaveAttachment(ctx, "admin", "shot.png", testPNG(t))
	require.NoError(t, err)

	run, err := service.StartRun(ctx, StartRunInput{
		ConversationID: conversation.ID, UserID: "admin", Prompt: "What is this?",
		AttachmentIDs: []string{attachment.ID}, Model: "think", Effort: "max",
	})
	require.NoError(t, err)
	require.Equal(t, string(RunSucceeded), waitForTerminalRun(t, service, run.ID).Status)

	requests := chat.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, "see", requests[0].Model)
	require.Equal(t, ai.ReasoningEffortHigh, requests[0].ReasoningEffort, "max is stepped down to what the answering model offers")
	require.EqualValues(t, 32_000, requests[0].MaxOutputTokens, "the budget follows the effort that is actually used")
	var said string
	for _, event := range eventsOf(t, service, run.ID) {
		if event.Kind == "status" && strings.Contains(event.AgentMessage, "reasoning") {
			said = event.AgentMessage
		}
	}
	require.Equal(t, "See does not offer max reasoning, so high is used", said)
}

func TestModelsOutsideTheCuratedListOfferWhatTheirProviderSays(t *testing.T) {
	ctx := context.Background()
	chat := &scriptedChat{}
	service := NewService(adminAgentTestDB(t), nil)
	// The source is set before the agent on purpose: either order must work.
	service.SetReasoningSource(fixedSource{"vendor/new-model": ai.NewReasoningProfile(ai.ReasoningEffortLow, ai.ReasoningEffortXHigh)})
	service.SetAgent(chat, "vendor/new-model", nil)

	defaultModel, options := service.Models(ctx)
	require.Equal(t, "vendor/new-model", defaultModel)
	require.Len(t, options, 1)
	require.Equal(t, []string{"low", "xhigh"}, options[0].Efforts)

	service.SetReasoningSource(nil)
	_, options = service.Models(ctx)
	require.Len(t, options, 1)
	require.Equal(t, []string{"low", "medium", "high"}, options[0].Efforts, "a model nothing is known about gets the common levels")

	service.SetAgent(chat, "gpt-4o-mini", nil)
	service.SetReasoningSource(ai.OpenAIReasoning{})
	_, options = service.Models(ctx)
	require.Len(t, options, 1)
	require.Equal(t, []string{}, options[0].Efforts, "a model that does not reason has no effort to choose")
}

func TestTheCuratedListNeverNeedsTheProviderToAnswer(t *testing.T) {
	ctx := context.Background()
	service := NewService(adminAgentTestDB(t), nil)
	service.SetReasoningSource(fixedSource{"gpt-6-luna": ai.NewReasoningProfile(ai.ReasoningEffortLow)})
	service.SetAgent(&scriptedChat{}, "gpt-6-luna", OpenCodeModels())

	_, options := service.Models(ctx)
	for _, option := range options {
		if option.ID == "gpt-6-luna" {
			require.Equal(t, []string{"none", "low", "medium", "high", "xhigh", "max"}, option.Efforts, "the curated list wins over a source")
		}
		require.NotNil(t, option.Efforts, option.ID)
	}
}
