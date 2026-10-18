-- Earlier deployments have no recorded source.
ALTER TABLE deployment_records ADD COLUMN source TEXT CHECK (source IS NULL OR json_valid(source));
