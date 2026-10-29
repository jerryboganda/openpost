package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/api/middleware"
	"github.com/uptrace/bun"
)

// The diagnostic is an explicit, administrator-triggered paid model request. It
// stays separate from liveness and readiness so routine probes never spend
// model budget or depend on an external provider.
const (
	aiDiagnosticPrompt  = "Reply with exactly OK."
	aiDiagnosticTimeout = 60 * time.Second
)

// AIDiagnostic code values. They separate "the instance is not configured"
// from "the provider rejected us" so an operator can act without reading
// provider response bodies, which are never returned.
const (
	aiDiagnosticCodeOK                 = "ok"
	aiDiagnosticCodeNotConfigured      = "not_configured"
	aiDiagnosticCodeInvalidCredentials = "invalid_credentials"
	aiDiagnosticCodeRateLimited        = "rate_limited"
	aiDiagnosticCodeProviderError      = "provider_error"
	aiDiagnosticCodeTimeout            = "timeout"
	aiDiagnosticCodeEmptyResponse      = "empty_response"
	aiDiagnosticCodeMalformedResponse  = "malformed_response"
)

type AIDiagnosticHandler struct {
	db        *bun.DB
	auth      middleware.Authenticator
	generator ai.Generator
	provider  string
	model     string
	timeout   time.Duration
}

type AIDiagnosticOutput struct {
	Body struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Valid     bool   `json:"valid"`
		Code      string `json:"code"`
		Failure   string `json:"failure,omitempty"`
		LatencyMS int64  `json:"latency_ms"`
	}
}

func NewAIDiagnosticHandler(
	db *bun.DB,
	auth middleware.Authenticator,
	generator ai.Generator,
	provider,
	model string,
) *AIDiagnosticHandler {
	return &AIDiagnosticHandler{
		db:        db,
		auth:      auth,
		generator: generator,
		provider:  provider,
		model:     model,
		timeout:   aiDiagnosticTimeout,
	}
}

func (h *AIDiagnosticHandler) RegisterRoutes(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "run-instance-ai-diagnostic", Method: http.MethodPost, Path: "/admin/ai-diagnostic",
		Summary: "Test the configured AI provider", Tags: []string{"Admin"},
		Middlewares: huma.Middlewares{middleware.AuthMiddleware(api, h.auth)},
	}, h.run)
}

func (h *AIDiagnosticHandler) run(ctx context.Context, _ *struct{}) (out *AIDiagnosticOutput, err error) {
	out = &AIDiagnosticOutput{}
	out.Body.Provider = h.provider
	out.Body.Model = h.model
	started := time.Now()
	defer func() {
		out.Body.LatencyMS = time.Since(started).Milliseconds()
	}()

	if err := requireBrowserSessionInstanceAdmin(ctx, h.db); err != nil {
		return nil, err
	}
	if h.generator == nil || strings.TrimSpace(h.model) == "" {
		out.Body.Code = aiDiagnosticCodeNotConfigured
		out.Body.Failure = "AI provider is not configured"
		return out, nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	result, generateErr := h.generator.Generate(requestCtx, ai.GenerateRequest{
		Model:           h.model,
		SystemPrompt:    "You are a connectivity diagnostic.",
		UserPrompt:      aiDiagnosticPrompt,
		MaxOutputTokens: 64,
	})
	if generateErr != nil {
		out.Body.Code, out.Body.Failure = classifyAIDiagnosticFailure(requestCtx, generateErr)
		return out, nil
	}
	if strings.TrimSpace(result.Text) != "OK" {
		out.Body.Code = aiDiagnosticCodeMalformedResponse
		out.Body.Failure = "AI provider returned an unexpected response"
		return out, nil
	}
	out.Body.Valid = true
	out.Body.Code = aiDiagnosticCodeOK
	return out, nil
}

// classifyAIDiagnosticFailure maps provider errors onto stable, operator-facing
// codes. It never forwards provider messages because they can echo prompts,
// account details, or credentials.
func classifyAIDiagnosticFailure(ctx context.Context, err error) (code, failure string) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return aiDiagnosticCodeTimeout, "AI provider request timed out"
	}
	if errors.Is(err, ai.ErrEmptyResponse) {
		return aiDiagnosticCodeEmptyResponse, "AI provider returned an empty response"
	}
	var providerErr *ai.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return aiDiagnosticCodeInvalidCredentials, "AI provider rejected the configured credentials"
		case http.StatusTooManyRequests:
			return aiDiagnosticCodeRateLimited, "AI provider rate limited the diagnostic request"
		}
	}
	return aiDiagnosticCodeProviderError, "AI provider request failed"
}
