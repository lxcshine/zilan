-- Rollback: message token usage attribution (SQLite)
ALTER TABLE messages DROP COLUMN context_stats;
ALTER TABLE messages DROP COLUMN usage;
