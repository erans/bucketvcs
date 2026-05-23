CREATE TABLE protected_paths (
    tenant            TEXT NOT NULL,
    repo              TEXT NOT NULL,
    refname_pattern   TEXT NOT NULL,
    path_pattern      TEXT NOT NULL,
    created_at        INTEGER NOT NULL,
    PRIMARY KEY (tenant, repo, refname_pattern, path_pattern),
    FOREIGN KEY (tenant, repo) REFERENCES repos(tenant, name) ON DELETE CASCADE
);
CREATE INDEX protected_paths_by_repo ON protected_paths (tenant, repo);

INSERT INTO schema_version (version, applied_at) VALUES (7, strftime('%s','now'));
