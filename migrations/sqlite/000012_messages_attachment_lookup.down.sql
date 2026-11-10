-- Rollback: message attachment lookup for TTL exemption (SQLite)
DROP INDEX IF EXISTS idx_messages_live_attachments;
ALTER TABLE messages DROP COLUMN attachments;
