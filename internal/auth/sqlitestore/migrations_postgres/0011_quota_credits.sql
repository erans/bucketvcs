-- quota_credits records each counted (tenant, oid) LFS upload so that
-- verify-replay (the same upload arriving twice, possibly on different gateway
-- nodes) increments used_bytes exactly once. The unique PK is the cross-node
-- idempotency point. Rows are removed on sweep/delete (Subtract), keeping the
-- table bounded to currently-counted objects.
CREATE TABLE quota_credits (
    tenant      TEXT    NOT NULL,
    oid         TEXT    NOT NULL,
    bytes       INTEGER NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (tenant, oid)
);

INSERT INTO schema_version (version, applied_at) VALUES (11, EXTRACT(EPOCH FROM now())::bigint);
