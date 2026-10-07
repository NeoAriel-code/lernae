CREATE TABLE sessions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    work_id TEXT NOT NULL REFERENCES works(id),
    edition_id TEXT NOT NULL REFERENCES editions(id),
    asset_id TEXT NOT NULL REFERENCES assets(id),
    started_at_utc TEXT NOT NULL,
    ended_at_utc TEXT,
    outcome TEXT CHECK (outcome IS NULL OR outcome IN ('normal_exit', 'nonzero_exit', 'interrupted')),
    CHECK (
        (ended_at_utc IS NULL AND outcome IS NULL)
        OR (ended_at_utc IS NOT NULL AND outcome IS NOT NULL)
    ),
    CHECK (
        length(started_at_utc) = 30
        AND started_at_utc GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
    ),
    CHECK (
        ended_at_utc IS NULL
        OR (
            length(ended_at_utc) = 30
            AND ended_at_utc GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9].[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]Z'
        )
    ),
    CHECK (ended_at_utc IS NULL OR ended_at_utc >= started_at_utc)
);

CREATE INDEX sessions_active_started_at ON sessions(ended_at_utc, started_at_utc);
