-- Only accepted choices are durable. Edition/Work association is derived from
-- the referenced Job; discovery snapshots are deliberately not stored here.
CREATE TABLE acquisition_selections (
    job_id TEXT PRIMARY KEY NOT NULL REFERENCES jobs(id) ON DELETE RESTRICT,
    provider_id TEXT NOT NULL CHECK (
        length(provider_id) BETWEEN 1 AND 64 AND
        substr(provider_id, 1, 1) GLOB '[a-z]' AND
        provider_id NOT GLOB '*[^a-z0-9_-]*'
    ),
    candidate_id TEXT NOT NULL CHECK (
        length(candidate_id) BETWEEN 1 AND 128 AND
        substr(candidate_id, 1, 1) GLOB '[A-Za-z0-9]' AND
        candidate_id NOT GLOB '*[^A-Za-z0-9_-]*'
    ),
    candidate_handle TEXT NOT NULL CHECK (
        length(candidate_handle) = 64 AND candidate_handle NOT GLOB '*[^a-f0-9]*'
    ),
    execution_ref TEXT NOT NULL CHECK (
        length(execution_ref) BETWEEN 1 AND 512 AND
        execution_ref NOT GLOB '*[^A-Za-z0-9_-]*'
    ),
    title TEXT NOT NULL CHECK (length(CAST(title AS BLOB)) BETWEEN 1 AND 256 AND length(trim(title)) > 0),
    label TEXT NOT NULL CHECK (length(CAST(label AS BLOB)) <= 128),
    language TEXT NOT NULL CHECK (length(language) <= 35),
    selected_at_utc TEXT NOT NULL CHECK (length(selected_at_utc) = 30)
);

-- The primary-key index supplies Job lookup and one-winner arbitration.
-- This trigger also protects trusted direct repository callers against a
-- racing Job transition. No changes to the Jobs lifecycle are needed.
CREATE TRIGGER acquisition_selection_queued_insert
BEFORE INSERT ON acquisition_selections
WHEN NOT EXISTS (
    SELECT 1 FROM jobs WHERE id = NEW.job_id AND kind = 'acquire' AND status = 'queued'
)
BEGIN
    SELECT RAISE(ABORT, 'acquisition selection requires queued acquisition');
END;

CREATE TRIGGER acquisition_selection_immutable_update
BEFORE UPDATE ON acquisition_selections
BEGIN
    SELECT RAISE(ABORT, 'acquisition selection is immutable');
END;

CREATE TRIGGER acquisition_selection_immutable_delete
BEFORE DELETE ON acquisition_selections
BEGIN
    SELECT RAISE(ABORT, 'acquisition selection is immutable');
END;
