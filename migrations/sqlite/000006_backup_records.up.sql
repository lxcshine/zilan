-- Backup & recovery for the SQLite (Lite) build. Mirrors the Postgres
-- 000083_backup_records migration with SQLite affinities.
CREATE TABLE IF NOT EXISTS backup_records (
    id            VARCHAR(64) PRIMARY KEY,
    trigger_type  VARCHAR(16) NOT NULL,
    status        VARCHAR(16) NOT NULL,
    started_at    DATETIME NOT NULL,
    finished_at   DATETIME,
    base_path     VARCHAR(512) NOT NULL,
    stats         TEXT NOT NULL DEFAULT '{}',
    error         TEXT,
    retention_tag VARCHAR(8),
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_backup_records_status ON backup_records (status);
CREATE INDEX IF NOT EXISTS idx_backup_records_retention ON backup_records (retention_tag);

CREATE TABLE IF NOT EXISTS backup_restore_jobs (
    id            VARCHAR(80) PRIMARY KEY,
    backup_id     VARCHAR(64) NOT NULL,
    scope         VARCHAR(16) NOT NULL,
    tenant_id     INTEGER,
    conflict_mode VARCHAR(16),
    status        VARCHAR(16) NOT NULL,
    progress      TEXT NOT NULL DEFAULT '{}',
    created_by    VARCHAR(64) NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    report        TEXT,
    finished_at   DATETIME
);

CREATE INDEX IF NOT EXISTS idx_backup_restore_jobs_backup ON backup_restore_jobs (backup_id);
CREATE INDEX IF NOT EXISTS idx_backup_restore_jobs_status ON backup_restore_jobs (status);
