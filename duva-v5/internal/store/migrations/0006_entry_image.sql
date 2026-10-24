-- A release is an image, a tag and a digest; the tag means nothing on another
-- image. The entry carries its image, as found, rather than reading it through
-- the service, which the service sync rewrites.
ALTER TABLE queue_entries ADD COLUMN image TEXT;

-- Filled from the service as it stands. Except 'sha256': the service sync
-- took names from docker's container list, which reports a container by image
-- ID once its tag has moved on, and recorded the ID's prefix as the image.
-- Those entries are left empty and read their service's image, which the sync
-- now takes from the compose file.
UPDATE queue_entries SET image = (
    SELECT NULLIF(i.repo, 'sha256') FROM services s JOIN images i ON i.id = s.image_id
    WHERE s.id = queue_entries.service_id);

-- The tag the compose file follows, as the service sync last read it.
ALTER TABLE services ADD COLUMN tag TEXT;
