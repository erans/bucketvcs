-- internal/auth/sqlitestore/migrations/0013_web_sessions.sql
-- Web UI (M24 Phase 1): browser sessions + local password login.
-- The session id is a 256-bit random value held only in the client cookie;
-- the database stores SHA-256(id) so a DB read cannot forge a cookie.
CREATE TABLE sessions (
    id_hash     TEXT    NOT NULL PRIMARY KEY,
    user_id     TEXT    NOT NULL,
    provider    TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);
CREATE INDEX sessions_user_idx    ON sessions(user_id);
CREATE INDEX sessions_expires_idx ON sessions(expires_at);

-- Local password login (provider 'password'). Nullable: OIDC-only users
-- (Phase 1.5) never set a password.
ALTER TABLE users ADD COLUMN password_hash TEXT;

INSERT INTO schema_version (version, applied_at) VALUES (13, strftime('%s','now'));
