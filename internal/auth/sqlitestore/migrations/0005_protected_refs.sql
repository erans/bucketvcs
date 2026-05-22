CREATE TABLE protected_refs (
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    refname_pattern   TEXT NOT NULL,
    block_deletion    INTEGER NOT NULL DEFAULT 1 CHECK (block_deletion IN (0,1)),
    block_force_push  INTEGER NOT NULL DEFAULT 1 CHECK (block_force_push IN (0,1)),
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, refname_pattern),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

INSERT INTO schema_version (version, applied_at) VALUES (5, strftime('%s','now'));
