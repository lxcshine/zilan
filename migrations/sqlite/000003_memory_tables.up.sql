-- Three-layer memory foundation for the SQLite (Lite) build. Mirrors the
-- Postgres 000079_memory_tables migration with SQLite column affinities
-- (DATETIME/REAL/TEXT). See that migration for the design rationale.
CREATE TABLE IF NOT EXISTS memory_facts (
    id               VARCHAR(36) PRIMARY KEY,
    tenant_id        INTEGER     NOT NULL,
    user_id          VARCHAR(512) NOT NULL DEFAULT '',
    session_id       VARCHAR(36) NOT NULL DEFAULT '',
    message_id       VARCHAR(36) NOT NULL DEFAULT '',
    category         VARCHAR(32) NOT NULL,
    subject          VARCHAR(255) NOT NULL DEFAULT '',
    predicate        VARCHAR(128) NOT NULL DEFAULT '',
    object           TEXT        NOT NULL DEFAULT '',
    triple_hash      VARCHAR(64) NOT NULL DEFAULT '',
    content          TEXT        NOT NULL,
    confidence       REAL        NOT NULL DEFAULT 0.7,
    importance       REAL        NOT NULL DEFAULT 0.5,
    status           VARCHAR(16) NOT NULL DEFAULT 'active',
    access_count     INTEGER     NOT NULL DEFAULT 0,
    last_accessed_at DATETIME,
    due_at           DATETIME,
    embedding        TEXT,
    created_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       DATETIME
);

CREATE INDEX IF NOT EXISTS idx_memory_facts_user
    ON memory_facts(tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_facts_category
    ON memory_facts(tenant_id, user_id, category) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_facts_session
    ON memory_facts(session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_facts_due
    ON memory_facts(tenant_id, user_id, due_at)
    WHERE deleted_at IS NULL AND due_at IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_facts_triple
    ON memory_facts(tenant_id, user_id, triple_hash)
    WHERE deleted_at IS NULL AND triple_hash <> '';

CREATE TABLE IF NOT EXISTS memory_session_summaries (
    id              VARCHAR(36) PRIMARY KEY,
    tenant_id       INTEGER      NOT NULL,
    user_id         VARCHAR(512) NOT NULL DEFAULT '',
    session_id      VARCHAR(36)  NOT NULL,
    title           VARCHAR(512) NOT NULL DEFAULT '',
    summary         TEXT         NOT NULL,
    key_topics      TEXT,
    message_count   INTEGER      NOT NULL DEFAULT 0,
    embedding       TEXT,
    last_message_at DATETIME,
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at      DATETIME
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_session_summaries_session
    ON memory_session_summaries(tenant_id, session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_session_summaries_user
    ON memory_session_summaries(tenant_id, user_id, updated_at) WHERE deleted_at IS NULL;
