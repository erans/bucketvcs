-- M20 Tier 3 custom hooks.
--
-- Note on the `trigger` column name: TRIGGER is a reserved word in both
-- SQLite (CREATE TRIGGER) and PostgreSQL. The Postgres set quotes it as
-- "trigger" in the DDL, CHECK, PK, and index; the shared Go queries in
-- internal/hooks/store.go also use the quoted identifier "trigger", which
-- both dialects accept. We chose `trigger` over the safer `trigger_kind`
-- because the user-visible CLI flag is `--trigger=pre-receive|post-receive`
-- and the column name matching the flag avoids translation noise.
CREATE TABLE hooks (
    tenant       TEXT NOT NULL,
    repo         TEXT NOT NULL,
    "trigger"    TEXT NOT NULL CHECK ("trigger" IN ('pre-receive', 'post-receive')),
    script_name  TEXT NOT NULL,
    sort_order   INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, "trigger", script_name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX hooks_lookup ON hooks (tenant, repo, "trigger", enabled, sort_order);

INSERT INTO schema_version (version, applied_at) VALUES (9, EXTRACT(EPOCH FROM now())::bigint);
