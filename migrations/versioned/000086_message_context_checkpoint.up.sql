-- FR-10 压缩检查点与增量压缩：messages 记录 Agent 上下文压缩摘要，
-- 覆盖该轮及其之前的全部轮次。下一轮从最新检查点起读历史，避免每轮重算摘要。
-- 空值存 NULL（该轮从未被压缩过，或检查点已因前缀变化/消息删除而失效）。
ALTER TABLE messages ADD COLUMN IF NOT EXISTS context_checkpoint JSONB;

COMMENT ON COLUMN messages.context_checkpoint IS 'Agent compaction summary covering this turn and every turn before it (FR-10)';
