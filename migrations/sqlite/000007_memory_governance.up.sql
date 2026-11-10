-- 记忆治理（FR-01~FR-04）的 SQLite (Lite) 版本。镜像 Postgres
-- 000084_memory_governance 迁移，改用 SQLite 列亲和性（DATETIME/TEXT/INTEGER）。
-- 设计理由见该迁移的注释。
ALTER TABLE memory_facts ADD COLUMN origin         VARCHAR(16) NOT NULL DEFAULT 'extracted';
ALTER TABLE memory_facts ADD COLUMN normalized_key VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN replaces_id    VARCHAR(36) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN superseded_by  VARCHAR(36) NOT NULL DEFAULT '';
ALTER TABLE memory_facts ADD COLUMN superseded_at  DATETIME;

CREATE INDEX IF NOT EXISTS idx_memory_facts_status
    ON memory_facts(tenant_id, user_id, status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_facts_conflict_key
    ON memory_facts(tenant_id, user_id, normalized_key)
    WHERE deleted_at IS NULL AND normalized_key <> '';

CREATE TABLE IF NOT EXISTS memory_governance (
    tenant_id            INTEGER      NOT NULL,
    user_id              VARCHAR(512) NOT NULL,
    generation           VARCHAR(36)  NOT NULL DEFAULT '',
    cutoff_nano          INTEGER      NOT NULL DEFAULT 0,
    blocked_writes       INTEGER      NOT NULL DEFAULT 0,
    archive_failures     INTEGER      NOT NULL DEFAULT 0,
    extraction_paused_at DATETIME,
    created_at           DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at           DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE IF NOT EXISTS memory_governance_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       INTEGER      NOT NULL,
    user_id         VARCHAR(512) NOT NULL,
    kind            VARCHAR(32)  NOT NULL,
    reason          TEXT         NOT NULL DEFAULT '',
    item_count      INTEGER      NOT NULL DEFAULT 0,
    item_ids        TEXT,
    item_contents   TEXT,
    acknowledged_at DATETIME,
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_memory_governance_events_pending
    ON memory_governance_events(tenant_id, user_id, id DESC)
    WHERE acknowledged_at IS NULL;

ALTER TABLE tenants ADD COLUMN memory_config TEXT;
