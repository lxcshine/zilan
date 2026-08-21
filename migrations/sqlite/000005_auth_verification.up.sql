-- P0-4 dual-channel registration for the SQLite (Lite) build. Mirrors the
-- Postgres 000082_auth_verification migration with SQLite column
-- affinities (DATETIME). See that migration for the design rationale.
ALTER TABLE users ADD COLUMN phone VARCHAR(20);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_phone ON users(phone);

CREATE TABLE IF NOT EXISTS verification_codes (
    id          VARCHAR(36) PRIMARY KEY,
    channel     VARCHAR(8)   NOT NULL,
    target      VARCHAR(255) NOT NULL,
    purpose     VARCHAR(16)  NOT NULL,
    code_hash   VARCHAR(64)  NOT NULL,
    attempts    INTEGER      NOT NULL DEFAULT 0,
    consumed_at DATETIME,
    expires_at  DATETIME     NOT NULL,
    created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_verification_codes_lookup
    ON verification_codes(channel, target, purpose, created_at);
