package handlers

import (
    "testing"

    "github.com/openpost/backend/internal/services/workflows"
    "github.com/stretchr/testify/require"
)

func TestWorkflowWriteRequiresExplicitIntent(t *testing.T) {
    for _, tc := range []struct { prompt, operation string; allowed bool }{
        {"Create a workflow that generates AI text", "workflow-create", true},
        {"Please edit the workflow to use an AI decision", "workflow-update", true},
        {"Create a workflow", "workflow-update", false},
        {"List workflows", "workflow-create", false},
        {"How do I create a workflow?", "workflow-create", false},
        {"Don't create a workflow", "workflow-create", false},
        {"Do not update the workflow", "workflow-update", false},
        {"Publish the workflow", "workflow-update", false},
    } {
        require.Equal(t, tc.allowed, explicitWorkflowIntent(tc.prompt, tc.operation), tc.prompt)
    }
}

func TestAgentWorkflowDraftStepsRejectExecutableTools(t *testing.T) {
    require.True(t, agentAllowedWorkflowSteps([]workflows.Step{{ID: "a", Kind: workflows.KindAIText}, {ID: "b", Kind: workflows.KindAIDecision, Then: []workflows.Step{{ID: "c", Kind: workflows.KindText}}}}))
    require.False(t, agentAllowedWorkflowSteps([]workflows.Step{{ID: "a", Kind: workflows.KindHTTP}}))
    require.False(t, agentAllowedWorkflowSteps([]workflows.Step{{ID: "a", Kind: workflows.KindAIDecision, Else: []workflows.Step{{ID: "b", Kind: workflows.KindCode}}}}))
}
