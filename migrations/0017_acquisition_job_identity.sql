-- Edition is the sole acquisition target; Work identity remains in the catalog.
-- Other Job kinds retain their existing identity and lifecycle contracts.
ALTER TABLE jobs
    ADD COLUMN target_edition_id TEXT REFERENCES editions(id) ON DELETE RESTRICT
    CHECK (
        (kind = 'acquire' AND target_edition_id IS NOT NULL AND status <> 'waiting_on_agent')
        OR (kind <> 'acquire' AND target_edition_id IS NULL)
    );

-- Persistent protection across concurrent requests and Server instances.
-- Terminal attempts release the target for a new request with a new UUID.
CREATE UNIQUE INDEX jobs_acquire_active_edition
    ON jobs(target_edition_id)
    WHERE kind = 'acquire' AND status IN ('queued', 'running');

CREATE INDEX jobs_acquire_recent
    ON jobs(created_at_utc DESC, id DESC)
    WHERE kind = 'acquire';
