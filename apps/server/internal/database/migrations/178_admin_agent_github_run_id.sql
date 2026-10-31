ALTER TABLE admin_agent_runs ADD COLUMN github_run_id BIGINT NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_admin_agent_runs_github_run_id
  ON admin_agent_runs (github_run_id) WHERE github_run_id > 0;
