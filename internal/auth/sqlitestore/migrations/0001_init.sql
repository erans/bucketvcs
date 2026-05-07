CREATE TABLE schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE users (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    is_admin    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    disabled_at INTEGER
);

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    secret_hash  TEXT NOT NULL,
    label        TEXT,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
CREATE INDEX tokens_user_idx ON tokens(user_id);

CREATE TABLE repos (
    tenant      TEXT NOT NULL,
    name        TEXT NOT NULL,
    public_read INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, name)
);

CREATE TABLE repo_permissions (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    perm       TEXT NOT NULL CHECK (perm IN ('read','write','admin')),
    granted_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, tenant, repo),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);

INSERT INTO schema_version (version, applied_at) VALUES (1, strftime('%s','now'));
