package adminagent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadCapabilityBoundaries(t *testing.T) {
	now := time.Now().UTC()
	run := AgentRun{ID: "run-one", GitHubRunID: 12345}
	token, err := IssueReadCapability("signing-secret", run, "workspace-one", "accounts", now)
	require.NoError(t, err)
	require.NoError(t, VerifyReadCapability("signing-secret", token, run, "workspace-one", "accounts", now))
	require.Error(t, VerifyReadCapability("wrong-secret", token, run, "workspace-one", "accounts", now))
	require.Error(t, VerifyReadCapability("signing-secret", token, AgentRun{ID: "run-two", GitHubRunID: 12345}, "workspace-one", "accounts", now))
	require.Error(t, VerifyReadCapability("signing-secret", token, AgentRun{ID: "run-one", GitHubRunID: 99999}, "workspace-one", "accounts", now))
	require.Error(t, VerifyReadCapability("signing-secret", token, run, "workspace-two", "accounts", now))
	require.Error(t, VerifyReadCapability("signing-secret", token, run, "workspace-one", "posts", now))
	require.Error(t, VerifyReadCapability("signing-secret", token, run, "workspace-one", "accounts", now.Add(21*time.Minute)))
	_, err = IssueReadCapability("signing-secret", AgentRun{ID: "unregistered"}, "workspace-one", "accounts", now)
	require.Error(t, err)
	_, err = IssueReadCapability("signing-secret", run, "workspace-one", "posts", now)
	require.Error(t, err)
}

func TestReadCapabilityOperationsAreIsolated(t *testing.T) {
	now := time.Now().UTC()
	run := AgentRun{ID: "run-one", GitHubRunID: 12345}
	for _, operation := range []string{"accounts", "posts", "schedules", "analytics", "workflows", "workflow-runs"} {
		capability, err := IssueReadCapability("secret", run, "workspace", operation, now)
		require.NoError(t, err)
		require.NoError(t, VerifyReadCapability("secret", capability, run, "workspace", operation, now))
		for _, other := range []string{"accounts", "posts", "schedules", "analytics", "workflows", "workflow-runs"} {
			if other != operation {
				require.Error(t, VerifyReadCapability("secret", capability, run, "workspace", other, now))
			}
		}
	}
	_, err := IssueReadCapability("secret", run, "workspace", "publish", now)
	require.Error(t, err)
}

func TestWorkflowWriteCapabilityIsIsolated(t *testing.T) {
	now := time.Now()
	run := AgentRun{ID: "run", GitHubRunID: 99}
	token, err := IssueWorkflowWriteCapability("secret", run, "workspace", "workflow-create", now)
	require.NoError(t, err)
	require.NoError(t, VerifyWorkflowWriteCapability("secret", token, run, "workspace", "workflow-create", now))
	require.Error(t, VerifyWorkflowWriteCapability("secret", token, run, "workspace", "workflow-update", now))
	require.Error(t, VerifyReadCapability("secret", token, run, "workspace", "workflows", now))
	require.Error(t, VerifyWorkflowWriteCapability("secret", token, run, "other", "workflow-create", now))
	require.Error(t, VerifyWorkflowWriteCapability("secret", token, run, "workspace", "workflow-create", now.Add(21*time.Minute)))
	_, err = IssueWorkflowWriteCapability("secret", run, "workspace", "publish", now)
	require.Error(t, err)
}

func TestPublicationSchedulingCapabilitiesAreIsolated(t *testing.T) {
	now := time.Now().UTC()
	run := AgentRun{ID: "run", GitHubRunID: 123}
	for _, operation := range []string{"publication-create", "publication-update", "publication-schedule", "publication-reschedule", "publication-publish-now"} {
		token, err := IssuePublicationWriteCapability("secret", run, "workspace", operation, now)
		require.NoError(t, err)
		require.NoError(t, VerifyPublicationWriteCapability("secret", token, run, "workspace", operation, now))
		for _, other := range []string{"publication-create", "publication-update", "publication-schedule", "publication-reschedule", "publication-publish-now"} {
			if other != operation {
				require.Error(t, VerifyPublicationWriteCapability("secret", token, run, "workspace", other, now))
			}
		}
		require.Error(t, VerifyReadCapability("secret", token, run, "workspace", "posts", now))
		require.Error(t, VerifyPublicationWriteCapability("secret", token, run, "different", operation, now))
		require.Error(t, VerifyPublicationWriteCapability("secret", token, run, "workspace", operation, now.Add(21*time.Minute)))
	}
	_, err := IssuePublicationWriteCapability("secret", run, "workspace", "publish", now)
	require.Error(t, err)
}
