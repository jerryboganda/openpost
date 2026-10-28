package publicationbuilder

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/openpost/backend/internal/ai"
)

const MaxSeriesCount = 3

type SeriesConcept struct {
	ID           string   `json:"id"`
	Revision     int      `json:"revision"`
	Title        string   `json:"title" maxLength:"200"`
	Brief        string   `json:"brief" maxLength:"1500"`
	Evidence     string   `json:"evidence" maxLength:"1000"`
	SourceRefs   []string `json:"source_refs"`
	ChildBuildID string   `json:"child_build_id,omitempty"`
}

type SeriesOrigin struct {
	BuildID      string `json:"build_id"`
	ItemID       string `json:"item_id"`
	ItemRevision int    `json:"item_revision"`
}

type generatedSeriesConcept struct {
	Title      string   `json:"title"`
	Brief      string   `json:"brief"`
	Evidence   string   `json:"evidence"`
	SourceRefs []string `json:"source_refs"`
}
type generatedSeries struct {
	Concepts []generatedSeriesConcept `json:"concepts"`
}

const seriesPrompt = `ROLE: source-backed series editor
Propose a small series of complementary standalone social posts from the supplied work, launch, lesson or evidence.
Each concept must offer a DISTINCT reader benefit, fact, decision or useful perspective. These are separate posts, not alternative hooks for the same post, a thread, or per-platform variants.
Do not pad a thin source into the requested count. Return fewer concepts when the source only supports fewer useful posts, including a single concept when appropriate.
Never invent product capabilities, customer stories, metrics, announcements, tutorials, timelines or promises. Do not manufacture controversy or a calendar campaign.
Respect the author's locked direction and account voice. Describe what each post should cover in its brief and name the actual supplied evidence in evidence. source_refs must identify the supplied sources, using idea for user-authored facts.
No draft posts, publishing dates or automatic scheduling. The author chooses and edits concepts before any post is generated.
Return only the JSON concepts object.`

func (service *Service) planSeries(ctx context.Context, input BuildInput) (BuildResult, error) {
	payload := struct {
		Context   generationContext `json:"context"`
		Direction DirectionInput    `json:"direction"`
		Count     int               `json:"maximum_posts"`
	}{generationContextFor(input, input.Destinations), input.Direction, input.SeriesCount}
	prompt, err := encodeGenerationInput("series_input", payload)
	if err != nil {
		return BuildResult{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	properties := map[string]any{"title": map[string]any{"type": "string"}, "brief": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "string"}, "source_refs": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}
	result, err := generateValidated(requestCtx, service, "series", "", ai.GenerateRequest{SystemPrompt: seriesPrompt, UserPrompt: prompt, MaxOutputTokens: 8000, ReasoningEffort: ai.ReasoningEffortMedium, Parts: input.Parts, Images: input.Images, Files: input.Files, Audio: input.Audio, Videos: input.Videos, ResponseSchema: &ai.JSONSchema{Name: "post_series", Schema: map[string]any{"type": "object", "additionalProperties": false, "required": []string{"concepts"}, "properties": map[string]any{"concepts": map[string]any{"type": "array", "minItems": 1, "maxItems": input.SeriesCount, "items": map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": []string{"title", "brief", "evidence", "source_refs"}}}}}}}, func(result *generatedSeries) error {
		if len(result.Concepts) < 1 || len(result.Concepts) > input.SeriesCount {
			return errors.New("return between one and the requested number of supported concepts")
		}
		refs := map[string]bool{"idea": strings.TrimSpace(input.Idea) != ""}
		for _, source := range input.Sources {
			refs[source.ID] = true
		}
		seen := map[string]bool{}
		for _, concept := range result.Concepts {
			if err := validateSeriesConcept(concept.Title, concept.Brief, concept.Evidence); err != nil {
				return err
			}
			key := strings.ToLower(strings.TrimSpace(concept.Title))
			if seen[key] {
				return errors.New("series concepts must be distinct")
			}
			seen[key] = true
			if len(concept.SourceRefs) == 0 {
				return errors.New("each concept needs supplied evidence")
			}
			for _, ref := range concept.SourceRefs {
				if !refs[ref] {
					return errors.New("unknown series source reference")
				}
			}
		}
		return nil
	})
	if err != nil {
		return BuildResult{}, err
	}
	concepts := make([]SeriesConcept, 0, len(result.Concepts))
	for i, concept := range result.Concepts {
		concepts = append(concepts, SeriesConcept{ID: fmt.Sprintf("post-%d", i+1), Revision: 1, Title: strings.TrimSpace(concept.Title), Brief: strings.TrimSpace(concept.Brief), Evidence: strings.TrimSpace(concept.Evidence), SourceRefs: concept.SourceRefs})
	}
	return BuildResult{Series: concepts}, nil
}

func validateSeriesConcept(title, brief, evidence string) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(brief) == "" || strings.TrimSpace(evidence) == "" || utf8.RuneCountInString(title) > 200 || utf8.RuneCountInString(brief) > MaxDirectionAngleCharacters || utf8.RuneCountInString(evidence) > 1000 {
		return errors.New("each concept requires a short title, brief and supplied evidence")
	}
	return nil
}
