CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at_utc TEXT NOT NULL
);

CREATE TABLE jobs (
    id TEXT PRIMARY KEY NOT NULL,
    kind TEXT NOT NULL CHECK (length(kind) > 0),
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'waiting_on_agent', 'succeeded', 'failed', 'interrupted', 'cancelled')),
    progress_current INTEGER NOT NULL DEFAULT 0 CHECK (progress_current >= 0),
    progress_total INTEGER NOT NULL DEFAULT 0 CHECK (progress_total >= 0),
    message TEXT NOT NULL DEFAULT '',
    error_code TEXT NOT NULL DEFAULT '',
    error_detail TEXT NOT NULL DEFAULT '',
    created_at_utc TEXT NOT NULL,
    updated_at_utc TEXT NOT NULL,
    CHECK (progress_total = 0 OR progress_current <= progress_total)
);

CREATE INDEX jobs_status_updated_at ON jobs(status, updated_at_utc);
