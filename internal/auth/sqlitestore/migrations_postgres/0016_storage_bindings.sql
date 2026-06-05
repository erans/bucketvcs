-- M27: per-tenant storage bindings for bring-your-own-bucket mode.
CREATE TABLE storage_bindings (
    tenant      TEXT    NOT NULL PRIMARY KEY,
    store_url   TEXT    NOT NULL,
    creds_json  BYTEA   NOT NULL,
    provider    TEXT    NOT NULL,
    created_at  BIGINT  NOT NULL,
    updated_at  BIGINT  NOT NULL,
    verified_at BIGINT  NOT NULL
);
INSERT INTO schema_version (version, applied_at) VALUES (16, EXTRACT(EPOCH FROM now())::bigint);
