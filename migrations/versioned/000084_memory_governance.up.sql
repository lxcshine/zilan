-- Migration: 000084_memory_governance
-- 记忆治理四件套（PRD §3.2 状态机 + FR-01/FR-02/FR-03/FR-04）：
--   * memory_facts 新增 origin / normalized_key / replaces_id / superseded_by /
--     superseded_at —— 支撑 pending 确认门控与取代链。
--   * memory_governance         -> 每个 (租户, 用户) 一行的治理状态：清空时序
--     围栏（世代令牌 + 单调截止点 + 拦截计数）与归档连续失败计数。
--   * memory_governance_events  -> 自动归档等治理事件，记忆管理页做一次性提示。
--   * tenants.memory_config     -> 工作区级写入模式与容量上限（FR-04 规则 1）。
--
-- 存量数据兼容（PRD §5.4）：本迁移不自动改写存量记忆的状态。origin 由列默认值
-- 回填为 extracted（存量记忆确实来自后台抽取），status 保持原值，因此升级前
-- active 的记忆升级后仍然 active，不会被追溯性地降级为待确认。
-- normalized_key 不做 SQL 回填：矛盾判定在服务层按 category 候选集在 Go 侧计算
-- （Postgres 与 SQLite 走同一条代码路径，避免两套方言的哈希表达式漂移），
-- 存量行在下一次写入时补齐该列。

DO $$ BEGIN RAISE NOTICE '[Migration 000084] Adding memory governance...'; END $$;

ALTER TABLE memory_facts ADD COLUMN IF NOT EXISTS origin         VARCHAR(16) NOT NULL DEFAULT 'extracted';
ALTER TABLE memory_facts ADD COLUMN IF NOT EXISTS normalized_key VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN IF NOT EXISTS replaces_id    VARCHAR(36) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN IF NOT EXISTS superseded_by  VARCHAR(36) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN IF NOT EXISTS superseded_at  TIMESTAMP WITH TIME ZONE;

-- 状态过滤（待确认角标 / 已归档分组 / 取代链）与矛盾判定键查询。
-- status 列宽 VARCHAR(16) 已足够容纳 'superseded'。
CREATE INDEX IF NOT EXISTS idx_memory_facts_status
    ON memory_facts(tenant_id, user_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_facts_conflict_key
    ON memory_facts(tenant_id, user_id, normalized_key)
    WHERE deleted_at IS NULL AND normalized_key <> '';

-- 清空时序围栏（FR-01）。截止点用 Unix 纳秒整数存储（cutoff_nano），比较全部在
-- Go 侧完成：整数既不受调用方时区影响，也不受数据库时间列精度（SQLite 的
-- DATETIME 可能截断亚秒）影响，这正是"单调不回退 + 跨时区收敛到同一序列"
-- 两个要求的落地方式。
CREATE TABLE IF NOT EXISTS memory_governance (
    tenant_id            BIGINT       NOT NULL,
    user_id              VARCHAR(512) NOT NULL,
    generation           VARCHAR(36)  NOT NULL DEFAULT '',
    cutoff_nano          BIGINT       NOT NULL DEFAULT 0,
    blocked_writes       BIGINT       NOT NULL DEFAULT 0,
    archive_failures     INTEGER      NOT NULL DEFAULT 0,
    extraction_paused_at TIMESTAMP WITH TIME ZONE,
    created_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, user_id)
);

-- 治理事件（FR-04 规则 5 一次性提示 / 异常通知）。
CREATE TABLE IF NOT EXISTS memory_governance_events (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL,
    user_id        VARCHAR(512) NOT NULL,
    kind           VARCHAR(32)  NOT NULL,
    reason         TEXT         NOT NULL DEFAULT '',
    item_count     INTEGER      NOT NULL DEFAULT 0,
    item_ids       JSONB,
    item_contents JSONB,
    acknowledged_at TIMESTAMP WITH TIME ZONE,
    created_at     TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_memory_governance_events_pending
    ON memory_governance_events(tenant_id, user_id, id DESC)
    WHERE acknowledged_at IS NULL;

-- 工作区级记忆配置（FR-02 规则 7 / FR-04 规则 1）。
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS memory_config JSONB;

DO $$ BEGIN RAISE NOTICE '[Migration 000084] Done.'; END $$;
