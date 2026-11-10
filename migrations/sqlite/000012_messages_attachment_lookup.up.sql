-- FR-22 按需会话附件（规则 ⑥）：被活跃会话（含其分叉）引用的附件不受 TTL 清理影响。
-- 对应 Postgres 000089_messages_attachment_lookup。
--
-- 清理查询在 SQLite 上把存活引用一次性物化出来（json_each 展开 messages.attachments），
-- 再用标量键 tenant_id || ':' || id 做 NOT IN 判断：SQLite 无法为 JSON 数组成员建索引。
--
-- 顺带补齐 messages.attachments 列：SQLite 初始 schema（000000_init）漏了这一列，
-- 而 GORM 的 types.Message 一直带着它。缺列时带附件的消息写入与上面的豁免查询都会直接失败。
ALTER TABLE messages ADD COLUMN attachments TEXT NOT NULL DEFAULT '[]';

-- 豁免子查询按 session_id 关联会话、按 role 过滤用户消息，
-- 用 (session_id, role) 复合索引把这次展开限制在存活的用户消息上。
CREATE INDEX IF NOT EXISTS idx_messages_live_attachments
    ON messages (session_id, role);
