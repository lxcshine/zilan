-- Remove the auto-detected corpus class column from knowledge bases.
-- SQLite (>= 3.35) supports DROP COLUMN directly.
ALTER TABLE knowledge_bases DROP COLUMN document_class;
