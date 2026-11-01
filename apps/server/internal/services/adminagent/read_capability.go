package adminagent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const readCapabilityLifetime = 20 * time.Minute

func ReadOperationAllowed(operation string) bool {
	switch operation {
	case "accounts", "posts", "schedules", "analytics", "workflows", "workflow-runs":
		return true
	default:
		return false
	}
}

type readCapabilityClaims struct {
	RunID       string `json:"run_id"`
	GitHubRunID int64  `json:"github_run_id"`
	WorkspaceID string `json:"workspace_id"`
	Operation   string `json:"operation"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresAt   int64  `json:"expires_at"`
}

// IssueReadCapability creates a short-lived, operation-specific bearer credential.
// The HMAC key never leaves the server or trusted Actions jobs.
func IssueReadCapability(secret string, run AgentRun, workspaceID, operation string, now time.Time) (string, error) {
	if secret == "" || run.ID == "" || run.GitHubRunID <= 0 || workspaceID == "" || !ReadOperationAllowed(operation) {
		return "", ErrInvalidCallback
	}
	claims := readCapabilityClaims{
		RunID: run.ID, GitHubRunID: run.GitHubRunID, WorkspaceID: workspaceID,
		Operation: operation, IssuedAt: now.Unix(), ExpiresAt: now.Add(readCapabilityLifetime).Unix(),
	}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	return encoded + "." + signReadCapability(secret, encoded), nil
}

func VerifyReadCapability(secret, token string, run AgentRun, workspaceID, operation string, now time.Time) error {
	if secret == "" || len(token) > 1024 || run.GitHubRunID <= 0 {
		return ErrInvalidCallback
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(signReadCapability(secret, encoded))) {
		return ErrInvalidCallback
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ErrInvalidCallback
	}
	var claims readCapabilityClaims
	if json.Unmarshal(data, &claims) != nil || claims.RunID != run.ID ||
		claims.GitHubRunID != run.GitHubRunID || claims.WorkspaceID != workspaceID ||
		claims.Operation != operation || !ReadOperationAllowed(claims.Operation) ||
		claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() ||
		claims.ExpiresAt-claims.IssuedAt != int64(readCapabilityLifetime/time.Second) {
		return ErrInvalidCallback
	}
	return nil
}

func signReadCapability(secret, encoded string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("openpost-agent-read-v1."))
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Write capabilities use a distinct signing domain and cannot authorize reads.
func IssueWorkflowWriteCapability(secret string, run AgentRun, workspaceID, operation string, now time.Time) (string, error) {
	if operation != "workflow-create" && operation != "workflow-update" {
		return "", ErrInvalidCallback
	}
	if secret == "" || run.ID == "" || run.GitHubRunID <= 0 || workspaceID == "" {
		return "", ErrInvalidCallback
	}
	claims := readCapabilityClaims{RunID: run.ID, GitHubRunID: run.GitHubRunID, WorkspaceID: workspaceID, Operation: operation, IssuedAt: now.Unix(), ExpiresAt: now.Add(readCapabilityLifetime).Unix()}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	return encoded + "." + signWorkflowWriteCapability(secret, encoded), nil
}

func VerifyWorkflowWriteCapability(secret, token string, run AgentRun, workspaceID, operation string, now time.Time) error {
	if secret == "" || len(token) > 1024 || run.GitHubRunID <= 0 || (operation != "workflow-create" && operation != "workflow-update") {
		return ErrInvalidCallback
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(signWorkflowWriteCapability(secret, encoded))) {
		return ErrInvalidCallback
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ErrInvalidCallback
	}
	var claims readCapabilityClaims
	if json.Unmarshal(data, &claims) != nil || claims.RunID != run.ID || claims.GitHubRunID != run.GitHubRunID || claims.WorkspaceID != workspaceID || claims.Operation != operation || claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt-claims.IssuedAt != int64(readCapabilityLifetime/time.Second) {
		return ErrInvalidCallback
	}
	return nil
}

func signWorkflowWriteCapability(secret, encoded string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("openpost-agent-workflow-write-v1."))
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func IssuePublicationWriteCapability(secret string, run AgentRun, workspaceID, operation string, now time.Time) (string, error) {
	if !publicationWriteOperationAllowed(operation) {
		return "", ErrInvalidCallback
	}
	if secret == "" || run.ID == "" || run.GitHubRunID <= 0 || workspaceID == "" {
		return "", ErrInvalidCallback
	}
	claims := readCapabilityClaims{RunID: run.ID, GitHubRunID: run.GitHubRunID, WorkspaceID: workspaceID, Operation: operation, IssuedAt: now.Unix(), ExpiresAt: now.Add(readCapabilityLifetime).Unix()}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	return encoded + "." + signPublicationWriteCapability(secret, encoded), nil
}

func VerifyPublicationWriteCapability(secret, token string, run AgentRun, workspaceID, operation string, now time.Time) error {
	if secret == "" || len(token) > 1024 || run.GitHubRunID <= 0 || !publicationWriteOperationAllowed(operation) {
		return ErrInvalidCallback
	}
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(signPublicationWriteCapability(secret, encoded))) {
		return ErrInvalidCallback
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ErrInvalidCallback
	}
	var claims readCapabilityClaims
	if json.Unmarshal(data, &claims) != nil || claims.RunID != run.ID || claims.GitHubRunID != run.GitHubRunID || claims.WorkspaceID != workspaceID || claims.Operation != operation || claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt-claims.IssuedAt != int64(readCapabilityLifetime/time.Second) {
		return ErrInvalidCallback
	}
	return nil
}

func publicationWriteOperationAllowed(operation string) bool {
	switch operation {
	case "publication-create", "publication-update", "publication-schedule", "publication-reschedule", "publication-publish-now":
		return true
	}
	return false
}

func signPublicationWriteCapability(secret, encoded string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("openpost-agent-publication-write-v1."))
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
