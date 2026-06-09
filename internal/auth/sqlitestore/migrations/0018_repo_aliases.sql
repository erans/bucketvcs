-- Repo rename aliases: (tenant, old_name) -> current live target_name.
-- No FK to repos: old_name must NOT be a live repo, and target cleanup is
-- handled explicitly by RenameRepo / RegisterRepo / DeleteRepoCascade.
CREATE TABLE repo_aliases (
    tenant      TEXT NOT NULL,
    old_name    TEXT NOT NULL,
    target_name TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (tenant, old_name)
);
CREATE INDEX idx_repo_aliases_target ON repo_aliases(tenant, target_name);

INSERT INTO schema_version (version, applied_at) VALUES (18, strftime('%s','now'));
