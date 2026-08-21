-- Migration: 000082_auth_verification
-- P0-4 dual-channel registration (docs/prd/auth-dual-channel-verification.md §4):
--   * users.phone                -> mobile-number identity column for
--     phone-registered accounts. Nullable; the unique index tolerates
--     multiple NULLs on both PostgreSQL and SQLite, so email-registered
--     users are unaffected.
--   * verification_codes         -> ownership-proof codes sent over SMS or
--     email. Only SHA-256(code) is stored; rows are TTL-bounded,
--     attempt-capped and single-use (consumed_at). The table doubles as
--     the audit/frequency-control source of truth (60s resend interval,
--     daily per-target cap) for the send endpoint.

DO $$ BEGIN RAISE NOTICE '[Migration 000082] Adding auth verification support...'; END $$;

ALTER TABLE users ADD COLUMN phone VARCHAR(20);
CREATE UNIQUE INDEX idx_users_phone ON users(phone);

CREATE TABLE IF NOT EXISTS verification_codes (
    id          VARCHAR(36) PRIMARY KEY,
    channel     VARCHAR(8)   NOT NULL,
    target      VARCHAR(255) NOT NULL,
    purpose     VARCHAR(16)  NOT NULL,
    code_hash   VARCHAR(64)  NOT NULL,
    attempts    INTEGER      NOT NULL DEFAULT 0,
    consumed_at TIMESTAMP WITH TIME ZONE,
    expires_at  TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_verification_codes_lookup
    ON verification_codes(channel, target, purpose, created_at);
