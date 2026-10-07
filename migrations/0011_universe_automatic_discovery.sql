ALTER TABLE universes
    ADD COLUMN existence_confidence REAL NOT NULL DEFAULT 1.0
    CHECK (existence_confidence >= 0.0 AND existence_confidence <= 1.0);

ALTER TABLE universes
    ADD COLUMN provenance TEXT NOT NULL DEFAULT 'manual'
    CHECK (provenance IN ('manual', 'automatic'));

ALTER TABLE universes
    ADD COLUMN confirmed_by_user BOOLEAN NOT NULL DEFAULT 1
    CHECK (confirmed_by_user IN (0, 1));

CREATE TRIGGER universes_provenance_insert
BEFORE INSERT ON universes
WHEN NEW.provenance NOT IN ('manual', 'automatic')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe provenance');
END;

CREATE TRIGGER universes_provenance_update
BEFORE UPDATE OF provenance ON universes
WHEN NEW.provenance NOT IN ('manual', 'automatic')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe provenance');
END;

DROP TRIGGER universe_memberships_provenance_insert;
DROP TRIGGER universe_memberships_provenance_update;

CREATE TRIGGER universe_memberships_provenance_insert
BEFORE INSERT ON universe_memberships
WHEN NEW.provenance NOT IN ('provider', 'rule', 'manual', 'automatic')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe membership provenance');
END;

CREATE TRIGGER universe_memberships_provenance_update
BEFORE UPDATE OF provenance ON universe_memberships
WHEN NEW.provenance NOT IN ('provider', 'rule', 'manual', 'automatic')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe membership provenance');
END;

-- Search-query keys are Unicode-normalized by the application before storage.
-- A unique normalized key makes repeated discovery idempotent without using a
-- display title or external provider identity as a local Universe ID.
CREATE TABLE universe_discovery_keys (
    normalized_key TEXT PRIMARY KEY NOT NULL CHECK (length(trim(normalized_key)) BETWEEN 1 AND 200),
    universe_id TEXT NOT NULL REFERENCES universes(id) ON DELETE CASCADE,
    created_at_utc TEXT NOT NULL
);

CREATE INDEX universe_discovery_keys_universe_id
    ON universe_discovery_keys(universe_id, normalized_key);
