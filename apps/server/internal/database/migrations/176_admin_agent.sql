CREATE TABLE admin_agent_conversations (
  id TEXT PRIMARY KEY,
  engine TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  created_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL,
  created_at DATETIME NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE INDEX idx_admin_agent_conversations_updated
  ON admin_agent_conversations (updated_at DESC);

CREATE TABLE admin_agent_messages (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES admin_agent_conversations(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  run_id TEXT,
  created_at DATETIME NOT NULL
);

CREATE INDEX idx_admin_agent_messages_conversation
  ON admin_agent_messages (conversation_id, created_at);

CREATE TABLE admin_agent_runs (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES admin_agent_conversations(id) ON DELETE CASCADE,
  engine TEXT NOT NULL,
  status TEXT NOT NULL,
  prompt TEXT NOT NULL,
  base_revision TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  dispatch_ref TEXT NOT NULL DEFAULT '',
  workflow_url TEXT NOT NULL DEFAULT '',
  failure_code TEXT NOT NULL DEFAULT '',
  failure_message TEXT NOT NULL DEFAULT '',
  summary TEXT NOT NULL DEFAULT '',
  created_at DATETIME NOT NULL,
  started_at DATETIME,
  finished_at DATETIME
);

CREATE INDEX idx_admin_agent_runs_conversation
  ON admin_agent_runs (conversation_id, created_at);
CREATE INDEX idx_admin_agent_runs_status
  ON admin_agent_runs (status);

CREATE TABLE admin_agent_events (
  id TEXT PRIMARY KEY,
  run_id TEXT NOT NULL REFERENCES admin_agent_runs(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  event_key TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  message TEXT NOT NULL,
  data_json TEXT NOT NULL DEFAULT '{}',
  created_at DATETIME NOT NULL,
  UNIQUE (run_id, event_key)
);

CREATE INDEX idx_admin_agent_events_run
  ON admin_agent_events (run_id, seq);
