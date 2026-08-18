-- GraphRAG community summaries for the SQLite (Lite) build. Mirrors the
-- Postgres 000080_graph_communities migration with SQLite column affinities
-- (DATETIME/TEXT/INTEGER). See that migration for the design rationale.
CREATE TABLE IF NOT EXISTS graph_communities (
    id                 VARCHAR(36) PRIMARY KEY,
    tenant_id          INTEGER      NOT NULL,
    knowledge_base_id  VARCHAR(36)  NOT NULL,
    community_key      VARCHAR(64)  NOT NULL DEFAULT '',
    title              VARCHAR(512) NOT NULL DEFAULT '',
    summary            TEXT         NOT NULL,
    node_names         TEXT,
    node_count         INTEGER      NOT NULL DEFAULT 0,
    rel_count          INTEGER      NOT NULL DEFAULT 0,
    summary_model_id   VARCHAR(64)  NOT NULL DEFAULT '',
    embedding_model_id VARCHAR(64)  NOT NULL DEFAULT '',
    embedding          TEXT,
    created_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at         DATETIME
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_graph_communities_key
    ON graph_communities(tenant_id, knowledge_base_id, community_key)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_graph_communities_kb
    ON graph_communities(tenant_id, knowledge_base_id) WHERE deleted_at IS NULL;
