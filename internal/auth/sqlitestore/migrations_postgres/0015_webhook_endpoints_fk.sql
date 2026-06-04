-- internal/auth/sqlitestore/migrations_postgres/0015_webhook_endpoints_fk.sql
-- M25: drop the webhook_endpoints→repos FK (created unnamed by 0006). The
-- M15.1 drain design requires webhook_endpoints rows to SURVIVE repo
-- deletion so a pending repo.deleted delivery still joins its endpoint at
-- claim time. Postgres has no per-connection FK suppression (sqlite's
-- PRAGMA foreign_keys=OFF), so the constraint itself must go.
-- PG auto-names this unnamed composite FK after the FIRST column only →
-- webhook_endpoints_tenant_fkey (all versions). The _tenant_repo_fkey DROP
-- is belt-and-suspenders for any hand-named variant; both are IF EXISTS.
ALTER TABLE webhook_endpoints DROP CONSTRAINT IF EXISTS webhook_endpoints_tenant_repo_fkey;
ALTER TABLE webhook_endpoints DROP CONSTRAINT IF EXISTS webhook_endpoints_tenant_fkey;
INSERT INTO schema_version (version, applied_at) VALUES (15, EXTRACT(EPOCH FROM now())::bigint);
