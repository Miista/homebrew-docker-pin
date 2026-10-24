-- What a block was made from. release is the version (17.2), or the tag for a
-- digest move, so a list of blocks says something a person recognises; the
-- digest is what is blocked. entry_id is the queue entry it took out of the
-- queue, so unblocking can put it back -- a version change's entry carries no
-- digest to find it by.
ALTER TABLE blocked_releases ADD COLUMN release TEXT NOT NULL DEFAULT '';
ALTER TABLE blocked_releases ADD COLUMN entry_id INTEGER REFERENCES queue_entries;
