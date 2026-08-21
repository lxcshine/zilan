-- Roll back P0-4 auth verification support.
DROP TABLE IF EXISTS verification_codes;
DROP INDEX IF EXISTS idx_users_phone;
ALTER TABLE users DROP COLUMN IF EXISTS phone;
