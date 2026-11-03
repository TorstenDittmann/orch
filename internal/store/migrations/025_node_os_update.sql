-- The lifecycle orch node update restores once a node's agent is back after
-- the reboot, and the boot id of the agent that rebooted. Empty means none.
ALTER TABLE nodes ADD COLUMN os_update_lifecycle TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN os_update_boot_id TEXT NOT NULL DEFAULT '';
