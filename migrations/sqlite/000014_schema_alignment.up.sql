-- 000014_schema_alignment
--
-- 把 SQLite schema 与 Go 模型对齐。
--
-- migrations/sqlite/000000_init.up.sql 是早期全量快照，此后新增的列与表只在
-- versioned（PostgreSQL）迁移里维护，SQLite 侧长期没有同步，导致 Lite 部署在
-- 这些路径上直接报 "no such column / no such table"：
--   - 注册/创建工作区：users.is_system_admin、tenants.api_principal_config
--   - 上传文档：knowledges.pending_subtasks_count
--   - 邀请：tenant_invitations.token / accepted_count
--   - OAuth 客户端绑定主体：mcp_oauth_tokens.principal_type / principal_id
--   - 解析阶段追踪：knowledge_processing_spans
--   - 系统设置实时下发：system_settings
--   - 知识多标签：knowledge_tag_relations
--   - 解析复核队列：parse_review_items
--   - 持久任务队列与死信：task_pending_ops / task_dead_letters
--
-- 所有语句都按 SQLite 方言改写：JSONB → TEXT、BIGSERIAL → INTEGER PRIMARY KEY
-- AUTOINCREMENT、TIMESTAMPTZ → DATETIME。

-- ============ 1) 补齐缺失列 ============
ALTER TABLE users ADD COLUMN is_system_admin BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE tenants ADD COLUMN api_principal_config TEXT;
ALTER TABLE knowledges ADD COLUMN pending_subtasks_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mcp_oauth_tokens ADD COLUMN principal_type VARCHAR(32);
ALTER TABLE mcp_oauth_tokens ADD COLUMN principal_id VARCHAR(512);
ALTER TABLE tenant_invitations ADD COLUMN token VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE tenant_invitations ADD COLUMN accepted_count INTEGER NOT NULL DEFAULT 0;

-- ============ 2) 持久任务队列（task_pending_ops） ============
-- 以 (task_type, scope, scope_id) 为逻辑键的持久待办队列，用来替代会被 TTL
-- 清空的 Redis list。
CREATE TABLE IF NOT EXISTS task_pending_ops (
    id           INTEGER      PRIMARY KEY AUTOINCREMENT,
    tenant_id    INTEGER      NOT NULL,
    task_type    VARCHAR(64)  NOT NULL,
    scope        VARCHAR(32)  NOT NULL,
    scope_id     VARCHAR(64)  NOT NULL,
    op           VARCHAR(32)  NOT NULL,
    dedup_key    VARCHAR(128) NOT NULL DEFAULT '',
    payload      TEXT         NOT NULL DEFAULT '{}',
    fail_count   INTEGER      NOT NULL DEFAULT 0,
    enqueued_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    claimed_at   DATETIME
);

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_scope
    ON task_pending_ops (task_type, scope, scope_id, id);

CREATE INDEX IF NOT EXISTS idx_task_pending_ops_tenant
    ON task_pending_ops (tenant_id);

-- ============ 3) 任务死信（task_dead_letters） ============
CREATE TABLE IF NOT EXISTS task_dead_letters (
    id          INTEGER      PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER      NOT NULL,
    task_type   VARCHAR(64)  NOT NULL,
    scope       VARCHAR(32)  NOT NULL,
    scope_id    VARCHAR(64)  NOT NULL,
    related_id  VARCHAR(64)  NOT NULL DEFAULT '',
    payload     TEXT         NOT NULL,
    last_error  TEXT         NOT NULL DEFAULT '',
    fail_count  INTEGER      NOT NULL,
    failed_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_task_dead_letters_scope
    ON task_dead_letters (scope, scope_id, failed_at DESC);

CREATE INDEX IF NOT EXISTS idx_task_dead_letters_tenant
    ON task_dead_letters (tenant_id, failed_at DESC);

CREATE INDEX IF NOT EXISTS idx_task_dead_letters_task_type
    ON task_dead_letters (task_type, failed_at DESC);

-- ============ 4) 系统设置（system_settings） ============
-- 管理员在「全局设置」里切换的开关（如注册模式）落在这里，并用于多副本实时
-- 下发；Lite 单副本下同样需要，否则设置页读取即失败。
CREATE TABLE IF NOT EXISTS system_settings (
    id               INTEGER      PRIMARY KEY AUTOINCREMENT,
    key              VARCHAR(128) NOT NULL UNIQUE,
    value            TEXT         NOT NULL,
    value_type       VARCHAR(16)  NOT NULL,
    category         VARCHAR(32)  NOT NULL,
    description      TEXT         NOT NULL DEFAULT '',
    is_secret        BOOLEAN      NOT NULL DEFAULT 0,
    requires_restart BOOLEAN      NOT NULL DEFAULT 0,
    last_modified_by VARCHAR(36)  NOT NULL DEFAULT '',
    created_at       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_system_settings_category
    ON system_settings (category);

-- ============ 5) 解析阶段追踪（knowledge_processing_spans） ============
-- 每个解析阶段一行；housekeeping 用它区分「真的卡住」与「某一阶段耗时长但仍在
-- 推进」。缺表时心跳查询会失败，巡检只能退回纯 updated_at 判断。
CREATE TABLE IF NOT EXISTS knowledge_processing_spans (
    id             INTEGER      PRIMARY KEY AUTOINCREMENT,
    knowledge_id   VARCHAR(64)  NOT NULL,
    attempt        INTEGER      NOT NULL DEFAULT 1,
    span_id        VARCHAR(64)  NOT NULL,
    parent_span_id VARCHAR(64),
    name           VARCHAR(64)  NOT NULL,
    kind           VARCHAR(16)  NOT NULL,
    status         VARCHAR(16)  NOT NULL,
    input          TEXT,
    output         TEXT,
    metadata       TEXT,
    error_code     VARCHAR(64),
    error_message  TEXT,
    error_detail   TEXT,
    started_at     DATETIME,
    finished_at    DATETIME,
    duration_ms    INTEGER,
    created_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (knowledge_id, attempt, span_id)
);

CREATE INDEX IF NOT EXISTS idx_kpspan_knowledge_attempt
    ON knowledge_processing_spans (knowledge_id, attempt);

-- ============ 6) 知识多标签（knowledge_tag_relations） ============
CREATE TABLE IF NOT EXISTS knowledge_tag_relations (
    knowledge_id VARCHAR(36) NOT NULL,
    tag_id       VARCHAR(36) NOT NULL,
    created_at   DATETIME    DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (knowledge_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_ktr_knowledge ON knowledge_tag_relations (knowledge_id);
CREATE INDEX IF NOT EXISTS idx_ktr_tag ON knowledge_tag_relations (tag_id);

-- 回填存量单标签数据：老库的 knowledges.tag_id 仍保留着历史值。
INSERT OR IGNORE INTO knowledge_tag_relations (knowledge_id, tag_id, created_at)
SELECT id, tag_id, updated_at
FROM knowledges
WHERE tag_id IS NOT NULL AND tag_id <> '' AND deleted_at IS NULL;

-- ============ 7) 解析复核队列（parse_review_items） ============
CREATE TABLE IF NOT EXISTS parse_review_items (
    id                 VARCHAR(36) PRIMARY KEY,
    tenant_id          INTEGER      NOT NULL,
    knowledge_id       VARCHAR(36)  NOT NULL,
    knowledge_base_id  VARCHAR(36)  NOT NULL,
    file_name          VARCHAR(512) NOT NULL DEFAULT '',
    file_type          VARCHAR(32)  NOT NULL DEFAULT '',
    file_size          INTEGER      NOT NULL DEFAULT 0,
    engine_used        VARCHAR(64)  NOT NULL DEFAULT '',
    fallback_engine    VARCHAR(64)  NOT NULL DEFAULT '',
    quality_score      REAL         NOT NULL DEFAULT 0,
    garble_rate        REAL         NOT NULL DEFAULT 0,
    empty_page_rate    REAL         NOT NULL DEFAULT 0,
    table_damage_rate  REAL         NOT NULL DEFAULT 0,
    image_loss_rate    REAL         NOT NULL DEFAULT 0,
    retry_reason       TEXT         NOT NULL DEFAULT '',
    doc_type           VARCHAR(64)  NOT NULL DEFAULT '',
    status             VARCHAR(32)  NOT NULL DEFAULT 'pending',
    resolution         VARCHAR(64)  NOT NULL DEFAULT '',
    reviewer_id        VARCHAR(36)  NOT NULL DEFAULT '',
    reviewed_at        DATETIME,
    created_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at         DATETIME
);

CREATE INDEX IF NOT EXISTS idx_parse_review_tenant_kb
    ON parse_review_items (tenant_id, knowledge_base_id);

CREATE INDEX IF NOT EXISTS idx_parse_review_status
    ON parse_review_items (status) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_parse_review_knowledge
    ON parse_review_items (knowledge_id) WHERE deleted_at IS NULL;
