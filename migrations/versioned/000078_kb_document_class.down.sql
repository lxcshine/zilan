-- Remove the auto-detected corpus class column from knowledge bases
ALTER TABLE knowledge_bases DROP COLUMN IF EXISTS document_class;
