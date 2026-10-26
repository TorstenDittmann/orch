-- Domains an access token or a GitHub trust may route, each with its
-- subdomains, separated by spaces. Empty means no limit.
ALTER TABLE access_tokens ADD COLUMN domains TEXT NOT NULL DEFAULT '';
ALTER TABLE github_trust ADD COLUMN domains TEXT NOT NULL DEFAULT '';
