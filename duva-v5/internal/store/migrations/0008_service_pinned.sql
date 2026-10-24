-- Whether the compose file pins the service by digest, as the service sync
-- last read it: pinned is what duva tracks, and what "Check…" offers. Kept by
-- the sync rather than taken from the watcher's last full check, which left a
-- service pinned since then impossible to pick until the next one.
ALTER TABLE services ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
