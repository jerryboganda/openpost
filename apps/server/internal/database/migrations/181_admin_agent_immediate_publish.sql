ALTER TABLE admin_agent_publication_actions ADD COLUMN job_id TEXT NOT NULL DEFAULT '';
ALTER TABLE admin_agent_publication_actions ADD COLUMN publication_status TEXT NOT NULL DEFAULT '';
ALTER TABLE admin_agent_publication_actions ADD COLUMN job_status TEXT NOT NULL DEFAULT '';
