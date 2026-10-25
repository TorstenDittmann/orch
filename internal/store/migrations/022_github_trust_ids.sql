-- The GitHub repository and owner IDs a trust rule is bound to, recorded from
-- the first token that matches it. A repository deleted or renamed and then
-- recreated under the same name has new IDs and is refused. Empty until the
-- first deploy.
ALTER TABLE github_trust ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';
ALTER TABLE github_trust ADD COLUMN repository_owner_id TEXT NOT NULL DEFAULT '';
