-- FR-12 每轮 Token 用量归因与分层看板：messages 记录本轮的模型用量与五层上下文归因。
-- 对应 Postgres 000087_message_token_usage；SQLite 没有 JSONB，用 TEXT 存 JSON 文本。
-- 两列都只写在 assistant 行上，用户行为 NULL。
ALTER TABLE messages ADD COLUMN usage TEXT;
ALTER TABLE messages ADD COLUMN context_stats TEXT;
