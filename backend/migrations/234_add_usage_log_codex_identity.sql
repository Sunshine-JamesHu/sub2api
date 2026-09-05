-- Persist the client-provided Codex thread and context-window identifiers.
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS thread_id VARCHAR(255);
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS window_id VARCHAR(255);

CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created_identity
    ON usage_logs (account_id, created_at, session_id, thread_id, window_id);
