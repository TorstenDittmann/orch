CREATE TABLE service_scales (
    application_name TEXT NOT NULL,
    service_name TEXT NOT NULL,
    replicas INTEGER NOT NULL CHECK (replicas BETWEEN 1 AND 100),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (application_name, service_name)
) STRICT, WITHOUT ROWID;
