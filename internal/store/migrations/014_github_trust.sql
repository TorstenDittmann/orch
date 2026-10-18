CREATE TABLE github_trust (
 repository TEXT NOT NULL,
 application_name TEXT NOT NULL,
 branch TEXT NOT NULL,
 environment TEXT NOT NULL,
 created_at TEXT NOT NULL,
 PRIMARY KEY (repository, application_name)
) STRICT;
