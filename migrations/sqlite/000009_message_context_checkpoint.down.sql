-- Rollback: message context checkpoint (SQLite)
ALTER TABLE messages DROP COLUMN context_checkpoint;
