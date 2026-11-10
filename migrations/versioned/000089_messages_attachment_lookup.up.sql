-- FR-22 按需会话附件（规则 ⑥）：被活跃会话（含其分叉）引用的附件不受 TTL 清理影响。
--
-- 清理查询要对每个过期候选做一次 messages.attachments 的 JSONB 包含判断：
--   NOT EXISTS (SELECT 1 FROM messages m JOIN sessions s ON s.id = m.session_id
--               WHERE m.role = 'user' AND s.tenant_id = temporary_documents.tenant_id
--                 AND s.deleted_at IS NULL AND m.deleted_at IS NULL
--                 AND m.attachments @> jsonb_build_array(jsonb_build_object('id', temporary_documents.id)))
-- 没有索引时这会退化成对 messages 的全表扫描，且清理是循环分批跑的，代价会被放大。
--
-- 只索引存活的用户消息，让每个候选都能走 JSONB 包含而不必扫遍所有消息的 attachments。
-- ⚠️ 部分索引的谓词必须与查询里针对 messages 的谓词逐字一致（role = 'user' AND deleted_at IS NULL），
--    否则规划器不会选用它，静默退化成全表扫。sessions.deleted_at / s.tenant_id 属于另一张表，
--    不能出现在本索引谓词里。
--
-- 注意：这里没有用 CREATE INDEX CONCURRENTLY。Zilan 的迁移执行器会把每个迁移文件包在事务里
-- （见 000036 的说明），CONCURRENTLY 无法在事务中运行，硬写会让迁移失败并把 schema_migrations
-- 置为 dirty。messages 表很大时，请由运维在应用本迁移之前先用 CONCURRENTLY 手工建同名索引，
-- 本语句因为 IF NOT EXISTS 会变成空操作。
CREATE INDEX IF NOT EXISTS idx_messages_live_attachments
    ON messages USING gin (attachments jsonb_path_ops)
    WHERE role = 'user' AND deleted_at IS NULL;
