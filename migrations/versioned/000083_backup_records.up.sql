-- Backup & recovery (PRD docs/prd/data-backup-recovery.md §6.1)
CREATE TABLE IF NOT EXISTS backup_records (
    id            VARCHAR(64) PRIMARY KEY,
    trigger_type  VARCHAR(16) NOT NULL,
    status        VARCHAR(16) NOT NULL,
    started_at    TIMESTAMP NOT NULL,
    finished_at   TIMESTAMP NULL,
    base_path     VARCHAR(512) NOT NULL,
    stats         JSONB NOT NULL DEFAULT '{}',
    error         TEXT NULL,
    retention_tag VARCHAR(8) NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_backup_records_status ON backup_records (status);
CREATE INDEX IF NOT EXISTS idx_backup_records_retention ON backup_records (retention_tag);

CREATE TABLE IF NOT EXISTS backup_restore_jobs (
    id            VARCHAR(80) PRIMARY KEY,
    backup_id     VARCHAR(64) NOT NULL,
    scope         VARCHAR(16) NOT NULL,
    tenant_id     BIGINT NULL,
    conflict_mode VARCHAR(16) NULL,
    status        VARCHAR(16) NOT NULL,
    progress      JSONB NOT NULL DEFAULT '{}',
    created_by    VARCHAR(64) NOT NULL,
    created_at    TIMESTAMP NOT NULL DEFAULT NOW(),
    report        JSONB NULL,
    finished_at   TIMESTAMP NULL
);

CREATE INDEX IF NOT EXISTS idx_backup_restore_jobs_backup ON backup_restore_jobs (backup_id);
CREATE INDEX IF NOT EXISTS idx_backup_restore_jobs_status ON backup_restore_jobs (status);
