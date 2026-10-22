-- Where a waiting release's image says its source lives: the running
-- container's org.opencontainers.image.source label, read with its policy
-- labels when the release is judged. The page links its release notes from
-- it. NULL when the image does not say.
ALTER TABLE queue_entries ADD COLUMN source TEXT;
