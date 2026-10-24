-- Whether the running container says duva.ignore: true, as the service sync
-- last read it. An ignored service is not tracked: not checked, not queued
-- for, not offered in "Check…" -- and listed, read-only, on the page.
ALTER TABLE services ADD COLUMN ignored INTEGER NOT NULL DEFAULT 0;
