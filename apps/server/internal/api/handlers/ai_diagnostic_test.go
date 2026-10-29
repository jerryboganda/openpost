package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humaecho"
	"github.com/labstack/echo/v4"
	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/api/middleware"
	"github.com/openpost/backend/internal/models"
	"github.com/stretchr/testify/require"
)

type diagnosticGenerator struct {
	text  string
	err   error
	calls int
	block bool
}

func (g *diagnosticGenerator) Generate(ctx context.Context, _ ai.GenerateRequest) (ai.GenerateResult, error) {
	g.calls++
	if g.block {
		<-ctx.Done()
		return ai.GenerateResult{}, ctx.Err()
	}
	return ai.GenerateResult{Text: g.text}, g.err
}

type diagnosticResponse struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Valid     bool   `json:"valid"`
	Code      string `json:"code"`
	Failure   string `json:"failure"`
	LatencyMS int64  `json:"latency_ms"`
}

func TestAIDiagnostic(t *testing.T) {
	type testCase struct {
		name       string
		admin      bool
		auth       middleware.Authenticator
		model      string
		text       string
		err        error
		block      bool
		timeout    time.Duration
		status     int
		valid      bool
		code       string
		wantCalls  int
		wantLeaked string
	}
	cases := []testCase{
		{name: "success", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", text: "OK", status: 200, valid: true, code: aiDiagnosticCodeOK, wantCalls: 1},
		{name: "trims whitespace", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", text: "\n OK \n", status: 200, valid: true, code: aiDiagnosticCodeOK, wantCalls: 1},
		{name: "malformed response", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", text: "wrong", status: 200, code: aiDiagnosticCodeMalformedResponse, wantCalls: 1},
		{name: "not configured", admin: true, auth: browserSessionTestAuthenticator(), model: "", text: "OK", status: 200, code: aiDiagnosticCodeNotConfigured, wantCalls: 0},
		{name: "empty response", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", err: ai.ErrEmptyResponse, status: 200, code: aiDiagnosticCodeEmptyResponse, wantCalls: 1},
		{name: "invalid credentials", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", err: &ai.ProviderError{Provider: "OpenCode", StatusCode: http.StatusUnauthorized}, status: 200, code: aiDiagnosticCodeInvalidCredentials, wantCalls: 1},
		{name: "rate limited", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", err: &ai.ProviderError{Provider: "OpenCode", StatusCode: http.StatusTooManyRequests}, status: 200, code: aiDiagnosticCodeRateLimited, wantCalls: 1},
		{name: "provider error", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", err: &ai.ProviderError{Provider: "OpenCode", StatusCode: http.StatusBadGateway}, status: 200, code: aiDiagnosticCodeProviderError, wantCalls: 1},
		{name: "timeout", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", block: true, timeout: 10 * time.Millisecond, status: 200, code: aiDiagnosticCodeTimeout, wantCalls: 1},
		{name: "provider message never leaks", admin: true, auth: browserSessionTestAuthenticator(), model: "test-model", err: errors.New("secret provider response with key sk-live"), status: 200, code: aiDiagnosticCodeProviderError, wantCalls: 1, wantLeaked: "sk-live"},
		{name: "ordinary user", admin: false, auth: browserSessionTestAuthenticator(), model: "test-model", text: "OK", status: 403, wantCalls: 0},
		{name: "api token", admin: true, auth: unboundCLIFullTestAuthenticator(), model: "test-model", text: "OK", status: 403, wantCalls: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := createHandlerTestDB(t, (*models.User)(nil))
			_, err := db.NewInsert().Model(&models.User{
				ID: "user-1", Email: "admin@example.com", DisplayName: "Admin",
				PasswordHash: "hash", IsAdmin: tc.admin, CreatedAt: time.Now().UTC(),
			}).Exec(t.Context())
			require.NoError(t, err)

			e := echo.New()
			api := humaecho.NewWithGroup(e, e.Group("/api/v1"), huma.DefaultConfig("Test", "1.0.0"))
			generator := &diagnosticGenerator{text: tc.text, err: tc.err, block: tc.block}
			handler := NewAIDiagnosticHandler(db, tc.auth, generator, "opencode", tc.model)
			if tc.timeout > 0 {
				handler.timeout = tc.timeout
			}
			handler.RegisterRoutes(api)

			response := requestAIPrompts(t, e, http.MethodPost, "/api/v1/admin/ai-diagnostic", nil)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, tc.wantCalls, generator.calls)
			if tc.wantLeaked != "" {
				require.NotContains(t, response.Body.String(), tc.wantLeaked)
			}
			if tc.status != http.StatusOK {
				return
			}

			var body diagnosticResponse
			require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
			require.Equal(t, "opencode", body.Provider)
			require.Equal(t, tc.model, body.Model)
			require.Equal(t, tc.valid, body.Valid)
			require.Equal(t, tc.code, body.Code)
			require.GreaterOrEqual(t, body.LatencyMS, int64(0))
			if tc.valid {
				require.Empty(t, body.Failure)
			} else {
				require.NotEmpty(t, body.Failure)
			}
		})
	}
}
