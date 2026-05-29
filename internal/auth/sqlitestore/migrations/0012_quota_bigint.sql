-- No-op on SQLite: its INTEGER storage class is dynamic and holds full 64-bit
-- values, so the quota byte columns (quotas.limit_bytes, quotas.used_bytes,
-- quota_credits.bytes) already store sizes far beyond 2^31. This migration
-- exists only to keep schema_version in lockstep with the Postgres set, where
-- 0012 widens those columns from 32-bit INTEGER to 64-bit BIGINT. SQLite's
-- limited ALTER TABLE cannot change a column's declared type, and there is
-- nothing to fix here, so the migration is intentionally empty apart from the
-- version bump.

INSERT INTO schema_version (version, applied_at) VALUES (12, strftime('%s','now'));
