package publicationdiscovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/services/sourcecontext"
	"github.com/stretchr/testify/require"
)

type recordingGenerator struct {
	request ai.GenerateRequest
	text    string
}

func (g *recordingGenerator) Generate(_ context.Context, request ai.GenerateRequest) (ai.GenerateResult, error) {
	g.request = request
	return ai.GenerateResult{Text: g.text, Model: "deepseek-v4.1-flash"}, nil
}

type failingSourceLoader struct{}

func (failingSourceLoader) Load(context.Context, string) (sourcecontext.Document, error) {
	return sourcecontext.Document{}, errors.New("source loading must not run without live web search")
}

func TestDiscoverWithoutLiveWebSearchDropsCitations(t *testing.T) {
	today := time.Now().UTC().Format(time.DateOnly)
	generator := &recordingGenerator{text: fmt.Sprintf(`{
		"opportunities": [{
			"title": "Practical AI workflows for solo founders",
			"why_it_fits": "It matches the audience and the selected platform.",
			"why_now": "The focus is a durable operating question for small teams.",
			"signal_date": %q,
			"hook": "A workflow is only useful when it survives a busy week.",
			"angles": [
				{"label": "Audit", "thesis": "Start from recurring work.", "approach": "List the work that repeats every week."},
				{"label": "Route", "thesis": "Match the model to the task.", "approach": "Separate classification, drafting, and review."},
				{"label": "Review", "thesis": "Keep a human checkpoint.", "approach": "Approve every customer-facing output."}
			],
			"sources": [],
			"platform_treatments": [{
				"platform": "x",
				"objective": "Share a practical framework",
				"format": "Single post",
				"rationale": "A compact checklist is easy to save.",
				"media": "A three-step checklist image"
			}]
		}]
	}`, today)}

	service, err := New(generator, Config{
		Model:            "deepseek-v4.1-flash",
		SourceLoader:     failingSourceLoader{},
		DisableWebSearch: true,
	})
	require.NoError(t, err)

	result, err := service.Discover(t.Context(), Input{
		Focus:     "Practical AI workflows for solo founders",
		Audience:  "solo founders",
		Voice:     VoiceContext{Name: "OpenPost Test"},
		Platforms: []string{"x"},
		Limit:     1,
	})
	require.NoError(t, err)
	require.Len(t, result.Opportunities, 1)
	require.Empty(t, result.Opportunities[0].Sources)
	require.False(t, generator.request.WebSearch.Enabled)
	require.Contains(t, generator.request.SystemPrompt, "no live web search")

	schema := generator.request.ResponseSchema.Schema
	opportunities := schema["properties"].(map[string]any)["opportunities"].(map[string]any)
	item := opportunities["items"].(map[string]any)
	sources := item["properties"].(map[string]any)["sources"].(map[string]any)
	require.Equal(t, 0, sources["minItems"])
	require.True(t, strings.Contains(sources["type"].(string), "array"))
}
