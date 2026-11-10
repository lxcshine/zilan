-- Migration: 000084_memory_governance (rollback)
DO $$ BEGIN RAISE NOTICE '[Migration 000084] Rolling back memory governance...'; END $$;

ALTER TABLE tenants DROP COLUMN IF EXISTS memory_config;

DROP TABLE IF EXISTS memory_governance_events;
DROP TABLE IF EXISTS memory_governance;

DROP INDEX IF EXISTS idx_memory_facts_conflict_key;
DROP INDEX IF EXISTS idx_memory_facts_status;

ALTER TABLE memory_facts DROP COLUMN IF EXISTS superseded_at;
ALTER TABLE memory_facts DROP COLUMN IF EXISTS superseded_by;
ALTER TABLE memory_facts DROP COLUMN IF EXISTS replaces_id;
ALTER TABLE memory_facts DROP COLUMN IF EXISTS normalized_key;
ALTER TABLE memory_facts DROP COLUMN IF EXISTS origin;

DO $$ BEGIN RAISE NOTICE '[Migration 000084] Rollback done.'; END $$;
