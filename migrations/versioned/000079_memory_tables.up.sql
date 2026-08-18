-- Migration: 000079_memory_tables
-- Three-layer memory foundation (ima-style):
--   * memory_facts             -> L3 long-term memory: fact triples, user
--     profile attributes, preferences, todos/deadlines and answer feedback
--     extracted asynchronously from conversations.
--   * memory_session_summaries -> L2 short-term memory: one rolling summary
--     row per session, recalled with semantic similarity + time decay.
--
-- Embeddings are stored as JSON arrays (jsonb here, TEXT on SQLite) instead of
-- a pgvector column on purpose: the per-user memory set is bounded by design
-- (importance-based eviction), so recall pre-filters by tenant/user/time in SQL
-- and applies score = semantic x time_decay x (1 + ln(1+access_count)) in Go.
-- That keeps one identical code path for Postgres and the SQLite Lite build.

DO $$ BEGIN RAISE NOTICE '[Migration 000079] Creating memory tables...'; END $$;

CREATE TABLE IF NOT EXISTS memory_facts (
    id               VARCHAR(36) PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL,
    user_id          VARCHAR(512) NOT NULL DEFAULT '',
    session_id       VARCHAR(36)  NOT NULL DEFAULT '',
    message_id       VARCHAR(36)  NOT NULL DEFAULT '',
    category         VARCHAR(32)  NOT NULL,
    subject          VARCHAR(255) NOT NULL DEFAULT '',
    predicate        VARCHAR(128) NOT NULL DEFAULT '',
    object           TEXT         NOT NULL DEFAULT '',
    -- Deterministic hash of (category|subject|predicate|object) used for
    -- dedup/upsert. Kept as a column so the unique index works identically
    -- on Postgres and SQLite (SQLite has no md5() for expression indexes).
    triple_hash      VARCHAR(64)  NOT NULL DEFAULT '',
    -- Canonical human-readable rendering of the fact; basis for embeddings
    -- and for prompt injection.
    content          TEXT         NOT NULL,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0.7,
    importance       DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    status           VARCHAR(16)  NOT NULL DEFAULT 'active',
    access_count     INTEGER      NOT NULL DEFAULT 0,
    last_accessed_at TIMESTAMP WITH TIME ZONE,
    due_at           TIMESTAMP WITH TIME ZONE,
    embedding        JSONB,
    created_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       TIMESTAMP WITH TIME ZONE
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
    tenant_id       BIGINT       NOT NULL,
    user_id         VARCHAR(512) NOT NULL DEFAULT '',
    session_id      VARCHAR(36)  NOT NULL,
    title           VARCHAR(512) NOT NULL DEFAULT '',
    summary         TEXT         NOT NULL,
    key_topics      JSONB,
    message_count   INTEGER      NOT NULL DEFAULT 0,
    embedding       JSONB,
    last_message_at TIMESTAMP WITH TIME ZONE,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at      TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_memory_session_summaries_session
    ON memory_session_summaries(tenant_id, session_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_memory_session_summaries_user
    ON memory_session_summaries(tenant_id, user_id, updated_at) WHERE deleted_at IS NULL;

DO $$ BEGIN RAISE NOTICE '[Migration 000079] Done.'; END $$;
