-- duva's state, phase 2 of docs/duva-v5-one-binary.md.
--
-- Foreign keys go into services, never into watch_cutoffs: the watcher's
-- state is disposable, and wiping it must cascade into nothing and be blocked
-- by nothing. A queue entry copies what was found rather than referencing it.
-- History is never cascaded away: services are disabled, not deleted.
--
-- Times are RFC 3339 text in UTC, so they sort and compare as strings.

CREATE TABLE images (
    id   INTEGER PRIMARY KEY,
    repo TEXT NOT NULL UNIQUE           -- example.com/app: no tag, no digest
);

CREATE TABLE services (
    id         INTEGER PRIMARY KEY,
    container  TEXT NOT NULL UNIQUE,    -- container_name: the identity
    name       TEXT NOT NULL,           -- the compose service name
    image_id   INTEGER NOT NULL REFERENCES images,
    removed_at TEXT                     -- disabled, never deleted
);

CREATE TABLE watch_cutoffs (
    service_id INTEGER PRIMARY KEY REFERENCES services,
    cutoff     TEXT NOT NULL            -- look for tags published after this
);

CREATE TABLE queue_entries (
    id           INTEGER PRIMARY KEY,
    service_id   INTEGER NOT NULL REFERENCES services,
    file         TEXT NOT NULL,         -- the compose file it was declared in
    from_ref     TEXT NOT NULL,
    to_ref       TEXT NOT NULL,         -- a tag, or a digest for a digest move
    tag          TEXT NOT NULL,         -- the tag to follow afterwards
    digest       TEXT,                  -- index digest, once known
    kind         TEXT,                  -- patch/minor/major/unknown; NULL = digest move
    auto         TEXT NOT NULL,         -- the duva.auto it was decided under
    why          TEXT NOT NULL,
    published_at TEXT,
    found_at     TEXT NOT NULL,
    release_at   TEXT,                  -- a soak's end; NULL = waits for a person
    -- A failed apply is not a status: the entry goes back to waiting for a
    -- person, and the failure is an event.
    status       TEXT NOT NULL CHECK (status IN
                   ('waiting', 'applying', 'completed',
                    'discarded', 'superseded', 'blocked', 'dropped'))
);

-- What is open, by service: the queue's working set.
CREATE INDEX queue_entries_open ON queue_entries (service_id)
    WHERE status IN ('waiting', 'applying');

CREATE TABLE queue_events (
    id         INTEGER PRIMARY KEY,
    entry_id   INTEGER REFERENCES queue_entries,
    service_id INTEGER REFERENCES services,
    at         TEXT NOT NULL,
    kind       TEXT NOT NULL,           -- found, soaking, released, approved, applied, ...
    actor      TEXT NOT NULL,           -- "duva", or who did it
    detail     TEXT
);

CREATE INDEX queue_events_entry ON queue_events (entry_id);
CREATE INDEX queue_events_service ON queue_events (service_id);

CREATE TABLE blocked_releases (
    id           INTEGER PRIMARY KEY,
    image_id     INTEGER NOT NULL REFERENCES images,
    digest       TEXT NOT NULL,         -- index digest; never a tag
    reason       TEXT NOT NULL,
    blocked_by   TEXT NOT NULL,
    blocked_at   TEXT NOT NULL,
    unblocked_at TEXT
);

-- One standing block per digest; an unblocked one stays as history.
CREATE UNIQUE INDEX blocked_releases_standing ON blocked_releases (image_id, digest)
    WHERE unblocked_at IS NULL;
