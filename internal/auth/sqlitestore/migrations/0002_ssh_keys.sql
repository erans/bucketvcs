CREATE TABLE ssh_keys (
    id              TEXT PRIMARY KEY,
    fingerprint     TEXT NOT NULL UNIQUE,
    public_key      BLOB NOT NULL,
    key_type        TEXT NOT NULL,
    label           TEXT,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER,

    user_id         TEXT REFERENCES users(id) ON DELETE CASCADE,
    scope_tenant    TEXT,
    scope_repo      TEXT,
    scope_perm      TEXT CHECK (scope_perm IN ('read','write')),

    CHECK (
        (user_id IS NOT NULL AND scope_tenant IS NULL
                              AND scope_repo IS NULL
                              AND scope_perm IS NULL)
        OR
        (user_id IS NULL      AND scope_tenant IS NOT NULL
                              AND scope_repo IS NOT NULL
                              AND scope_perm IS NOT NULL)
    ),
    FOREIGN KEY (scope_tenant, scope_repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

CREATE UNIQUE INDEX ssh_keys_fingerprint_idx ON ssh_keys(fingerprint);
CREATE INDEX        ssh_keys_user_idx        ON ssh_keys(user_id);
CREATE INDEX        ssh_keys_scope_idx       ON ssh_keys(scope_tenant, scope_repo);

INSERT INTO schema_version (version, applied_at) VALUES (2, strftime('%s','now'));
