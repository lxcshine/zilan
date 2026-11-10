-- 回滚记忆治理（SQLite）。
ALTER TABLE tenants DROP COLUMN memory_config;

DROP TABLE IF EXISTS memory_governance_events;
DROP TABLE IF EXISTS memory_governance;

DROP INDEX IF EXISTS idx_memory_facts_conflict_key;
DROP INDEX IF EXISTS idx_memory_facts_status;

ALTER TABLE memory_facts DROP COLUMN superseded_at;
ALTER TABLE memory_facts DROP COLUMN superseded_by;
ALTER TABLE memory_facts DROP COLUMN replaces_id;
ALTER TABLE memory_facts DROP COLUMN normalized_key;
ALTER TABLE memory_facts DROP COLUMN origin;
