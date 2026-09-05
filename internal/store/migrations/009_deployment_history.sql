CREATE TABLE deployment_records (
    revision_id TEXT PRIMARY KEY REFERENCES application_revisions(id) ON DELETE RESTRICT,
    rollback_from_revision_id TEXT REFERENCES application_revisions(id) ON DELETE RESTRICT,
    outcome TEXT NOT NULL CHECK (outcome IN ('UNKNOWN', 'IN_PROGRESS', 'SUCCEEDED', 'FAILED', 'ROLLED_BACK', 'SUPERSEDED', 'REMOVED')),
    status TEXT NOT NULL,
    reason TEXT NOT NULL,
    message TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    finished_at TEXT
) STRICT;

-- Previous revisions have no durable execution history. Do not invent results.
INSERT INTO deployment_records (revision_id, outcome, status, reason, message, updated_at)
SELECT id, 'UNKNOWN', '', 'history_unavailable', 'outcome was not retained before deployment history was introduced', created_at
FROM application_revisions;

-- Preserve the observation that is actually available for the current revision.
UPDATE deployment_records SET
    status = x.status, reason = x.reason, message = x.message, updated_at = x.updated_at,
    outcome = CASE
        WHEN a.deleted_at IS NOT NULL THEN 'REMOVED'
        WHEN x.observed_generation = a.generation AND x.status IN ('HEALTHY', 'RUNNING')
          AND x.active_generation = a.generation AND x.active_revision_id = deployment_records.revision_id THEN 'SUCCEEDED'
        WHEN x.observed_generation = a.generation AND x.status IN ('FAILED', 'ROLLED_BACK') THEN x.status
        ELSE 'IN_PROGRESS' END
FROM applications a JOIN application_execution x ON x.application_name = a.name
JOIN application_revisions r ON r.application_name = a.name AND r.generation = a.generation
WHERE deployment_records.revision_id = r.id;

UPDATE deployment_records SET finished_at = updated_at
WHERE outcome IN ('SUCCEEDED', 'FAILED', 'ROLLED_BACK', 'REMOVED');
