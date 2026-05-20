-- M13.3: LFS Locks
-- Lock state for the Git LFS Locking API. Per-repo isolation enforced
-- via (tenant, repo) columns. UNIQUE(tenant, repo, path) enforces the
-- "one lock per path per repo" invariant at the DB layer.
CREATE TABLE lfs_locks (
    id              TEXT PRIMARY KEY,
    tenant          TEXT NOT NULL,
    repo            TEXT NOT NULL,
    path            TEXT NOT NULL,
    ref_name        TEXT,                       -- nullable: NULL means repo-wide
    owner_user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    locked_at       INTEGER NOT NULL,           -- unix seconds
    UNIQUE(tenant, repo, path)
);
CREATE INDEX idx_lfs_locks_tenant_repo ON lfs_locks(tenant, repo);
CREATE INDEX idx_lfs_locks_owner ON lfs_locks(owner_user_id);
INSERT INTO schema_version (version, applied_at) VALUES (3, strftime('%s','now'));
