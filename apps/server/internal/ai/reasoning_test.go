package ai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseReasoningEffortReadsTheWholeLadderAndAuto(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  ReasoningEffort
	}{
		{"", ""},
		{"auto", ""},
		{" AUTO ", ""},
		{"none", ReasoningEffortNone},
		{"minimal", ReasoningEffortMinimal},
		{"low", ReasoningEffortLow},
		{"Medium", ReasoningEffortMedium},
		{" high ", ReasoningEffortHigh},
		{"xhigh", ReasoningEffortXHigh},
		{"max", ReasoningEffortMax},
	} {
		got, ok := ParseReasoningEffort(tc.value)
		require.True(t, ok, tc.value)
		require.Equal(t, tc.want, got, tc.value)
	}
	for _, value := range []string{"extreme", "ultra", "off", "high,max"} {
		_, ok := ParseReasoningEffort(value)
		require.False(t, ok, value)
	}
}

func TestReasoningProfileSortsDropsRepeatsAndUnknownEfforts(t *testing.T) {
	profile := NewReasoningProfile(ReasoningEffortMax, ReasoningEffortLow, ReasoningEffortMax, "wild", ReasoningEffortHigh)
	require.Equal(t, []string{"low", "high", "max"}, profile.Names())
	require.Equal(t, []string{}, NewReasoningProfile().Names(), "an empty profile still encodes as an empty list")
}

func TestReasoningProfileSupportsAutoAndItsOwnEffortsOnly(t *testing.T) {
	profile := NewReasoningProfile(ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax)
	require.True(t, profile.Supports(""))
	require.True(t, profile.Supports(ReasoningEffortHigh))
	require.False(t, profile.Supports(ReasoningEffortMedium))
	require.False(t, profile.Supports(ReasoningEffortXHigh))
	require.True(t, NewReasoningProfile().Supports(""))
	require.False(t, NewReasoningProfile().Supports(ReasoningEffortLow))
}

func TestReasoningProfileNearestStepsDownFirstThenUp(t *testing.T) {
	profile := NewReasoningProfile(ReasoningEffortLow, ReasoningEffortHigh, ReasoningEffortMax)
	for _, tc := range []struct {
		requested ReasoningEffort
		want      ReasoningEffort
	}{
		{"", ""},
		{ReasoningEffortMax, ReasoningEffortMax},
		{ReasoningEffortXHigh, ReasoningEffortHigh},
		{ReasoningEffortMedium, ReasoningEffortLow},
		{ReasoningEffortMinimal, ReasoningEffortLow},
		{ReasoningEffortNone, ReasoningEffortLow},
	} {
		require.Equal(t, tc.want, profile.Nearest(tc.requested), string(tc.requested))
	}
	require.Equal(t, ReasoningEffort(""), NewReasoningProfile().Nearest(ReasoningEffortHigh), "a model without effort control falls back to auto")
}

func TestParseReasoningProfileReadsOperatorListsAndRefusesUnknownNames(t *testing.T) {
	profile, ok := ParseReasoningProfile("low, HIGH ,max,,auto")
	require.True(t, ok)
	require.Equal(t, []string{"low", "high", "max"}, profile.Names())

	profile, ok = ParseReasoningProfile("auto")
	require.True(t, ok)
	require.Empty(t, profile.Names())

	_, ok = ParseReasoningProfile("low,extreme")
	require.False(t, ok)
}

func TestOpenAIReasoningProfilesFollowTheModelsOpenAIPublishes(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{"gpt-4o-mini", []string{}},
		{"gpt-4.1", []string{}},
		{"openai/gpt-5.2-chat", []string{}},
		{"gpt-5", []string{"minimal", "low", "medium", "high"}},
		{"gpt-5-mini", []string{"minimal", "low", "medium", "high"}},
		{"gpt-5-pro", []string{"high"}},
		{"gpt-5.1", []string{"none", "low", "medium", "high"}},
		{"gpt-5.1-codex", []string{"low", "medium", "high"}},
		{"gpt-5.4", []string{"none", "low", "medium", "high", "xhigh"}},
		{"gpt-5.4-pro", []string{"medium", "high", "xhigh"}},
		{"gpt-5.2-codex", []string{"low", "medium", "high", "xhigh"}},
		{"gpt-5.6-luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"gpt-6-luna", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"openai/gpt-6-luna:free", []string{"none", "low", "medium", "high", "xhigh", "max"}},
		{"gpt-6.1-terra-pro", []string{"low", "medium", "high", "xhigh", "max"}},
		{"o3", []string{"low", "medium", "high"}},
		{"o4-mini", []string{"low", "medium", "high"}},
	} {
		profile, ok := OpenAIReasoningProfile(tc.model)
		require.True(t, ok, tc.model)
		require.Equal(t, tc.want, profile.Names(), tc.model)
	}
	for _, model := range []string{"", "llama-4-scout", "deepseek-v4.1-flash", "gpt-oss-120b"} {
		_, ok := OpenAIReasoningProfile(model)
		require.False(t, ok, model)
	}
}

func TestReasoningSourcesKeepTheFirstAnswerAndSkipSilentOnes(t *testing.T) {
	sources := ReasoningSources{
		nil,
		OpenAIReasoning{},
		FixedReasoning{Profile: NewReasoningProfile(ReasoningEffortHigh)},
	}
	profile, ok := sources.ReasoningProfile(t.Context(), "gpt-6-luna")
	require.True(t, ok)
	require.Contains(t, profile.Names(), "max", "the OpenAI source answers before the fixed one")

	profile, ok = sources.ReasoningProfile(t.Context(), "some-local-model")
	require.True(t, ok)
	require.Equal(t, []string{"high"}, profile.Names(), "the fixed source answers when the OpenAI source does not know the model")

	_, ok = ReasoningSources{OpenAIReasoning{}}.ReasoningProfile(t.Context(), "some-local-model")
	require.False(t, ok)
}
