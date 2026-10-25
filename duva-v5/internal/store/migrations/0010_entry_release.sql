-- Which release the new image says it is: its org.opencontainers.image.version
-- label, read from the registry. A digest move has no version of its own, so
-- this is what its release notes link by; linking the repository's releases
-- list instead sent Sonarr's latest to a develop build. NULL when the image
-- does not say.
ALTER TABLE queue_entries ADD COLUMN release TEXT;
