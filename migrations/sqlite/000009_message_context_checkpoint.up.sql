-- FR-10 压缩检查点与增量压缩：messages 记录 Agent 上下文压缩摘要，
-- 覆盖该轮及其之前的全部轮次。下一轮从最新检查点起读历史，避免每轮重算摘要。
-- 对应 Postgres 000086_message_context_checkpoint；空值存 NULL。
ALTER TABLE messages ADD COLUMN context_checkpoint TEXT;
