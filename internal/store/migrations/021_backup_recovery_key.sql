-- The public half of the recovery key controller backups are encrypted to
-- before they are copied to the bucket. Empty for destinations set before
-- encryption existed; their copies stay unencrypted until set again.
ALTER TABLE backup_destination ADD COLUMN recovery_public_key TEXT NOT NULL DEFAULT '';
