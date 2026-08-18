-- Migration: 000080_graph_communities
-- GraphRAG community summaries: per-KB entity communities detected on the
-- Neo4j graph (label propagation) and summarized by an LLM. Recalled at chat
-- time by embedding similarity and injected as graph-context search results,
-- complementing chunk-level vector retrieval.
--
-- Embeddings are stored as JSON arrays (jsonb here, TEXT on SQLite) instead
-- of a pgvector column on purpose: the per-KB community set is bounded by
-- design (<= 32 rows), so recall pre-filters by tenant/KB in SQL and scores
-- cosine similarity in Go — one identical code path for Postgres and the
-- SQLite Lite build (same rationale as 000079_memory_tables).

DO $$ BEGIN RAISE NOTICE '[Migration 000080] Creating graph_communities table...'; END $$;

CREATE TABLE IF NOT EXISTS graph_communities (
    id                 VARCHAR(36) PRIMARY KEY,
    tenant_id          BIGINT       NOT NULL,
    knowledge_base_id  VARCHAR(36)  NOT NULL,
    -- Deterministic hash of (knowledge_base_id | sorted member names); a
    -- rebuild re-detecting the same member set updates this row in place.
    community_key      VARCHAR(64)  NOT NULL DEFAULT '',
    title              VARCHAR(512) NOT NULL DEFAULT '',
    summary            TEXT         NOT NULL,
    node_names         JSONB,
    node_count         INTEGER      NOT NULL DEFAULT 0,
    rel_count          INTEGER      NOT NULL DEFAULT 0,
    summary_model_id   VARCHAR(64)  NOT NULL DEFAULT '',
    embedding_model_id VARCHAR(64)  NOT NULL DEFAULT '',
    embedding          JSONB,
    created_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at         TIMESTAMP WITH TIME ZONE
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_graph_communities_key
    ON graph_communities(tenant_id, knowledge_base_id, community_key)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_graph_communities_kb
    ON graph_communities(tenant_id, knowledge_base_id) WHERE deleted_at IS NULL;

DO $$ BEGIN RAISE NOTICE '[Migration 000080] Done.'; END $$;
