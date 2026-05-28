CREATE TABLE oidc_issuers (
    alias       TEXT PRIMARY KEY,
    issuer_url  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL
);

CREATE TABLE oidc_trust_rules (
    id            TEXT PRIMARY KEY,
    issuer_alias  TEXT NOT NULL REFERENCES oidc_issuers(alias) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    audience      TEXT NOT NULL,
    tenant        TEXT NOT NULL,
    repo          TEXT NOT NULL,
    scopes        INTEGER NOT NULL,
    ttl_seconds   INTEGER NOT NULL CHECK (ttl_seconds > 0),
    created_at    INTEGER NOT NULL,
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
CREATE INDEX oidc_rules_issuer_idx ON oidc_trust_rules(issuer_alias);
CREATE INDEX oidc_rules_repo_idx   ON oidc_trust_rules(tenant, repo);

CREATE TABLE oidc_rule_claims (
    rule_id     TEXT NOT NULL REFERENCES oidc_trust_rules(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    claim_name  TEXT NOT NULL,
    claim_value TEXT NOT NULL,
    PRIMARY KEY (rule_id, claim_name)
);

-- Reserved system user so OIDC-minted tokens satisfy tokens.user_id NOT NULL.
INSERT INTO users (id, name, is_admin, created_at)
VALUES ('_oidc', '_oidc', 0, EXTRACT(EPOCH FROM now())::bigint);

-- Repo-binding columns on tokens (NULL for ordinary user tokens).
ALTER TABLE tokens ADD COLUMN scope_tenant TEXT;
ALTER TABLE tokens ADD COLUMN scope_repo   TEXT;
ALTER TABLE tokens ADD COLUMN scope_perm   TEXT CHECK (scope_perm IS NULL OR scope_perm IN ('read','write'));

INSERT INTO schema_version (version, applied_at) VALUES (10, EXTRACT(EPOCH FROM now())::bigint);
