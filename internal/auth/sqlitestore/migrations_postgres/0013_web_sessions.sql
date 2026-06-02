-- internal/auth/sqlitestore/migrations_postgres/0013_web_sessions.sql
-- Web UI (M24 Phase 1): browser sessions + local password login.
-- Postgres parallel of the SQLite 0013 set (keeps schema_version in lockstep).
CREATE TABLE sessions (
    id_hash     TEXT   NOT NULL PRIMARY KEY,
    user_id     TEXT   NOT NULL,
    provider    TEXT   NOT NULL,
    created_at  BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL,
    last_seen   BIGINT NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX sessions_user_idx    ON sessions(user_id);
CREATE INDEX sessions_expires_idx ON sessions(expires_at);

ALTER TABLE users ADD COLUMN password_hash TEXT;

INSERT INTO schema_version (version, applied_at) VALUES (13, EXTRACT(EPOCH FROM now())::bigint);
