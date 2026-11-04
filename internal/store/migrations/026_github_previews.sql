-- Whether pull_request runs may deploy preview applications APP-pr-N.
ALTER TABLE github_trust ADD COLUMN previews INTEGER NOT NULL DEFAULT 0;
