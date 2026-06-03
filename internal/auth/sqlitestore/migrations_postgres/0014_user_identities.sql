-- internal/auth/sqlitestore/migrations_postgres/0014_user_identities.sql
-- M24 Phase 1.5: OIDC browser login — external identity → local user mapping.
ALTER TABLE users ADD COLUMN email TEXT;
CREATE UNIQUE INDEX users_email_idx ON users(email) WHERE email IS NOT NULL;

CREATE TABLE user_identities (
    id          TEXT   NOT NULL PRIMARY KEY,
    user_id     TEXT   NOT NULL,
    provider    TEXT   NOT NULL,
    issuer      TEXT   NOT NULL,
    subject     TEXT   NOT NULL,
    email       TEXT,
    created_at  BIGINT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED
);
CREATE UNIQUE INDEX user_identities_issuer_subject_idx ON user_identities(issuer, subject);
CREATE INDEX user_identities_user_idx ON user_identities(user_id);

INSERT INTO schema_version (version, applied_at) VALUES (14, EXTRACT(EPOCH FROM now())::bigint);
