-- The newest failure of each volume's scheduled backup, so a failure that an
-- agent keeps reporting raises one event.
CREATE TABLE volume_backup_failures (
    application_name TEXT NOT NULL,
    volume_name TEXT NOT NULL,
    failed_at TEXT NOT NULL,
    PRIMARY KEY (application_name, volume_name)
) STRICT, WITHOUT ROWID;

-- The last event the notifier has handled.
CREATE TABLE notification_cursor (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    sequence INTEGER NOT NULL CHECK (sequence >= 0)
) STRICT;
