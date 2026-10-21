-- Settings are read from the environment only (internal/settings): nothing
-- edits them from the page, so a stored fallback would be a second place a
-- value could come from that nobody can see. Nothing ever wrote this table.
DROP TABLE settings;
