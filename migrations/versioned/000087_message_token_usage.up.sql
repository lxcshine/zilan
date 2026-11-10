-- FR-12 每轮 Token 用量归因与分层看板：messages 记录本轮的模型用量与五层上下文归因。
--   usage         — 输入/输出/总 token 与供应商侧的 prompt 缓存计量（FR-11）
--   context_stats — L0–L4 各层的实际占用与预算、弹性调整动作、压缩/截断事件
-- 两列都只写在 assistant 行上：用户消息不消耗模型用量，存 NULL。
-- 分层占用一律来自 internal/contextx 的精确 tokenizer，不含任何字符估算口径。
ALTER TABLE messages ADD COLUMN IF NOT EXISTS usage JSONB;
ALTER TABLE messages ADD COLUMN IF NOT EXISTS context_stats JSONB;

COMMENT ON COLUMN messages.usage IS 'Per-turn model token usage: prompt/completion/total plus prompt-cache counters (FR-12)';
COMMENT ON COLUMN messages.context_stats IS 'Per-turn five-layer (L0-L4) context attribution and compaction/truncation events (FR-12)';
