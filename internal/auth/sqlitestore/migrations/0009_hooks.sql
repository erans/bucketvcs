-- M20 Tier 3 custom hooks.
--
-- Note on the `trigger` column name: TRIGGER is a SQLite reserved word
-- (used in CREATE TRIGGER), but it's accepted as a bare column identifier
-- by both the SQLite parser and modernc.org/sqlite. We chose `trigger` over
-- the safer `trigger_kind` because the user-visible CLI flag is
-- `--trigger=pre-receive|post-receive` and the column name matching the
-- flag avoids translation noise in every read site. All queries in
-- internal/hooks/store.go use the bare identifier; if a future SQLite
-- version or alternate driver objects, rename here and there.
CREATE TABLE hooks (
    tenant       TEXT NOT NULL,
    repo         TEXT NOT NULL,
    trigger      TEXT NOT NULL CHECK (trigger IN ('pre-receive', 'post-receive')),
    script_name  TEXT NOT NULL,
    sort_order   INTEGER NOT NULL DEFAULT 0,
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, trigger, script_name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX hooks_lookup ON hooks (tenant, repo, trigger, enabled, sort_order);

INSERT INTO schema_version (version, applied_at) VALUES (9, strftime('%s','now'));
