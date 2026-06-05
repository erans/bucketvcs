-- M27: per-tenant storage bindings for bring-your-own-bucket mode.
-- creds_json is AES-256-GCM encrypted at the application layer.
CREATE TABLE storage_bindings (
    tenant      TEXT    NOT NULL PRIMARY KEY,
    store_url   TEXT    NOT NULL,
    creds_json  BLOB    NOT NULL,
    provider    TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    verified_at INTEGER NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (16, strftime('%s','now'));
