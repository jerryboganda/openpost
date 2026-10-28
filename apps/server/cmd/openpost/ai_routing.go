package main

import (
	"strings"
	"time"

	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/config"
)

const (
	contentAIRequestTimeout = 240 * time.Second
	contentAIMaxRetries     = 12
)

func aiProviderConfigs(cfg *config.Config) (ai.OpenRouterConfig, ai.OpenRouterConfig) {
	headers := map[string]string{}
	if cfg.AIProvider == config.AIProviderOpenCode && strings.TrimSpace(cfg.AISessionID) != "" {
		headers["x-opencode-session"] = strings.TrimSpace(cfg.AISessionID)
	}
	base := ai.OpenRouterConfig{
		Mode:                    aiProviderMode(cfg.AIProvider),
		APIKey:                  cfg.EffectiveAIAPIKey(),
		BaseURL:                 cfg.EffectiveAIBaseURL(),
		HTTPReferer:             cfg.PublicURL,
		XTitle:                  "OpenPost",
		ProviderLabel:           aiProviderLabel(cfg.AIProvider),
		DisableStructuredOutput: !cfg.AIStructuredOutput,
		DisableWebSearch:        !cfg.AIWebSearch,
		ReasoningEffortOverride: cfg.AIReasoningEffort,
		Headers:                 headers,
	}
	imageConfig := base
	imageConfig.Timeout = 90 * time.Second
	imageConfig.ReasoningEffortOverride = "none"
	imageConfig.Provider = cfg.ImageCaptionProvider
	imageConfig.RequireZDR = cfg.ImageCaptionRequireZDR
	contentConfig := base
	contentConfig.Timeout = contentAIRequestTimeout
	contentConfig.MaxRetries = contentAIMaxRetries
	contentConfig.Provider = cfg.ContentAIProvider
	contentConfig.RequireZDR = cfg.ContentAIRequireZDR
	return imageConfig, contentConfig
}

func aiProviderMode(provider string) ai.ProviderMode {
	if provider == "" || provider == config.AIProviderOpenRouter {
		return ai.ProviderModeOpenRouter
	}
	return ai.ProviderModeOpenAICompatible
}

func aiProviderLabel(provider string) string {
	switch provider {
	case config.AIProviderOpenCode:
		return "OpenCode"
	case config.AIProviderOpenAI:
		return "OpenAI-compatible"
	default:
		return "OpenRouter"
	}
}
