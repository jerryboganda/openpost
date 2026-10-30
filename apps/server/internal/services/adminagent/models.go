package adminagent

import (
	"errors"
	"strings"

	"github.com/openpost/backend/internal/ai"
)

const (
	AccessFull     = "full"
	AccessReadOnly = "read_only"
)

var (
	ErrInvalidModel  = errors.New("unsupported model")
	ErrInvalidEffort = errors.New("reasoning effort must be auto, low, medium, or high")
	ErrInvalidAccess = errors.New("access must be full or read_only")
)

// ModelOption is one model an administrator may choose for a run.
type ModelOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Vision      bool   `json:"vision"`
}

// OpenCodeModels lists models verified to answer tool-calling requests on the
// OpenCode Go endpoint. Vision follows each model's published input types.
func OpenCodeModels() []ModelOption {
	return []ModelOption{
		{ID: "deepseek-v4.1-flash", Label: "DeepSeek V4.1 Flash", Description: "Fast and low cost. Reads images.", Vision: true},
		{ID: "kimi-k3", Label: "Kimi K3", Description: "Strong at long multi-step tasks. Reads images.", Vision: true},
		{ID: "kimi-k2.7-code", Label: "Kimi K2.7 Code", Description: "Tuned for code changes. Reads images.", Vision: true},
		{ID: "gpt-6-luna", Label: "GPT-6 Luna", Description: "Balanced general model. Reads images.", Vision: true},
		{ID: "gemini-4-argon", Label: "Gemini 4 Argon", Description: "Large context. Reads images.", Vision: true},
		{ID: "qwen3.8-max", Label: "Qwen 3.8 Max", Description: "Strong reasoning. Reads images.", Vision: true},
		{ID: "grok-4.7", Label: "Grok 4.7", Description: "Fast agentic model. Reads images.", Vision: true},
		{ID: "minimax-m3", Label: "MiniMax M3", Description: "Long-context agent model. Reads images.", Vision: true},
		{ID: "glm-5.3", Label: "GLM 5.3", Description: "Text only.", Vision: false},
		{ID: "deepseek-v4-pro", Label: "DeepSeek V4 Pro", Description: "Deep reasoning. Text only.", Vision: false},
	}
}

// modelCatalog resolves administrator choices against the configured default
// and the models the deployment offers.
type modelCatalog struct {
	defaultID string
	options   []ModelOption
}

func newModelCatalog(defaultID string, options []ModelOption) modelCatalog {
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
	return modelCatalog{defaultID: defaultID, options: merged}
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
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return "", nil
	case "low":
		return ai.ReasoningEffortLow, nil
	case "medium":
		return ai.ReasoningEffortMedium, nil
	case "high":
		return ai.ReasoningEffortHigh, nil
	default:
		return "", ErrInvalidEffort
	}
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
