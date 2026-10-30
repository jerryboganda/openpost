ALTER TABLE admin_agent_runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
ALTER TABLE admin_agent_runs ADD COLUMN effort TEXT NOT NULL DEFAULT '';
ALTER TABLE admin_agent_runs ADD COLUMN access TEXT NOT NULL DEFAULT 'full';
ALTER TABLE admin_agent_runs ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE admin_agent_runs ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;

CREATE TABLE admin_agent_attachments (
  id TEXT PRIMARY KEY,
  conversation_id TEXT REFERENCES admin_agent_conversations(id) ON DELETE CASCADE,
  message_id TEXT REFERENCES admin_agent_messages(id) ON DELETE CASCADE,
  uploaded_by_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  filename TEXT NOT NULL DEFAULT '',
  content_type TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  width INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL,
  data BLOB NOT NULL,
  created_at DATETIME NOT NULL
);

CREATE INDEX idx_admin_agent_attachments_message
  ON admin_agent_attachments (message_id);
CREATE INDEX idx_admin_agent_attachments_created
  ON admin_agent_attachments (created_at);
