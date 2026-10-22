-- What each of duva's jobs did, the last few times it ran, for the page's
-- Jobs list. Nothing references it; trimmed as it is written.
CREATE TABLE job_runs (
    id         INTEGER PRIMARY KEY,
    job        TEXT NOT NULL,           -- check, sync, reconcile, release
    started_at TEXT NOT NULL,
    took_ms    INTEGER NOT NULL,
    summary    TEXT NOT NULL,           -- one line
    output     TEXT NOT NULL DEFAULT '', -- what it said, a line each
    failed     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX job_runs_latest ON job_runs (job, id);
