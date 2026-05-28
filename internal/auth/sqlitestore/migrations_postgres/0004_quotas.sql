CREATE TABLE quotas (
    tenant      TEXT PRIMARY KEY,
    limit_bytes INTEGER NOT NULL CHECK (limit_bytes >= 0),
    used_bytes  INTEGER NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    updated_at  INTEGER NOT NULL
);

INSERT INTO schema_version (version, applied_at) VALUES (4, EXTRACT(EPOCH FROM now())::bigint);
