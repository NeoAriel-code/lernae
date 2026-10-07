ALTER TABLE works
    ADD COLUMN summary TEXT NOT NULL DEFAULT '' CHECK (length(summary) <= 2048);

CREATE TABLE universes (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) BETWEEN 1 AND 128),
    title TEXT NOT NULL CHECK (length(trim(title)) BETWEEN 1 AND 512)
);

-- One Work has at most one accepted membership. Rejected candidates are kept
-- separately in universe_exclusions and never appear in this relation.
CREATE TABLE universe_memberships (
    work_id TEXT PRIMARY KEY NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    universe_id TEXT NOT NULL REFERENCES universes(id) ON DELETE CASCADE,
    provenance TEXT NOT NULL CHECK (length(trim(provenance)) BETWEEN 1 AND 64),
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    accepted_at_utc TEXT NOT NULL
);

CREATE INDEX universe_memberships_universe_id
    ON universe_memberships(universe_id, work_id);

CREATE TABLE universe_aliases (
    universe_id TEXT NOT NULL REFERENCES universes(id) ON DELETE CASCADE,
    alias TEXT NOT NULL CHECK (length(trim(alias)) BETWEEN 1 AND 256),
    provenance TEXT NOT NULL CHECK (length(trim(provenance)) BETWEEN 1 AND 64),
    created_at_utc TEXT NOT NULL,
    PRIMARY KEY (universe_id, alias)
);

CREATE INDEX universe_aliases_alias
    ON universe_aliases(alias, universe_id);

CREATE TABLE universe_exclusions (
    work_id TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    universe_id TEXT NOT NULL REFERENCES universes(id) ON DELETE CASCADE,
    reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 1 AND 512),
    recorded_at_utc TEXT NOT NULL,
    PRIMARY KEY (work_id, universe_id)
);

CREATE INDEX universe_exclusions_universe_id
    ON universe_exclusions(universe_id, work_id);
