CREATE UNIQUE INDEX idx_admin_agent_runs_active_conversation
  ON admin_agent_runs (conversation_id)
  WHERE status IN ('queued', 'dispatched', 'running');
