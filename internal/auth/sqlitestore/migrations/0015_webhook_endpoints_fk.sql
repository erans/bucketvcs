-- internal/auth/sqlitestore/migrations/0015_webhook_endpoints_fk.sql
-- M25: no-op on sqlite. The postgres twin drops the webhook_endpoints→repos
-- FK so DeleteRepoCascade can leave endpoint rows alive (M15.1 drain design:
-- a pending repo.deleted delivery must still join its endpoint at claim
-- time). sqlite achieves the same by suppressing FK enforcement via the
-- per-connection PRAGMA foreign_keys=OFF, so its (now decorative) FK stays.
INSERT INTO schema_version (version, applied_at) VALUES (15, strftime('%s','now'));
