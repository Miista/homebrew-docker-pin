# duva v5: backlog

Agreed, not started.

## A running container newer than the compose file

duva trusts the compose file's pin. A container recreated outside compose can
run a newer image than the file pins (grafana on optiplex was created before
its pin, and `latest` moved under it). Applying an entry then recreates from
the file, which can start an *older* version than the one that ran, and a
database it already migrated may not take that.

- Only newer-than-the-file matters. Older is an ordinary pending update.
- Newer by the image's creation time: the running image's `Created` is local;
  the pinned one's comes from the registry's image config.
  `org.opencontainers.image.version` where both carry it.
- Cannot tell: hold, as an unclassifiable version is treated as major.
- The sync records the running digest; the updater refuses a service whose
  container is newer than its pin; the page says "running a newer image than
  the compose file pins".
