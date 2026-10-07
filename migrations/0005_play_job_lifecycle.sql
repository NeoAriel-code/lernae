ALTER TABLE jobs
    ADD COLUMN phase TEXT NOT NULL DEFAULT ''
    CHECK (phase IN ('', 'restore', 'launch', 'playing'));

ALTER TABLE jobs
    ADD COLUMN session_id TEXT;
