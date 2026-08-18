-- Add auto-detected corpus class (paper/manual/faq/regulation/general) to
-- knowledge bases. Profiled lazily from chunk-level structural statistics by
-- the ROUTE_RETRIEVAL pipeline stage; drives per-class retrieval presets.
ALTER TABLE knowledge_bases ADD COLUMN IF NOT EXISTS document_class VARCHAR(32) NOT NULL DEFAULT '';
