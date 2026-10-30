package main

import (
	"context"
	"log"
	"time"

	"github.com/uptrace/bun"

	"github.com/openpost/backend/internal/ai"
	"github.com/openpost/backend/internal/config"
	operationallogging "github.com/openpost/backend/internal/operational/logging"
	"github.com/openpost/backend/internal/services/adminagent"
	"github.com/openpost/backend/internal/services/auth"
	"github.com/openpost/backend/internal/services/sessions"
)

var processStartedAt = time.Now()

// agentSessionIssuer opens a short-lived session for the administrator who owns
// a conversation. The agent's own API calls then pass through the same
// authentication and authorization as a browser, and the session is revoked
// when the run ends.
type agentSessionIssuer struct {
	db       *bun.DB
	sessions *sessions.Service
	auth     *auth.Service
}

func (i agentSessionIssuer) IssueSession(ctx context.Context, userID string, ttl time.Duration) (string, func(context.Context), error) {
	var email string
	if err := i.db.NewSelect().Table("users").Column("email").Where("id = ?", userID).Scan(ctx, &email); err != nil {
		return "", nil, err
	}
	expiresAt := time.Now().UTC().Add(ttl)
	session, err := i.sessions.CreateSession(ctx, sessions.CreateInput{
		UserID:    userID,
		UserAgent: "OpenPost administrator agent",
		IPAddress: "127.0.0.1",
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", nil, err
	}
	token, err := i.auth.GenerateTokenWithSession(userID, email, session.ID, expiresAt)
	if err != nil {
		_ = i.sessions.RevokeSession(ctx, userID, session.ID)
		return "", nil, err
	}
	revoke := func(revokeCtx context.Context) {
		_ = i.sessions.RevokeSession(revokeCtx, userID, session.ID)
	}
	return token, revoke, nil
}

// agentReasoningSource chooses where the agent learns which reasoning efforts a
// model offers when the built-in list does not say. An operator's list answers
// first. OpenRouter publishes the efforts of every model it routes to, and other
// endpoints use OpenAI's documented models. A model nothing is known about gets
// the common low, medium, and high.
func agentReasoningSource(cfg *config.Config) ai.ReasoningSource {
	var sources ai.ReasoningSources
	if list := cfg.AgentReasoningEfforts; list != "" {
		if profile, ok := ai.ParseReasoningProfile(list); ok {
			sources = append(sources, ai.FixedReasoning{Profile: profile})
		} else {
			log.Printf("OPENPOST_AGENT_REASONING_EFFORTS ignored: %q must list efforts from none, minimal, low, medium, high, xhigh, and max", list)
		}
	}
	if aiProviderMode(cfg.AIProvider) == ai.ProviderModeOpenRouter {
		_, contentConfig := aiProviderConfigs(cfg)
		sources = append(sources, ai.NewOpenRouterReasoning(contentConfig))
	}
	return append(sources, ai.OpenAIReasoning{})
}

// configureAdminAgent connects the agent to the instance's AI provider, its
// GitHub repository, the application's own router, and recent logs.
func configureAdminAgent(
	service *adminagent.Service,
	cfg *config.Config,
	chat ai.ChatGenerator,
	logs *operationallogging.RecentLines,
	issuer adminagent.SessionIssuer,
) {
	var models []adminagent.ModelOption
	if cfg.AIProvider == config.AIProviderOpenCode {
		models = adminagent.OpenCodeModels()
	}
	service.SetAgent(chat, cfg.EffectiveTextGenerationModel(), models)
	service.SetReasoningSource(agentReasoningSource(cfg))
	service.SetEnvironment(adminagent.Environment{
		PublicURL:      cfg.PublicURL,
		Version:        version,
		Revision:       runningBuildRevision(),
		Edition:        cfg.Edition,
		DatabaseDriver: cfg.DatabaseDriver,
		AIProvider:     cfg.AIProvider,
		StartedAt:      processStartedAt,
		Repository:     cfg.AgentRepository,
		DefaultBranch:  cfg.AgentRef,
		Secrets: []string{
			cfg.AgentGitHubToken, cfg.AgentCallbackSecret, cfg.EffectiveAIAPIKey(), cfg.JWTSecret, cfg.EncryptionKey,
		},
	})
	service.SetLogSource(logs)
	service.SetSessionIssuer(issuer)
	if err := service.SetGitHub(cfg.AgentGitHubToken, cfg.AgentRepository, cfg.AgentRef); err != nil {
		log.Printf("admin agent GitHub tools disabled: %v", err)
	}
	service.AllowPipelineEdits(cfg.AgentAllowPipelineEdits)
}
