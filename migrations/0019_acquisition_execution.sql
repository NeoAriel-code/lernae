-- One permanent dispatch reservation per immutable accepted selection. These
-- fields record boundary provenance, not a second Job lifecycle or payload.
CREATE TABLE acquisition_executions (
    job_id TEXT PRIMARY KEY NOT NULL REFERENCES acquisition_selections(job_id) ON DELETE RESTRICT,
    execution_id TEXT UNIQUE NOT NULL CHECK (
        length(execution_id) = 64 AND execution_id NOT GLOB '*[^a-f0-9]*'
    ),
    dispatch_outcome TEXT NOT NULL DEFAULT 'reserved'
        CHECK (dispatch_outcome IN ('reserved', 'accepted', 'rejected', 'unconfirmed')),
    reserved_at_utc TEXT NOT NULL CHECK (length(reserved_at_utc) = 30),
    decided_at_utc TEXT CHECK (decided_at_utc IS NULL OR length(decided_at_utc) = 30),
    CHECK ((dispatch_outcome = 'reserved') = (decided_at_utc IS NULL))
);

CREATE TRIGGER acquisition_execution_queued_insert
BEFORE INSERT ON acquisition_executions
WHEN NEW.dispatch_outcome <> 'reserved' OR NOT EXISTS (
    SELECT 1 FROM jobs WHERE id = NEW.job_id AND kind = 'acquire' AND status = 'queued'
)
BEGIN
    SELECT RAISE(ABORT, 'execution reservation requires queued acquisition');
END;

CREATE TRIGGER acquisition_execution_guard_update
BEFORE UPDATE ON acquisition_executions
WHEN NEW.job_id <> OLD.job_id OR NEW.execution_id <> OLD.execution_id
    OR NEW.reserved_at_utc <> OLD.reserved_at_utc
    OR OLD.dispatch_outcome <> 'reserved' OR NEW.dispatch_outcome = 'reserved'
BEGIN
    SELECT RAISE(ABORT, 'execution reservation is permanent');
END;

CREATE TRIGGER acquisition_execution_guard_delete
BEFORE DELETE ON acquisition_executions
BEGIN
    SELECT RAISE(ABORT, 'execution reservation is permanent');
END;
