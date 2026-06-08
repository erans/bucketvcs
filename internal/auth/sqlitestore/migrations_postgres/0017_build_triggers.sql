-- M30: build triggers — fire CI builds on push.
-- build_trigger_deliveries intentionally has NO FK to build_triggers (orphan-drain design).
CREATE TABLE build_triggers (
    id                TEXT PRIMARY KEY,
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    name              TEXT NOT NULL,
    kind              TEXT NOT NULL,
    config_json       BYTEA NOT NULL,
    ref_include       BYTEA NOT NULL,
    ref_exclude       BYTEA NOT NULL,
    token_mode        TEXT NOT NULL,
    token_scopes      INTEGER NOT NULL,
    token_ttl_seconds INTEGER NOT NULL,
    active            INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0,1)),
    created_at        BIGINT NOT NULL,
    UNIQUE (tenant, repo, name),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX build_triggers_by_repo ON build_triggers (tenant, repo, active);

CREATE TABLE build_trigger_deliveries (
    id                TEXT PRIMARY KEY,
    trigger_id        TEXT NOT NULL,
    payload_json      BYTEA NOT NULL,
    status            TEXT NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0,
    next_attempt_at   BIGINT NOT NULL,
    last_attempt_at   BIGINT,
    last_status_code  INTEGER,
    last_error        TEXT,
    delivered_at      BIGINT,
    created_at        BIGINT NOT NULL
);
CREATE INDEX build_trigger_deliveries_claim ON build_trigger_deliveries (status, next_attempt_at);

-- Reserved system user so build-trigger-minted tokens satisfy tokens.user_id NOT NULL.
INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_build', '_build', 0, EXTRACT(EPOCH FROM now())::bigint);

INSERT INTO schema_version (version, applied_at) VALUES (17, EXTRACT(EPOCH FROM now())::bigint);
