-- What a block was made from, as a person reads it: the version (17.2), or
-- the tag for a digest move. The digest is what is blocked; this is only so
-- a list of blocks says something a person recognises.
ALTER TABLE blocked_releases ADD COLUMN release TEXT NOT NULL DEFAULT '';
