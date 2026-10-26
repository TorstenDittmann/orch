-- Administrator certificates issued before this time are refused. orch
-- cluster certificates renew sets it to the start of the certificate it
-- issues, so a lost or leaked context stops working. Empty means no limit.
ALTER TABLE clusters ADD COLUMN administrator_not_before TEXT NOT NULL DEFAULT '';
