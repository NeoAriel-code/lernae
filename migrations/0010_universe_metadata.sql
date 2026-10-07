-- Universe metadata is intentionally nullable. The existing schema did not
-- retain creation timestamps, so legacy rows receive one shared migration-time
-- approximation rather than a fabricated historical date.
ALTER TABLE universes
    ADD COLUMN sort_title TEXT CHECK (sort_title IS NULL OR length(sort_title) <= 512);

ALTER TABLE universes
    ADD COLUMN description TEXT CHECK (description IS NULL OR length(description) <= 2048);

ALTER TABLE universes
    ADD COLUMN artwork TEXT CHECK (artwork IS NULL OR length(artwork) <= 2048);

ALTER TABLE universes
    ADD COLUMN created_at_utc TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z';

ALTER TABLE universes
    ADD COLUMN updated_at_utc TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z';

UPDATE universes
SET created_at_utc = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
    updated_at_utc = strftime('%Y-%m-%dT%H:%M:%fZ', 'now');
