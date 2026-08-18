-- Parse review queue for documents that failed parsing twice (§5.4)
CREATE TABLE IF NOT EXISTS parse_review_items (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    knowledge_id VARCHAR(36) NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    file_name VARCHAR(512) NOT NULL DEFAULT '',
    file_type VARCHAR(32) NOT NULL DEFAULT '',
    file_size BIGINT NOT NULL DEFAULT 0,
    engine_used VARCHAR(64) NOT NULL DEFAULT '',
    fallback_engine VARCHAR(64) NOT NULL DEFAULT '',
    quality_score DOUBLE PRECISION NOT NULL DEFAULT 0,
    garble_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    empty_page_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    table_damage_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    image_loss_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    retry_reason TEXT NOT NULL DEFAULT '',
    doc_type VARCHAR(64) NOT NULL DEFAULT '',
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    resolution VARCHAR(64) NOT NULL DEFAULT '',
    reviewer_id VARCHAR(36) NOT NULL DEFAULT '',
    reviewed_at TIMESTAMP NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP NULL
);

CREATE INDEX IF NOT EXISTS idx_parse_review_tenant_kb ON parse_review_items(tenant_id, knowledge_base_id);
CREATE INDEX IF NOT EXISTS idx_parse_review_status ON parse_review_items(status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_parse_review_knowledge ON parse_review_items(knowledge_id) WHERE deleted_at IS NULL;
