ALTER TABLE tokens ADD COLUMN scopes INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_version (version, applied_at) VALUES (8, EXTRACT(EPOCH FROM now())::bigint);
