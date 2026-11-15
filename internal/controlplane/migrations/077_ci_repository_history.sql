DROP INDEX IF EXISTS ci_repositories_name;
CREATE INDEX ci_repositories_name ON ci_repositories(installation_id,lower(full_name));
