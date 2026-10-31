CREATE TABLE IF NOT EXISTS admin_agent_workflow_actions (
    run_id TEXT NOT NULL,
    action_key TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending',
    workflow_id TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 0,
    outcome TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, action_key),
    FOREIGN KEY (run_id) REFERENCES admin_agent_runs(id)
);
