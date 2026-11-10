-- Rollback: chunk source locators (SQLite)
ALTER TABLE chunks DROP COLUMN source_locators;
