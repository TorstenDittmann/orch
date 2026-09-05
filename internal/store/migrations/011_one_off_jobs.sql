CREATE TABLE one_off_jobs (
 id TEXT PRIMARY KEY,
 application_name TEXT NOT NULL REFERENCES applications(name),
 revision_id TEXT NOT NULL REFERENCES application_revisions(id),
 node_id TEXT NOT NULL REFERENCES nodes(id),
 service_name TEXT NOT NULL,
 kind TEXT NOT NULL CHECK(kind IN ('manual','pre_deploy')),
 retry_of TEXT NOT NULL,
 template TEXT NOT NULL CHECK(json_valid(template)),
 status TEXT NOT NULL CHECK(json_valid(status)),
 cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK(cancel_requested IN (0,1)),
 created_at TEXT NOT NULL
) STRICT;
CREATE INDEX one_off_jobs_application ON one_off_jobs(application_name, created_at DESC, id);
CREATE TRIGGER one_off_jobs_immutable BEFORE UPDATE OF id, application_name, revision_id, node_id, service_name, kind, retry_of, template, created_at ON one_off_jobs
BEGIN SELECT RAISE(ABORT, 'job intent is immutable'); END;
