-- 回滚 000014_schema_alignment：删除新增的表与列。
-- SQLite 3.35+ 支持 DROP COLUMN；本仓库的 SQLite 由 mattn/go-sqlite3 内置，
-- 版本远高于该门槛。

DROP INDEX IF EXISTS idx_parse_review_knowledge;
DROP INDEX IF EXISTS idx_parse_review_status;
DROP INDEX IF EXISTS idx_parse_review_tenant_kb;
DROP TABLE IF EXISTS parse_review_items;

DROP INDEX IF EXISTS idx_ktr_tag;
DROP INDEX IF EXISTS idx_ktr_knowledge;
DROP TABLE IF EXISTS knowledge_tag_relations;

DROP INDEX IF EXISTS idx_kpspan_knowledge_attempt;
DROP TABLE IF EXISTS knowledge_processing_spans;

DROP INDEX IF EXISTS idx_system_settings_category;
DROP TABLE IF EXISTS system_settings;

DROP INDEX IF EXISTS idx_task_dead_letters_task_type;
DROP INDEX IF EXISTS idx_task_dead_letters_tenant;
DROP INDEX IF EXISTS idx_task_dead_letters_scope;
DROP TABLE IF EXISTS task_dead_letters;

DROP INDEX IF EXISTS idx_task_pending_ops_tenant;
DROP INDEX IF EXISTS idx_task_pending_ops_scope;
DROP TABLE IF EXISTS task_pending_ops;

ALTER TABLE tenant_invitations DROP COLUMN accepted_count;
ALTER TABLE tenant_invitations DROP COLUMN token;
ALTER TABLE mcp_oauth_tokens DROP COLUMN principal_id;
ALTER TABLE mcp_oauth_tokens DROP COLUMN principal_type;
ALTER TABLE knowledges DROP COLUMN pending_subtasks_count;
ALTER TABLE tenants DROP COLUMN api_principal_config;
ALTER TABLE users DROP COLUMN is_system_admin;
