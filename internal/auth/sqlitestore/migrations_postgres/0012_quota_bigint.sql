-- Widen the quota byte columns from 32-bit INTEGER to 64-bit BIGINT. On
-- PostgreSQL INTEGER is 32-bit (max 2147483647 ≈ 2.0 GiB), which overflows for
-- LFS objects and tenant totals above ~2 GB (the Go layer uses int64
-- throughout). This is a forward migration so existing Postgres databases
-- created under 0004/0011 (INTEGER) are converted in place; ALTER ... TYPE
-- preserves the column CHECK constraints (limit_bytes >= 0, used_bytes >= 0).
-- The SQLite set's 0012 is a no-op (SQLite INTEGER is already 64-bit).
ALTER TABLE quotas ALTER COLUMN limit_bytes TYPE BIGINT;
ALTER TABLE quotas ALTER COLUMN used_bytes TYPE BIGINT;
ALTER TABLE quota_credits ALTER COLUMN bytes TYPE BIGINT;

INSERT INTO schema_version (version, applied_at) VALUES (12, EXTRACT(EPOCH FROM now())::bigint);
