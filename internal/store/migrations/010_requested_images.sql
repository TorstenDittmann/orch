-- Existing revisions already contain the only known requested references.
ALTER TABLE application_revisions ADD COLUMN requested_manifest TEXT;
