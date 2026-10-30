package adminagent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openpost/backend/internal/ai"
)

const (
	AccessFull     = "full"
	AccessReadOnly = "read_only"
)

var (
	ErrInvalidModel  = errors.New("unsupported model")
	ErrInvalidEffort = errors.New("reasoning effort must be auto, none, minimal, low, medium, high, xhigh, or max")
	ErrInvalidAccess = errors.New("access must be full or read_only")
)

// UnsupportedEffortError says which reasoning efforts the chosen model offers.
// It unwraps to ErrInvalidEffort, so callers that reject an invalid effort
// reject this one too.
type UnsupportedEffortError struct {
	Model     string
	Effort    string
	Supported []string
}

func (e *UnsupportedEffortError) Error() string {
	if len(e.Supported) == 0 {
		return fmt.Sprintf("%s has no reasoning levels to choose, so use auto", e.Model)
	}
	return fmt.Sprintf("%s does not offer %s reasoning; choose auto, %s", e.Model, e.Effort, strings.Join(e.Supported, ", "))
}

func (e *UnsupportedEffortError) Unwrap() error { return ErrInvalidEffort }

// ModelOption is one model an administrator may choose for a run.
type ModelOption struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Vision      bool     `json:"vision"`
	Efforts     []string `json:"efforts" doc:"Reasoning efforts the model accepts besides auto, lowest first. Empty when the model has no effort to choose."`
}

const (
	effortOff    = ai.ReasoningEffortNone
	effortLow    = ai.ReasoningEffortLow
	effortMedium = ai.ReasoningEffortMedium
	effortHigh   = ai.ReasoningEffortHigh
	effortXHigh  = ai.ReasoningEffortXHigh
	effortMax    = ai.ReasoningEffortMax
)

func effortList(values ...ai.ReasoningEffort) []string {
	return ai.NewReasoningProfile(values...).Names()
}

// OpenCodeModels lists models verified to answer tool-calling requests on the
// OpenCode Go endpoint. Vision follows each model's published input types.
//
// Efforts are the reasoning levels each model really has. They were checked
// three ways: against what the endpoint accepts, against the OpenCode model
// registry, and against the lists OpenRouter publishes for the same models.
// Aliases are left out, so DeepSeek lists its own low, high, and max rather
// than medium and xhigh, which it only maps onto those. Models that cannot turn
// thinking off leave "none" out, and models with no graded effort list nothing.
func OpenCodeModels() []ModelOption {
	return []ModelOption{
		{ID: "deepseek-v4.1-flash", Label: "DeepSeek V4.1 Flash", Description: "Fast and low cost. Reads images.", Vision: true, Efforts: effortList(effortOff, effortLow, effortHigh, effortMax)},
		{ID: "kimi-k3", Label: "Kimi K3", Description: "Strong at long multi-step tasks. Reads images.", Vision: true, Efforts: effortList(effortOff, effortLow, effortHigh, effortMax)},
		{ID: "kimi-k2.7-code", Label: "Kimi K2.7 Code", Description: "Tuned for code changes. Reads images.", Vision: true, Efforts: effortList()},
		{ID: "gpt-6-luna", Label: "GPT-6 Luna", Description: "Balanced general model. Reads images.", Vision: true, Efforts: effortList(effortOff, effortLow, effortMedium, effortHigh, effortXHigh, effortMax)},
		{ID: "gemini-4-argon", Label: "Gemini 4 Argon", Description: "Large context. Reads images.", Vision: true, Efforts: effortList(effortLow, effortMedium, effortHigh)},
		{ID: "qwen3.8-max", Label: "Qwen 3.8 Max", Description: "Strong reasoning. Reads images.", Vision: true, Efforts: effortList(effortOff, effortLow, effortMedium, effortHigh, effortXHigh)},
		{ID: "grok-4.7", Label: "Grok 4.7", Description: "Fast agentic model. Reads images.", Vision: true, Efforts: effortList(effortLow, effortMedium, effortHigh, effortXHigh)},
		{ID: "minimax-m3", Label: "MiniMax M3", Description: "Long-context agent model. Reads images.", Vision: true, Efforts: effortList()},
		{ID: "glm-5.3", Label: "GLM 5.3", Description: "Text only.", Vision: false, Efforts: effortList(effortLow, effortHigh, effortMax)},
		{ID: "deepseek-v4-pro", Label: "DeepSeek V4 Pro", Description: "Deep reasoning. Text only.", Vision: false, Efforts: effortList(effortOff, effortHigh, effortMax)},
	}
}

// modelCatalog resolves administrator choices against the configured default
// and the models the deployment offers.
type modelCatalog struct {
	defaultID string
	options   []ModelOption
	// source answers for models whose efforts the list above does not state.
	source ai.ReasoningSource
}

func newModelCatalog(defaultID string, options []ModelOption, source ai.ReasoningSource) modelCatalog {
	defaultID = strings.TrimSpace(defaultID)
	merged := make([]ModelOption, 0, len(options)+1)
	known := false
	for _, option := range options {
		if option.ID == defaultID {
			known = true
		}
		merged = append(merged, option)
	}
	if !known && defaultID != "" {
		// A deployment-chosen model outside the curated list is still valid.
		// Its vision support is unknown, so image runs fall back to a model that
		// is known to read images.
		merged = append([]ModelOption{{ID: defaultID, Label: defaultID, Description: "Configured for this instance."}}, merged...)
	}
	return modelCatalog{defaultID: defaultID, options: merged, source: source}
}

func (c modelCatalog) resolve(requested string) (ModelOption, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = c.defaultID
	}
	for _, option := range c.options {
		if strings.EqualFold(option.ID, requested) {
			return option, nil
		}
	}
	return ModelOption{}, ErrInvalidModel
}

// profile returns the reasoning efforts of one model. A curated list wins.
// Otherwise the provider's own list or the documented model families answer,
// and a model nothing is known about gets the levels almost every reasoning
// endpoint accepts.
func (c modelCatalog) profile(ctx context.Context, option ModelOption) ai.ReasoningProfile {
	if option.Efforts != nil {
		profile, _ := ai.ParseReasoningProfile(strings.Join(option.Efforts, ","))
		return profile
	}
	if c.source != nil {
		if profile, ok := c.source.ReasoningProfile(ctx, option.ID); ok {
			return profile
		}
	}
	return ai.DefaultReasoningProfile()
}

// withEfforts returns the selectable models with every reasoning list filled in.
func (c modelCatalog) withEfforts(ctx context.Context) []ModelOption {
	options := make([]ModelOption, len(c.options))
	for index, option := range c.options {
		option.Efforts = c.profile(ctx, option).Names()
		options[index] = option
	}
	return options
}

// visionModel returns the model to use when images are attached. It prefers
// the requested model, then the default, then any model that reads images.
func (c modelCatalog) visionModel(preferred ModelOption) (ModelOption, bool) {
	if preferred.Vision {
		return preferred, true
	}
	for _, option := range c.options {
		if option.ID == c.defaultID && option.Vision {
			return option, true
		}
	}
	for _, option := range c.options {
		if option.Vision {
			return option, true
		}
	}
	return ModelOption{}, false
}

func normalizeEffort(value string) (ai.ReasoningEffort, error) {
	effort, ok := ai.ParseReasoningEffort(value)
	if !ok {
		return "", ErrInvalidEffort
	}
	return effort, nil
}

func normalizeAccess(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", AccessFull:
		return AccessFull, nil
	case AccessReadOnly:
		return AccessReadOnly, nil
	default:
		return "", ErrInvalidAccess
	}
}
