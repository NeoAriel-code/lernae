ALTER TABLE universe_memberships
    ADD COLUMN confirmed_by_user BOOLEAN NOT NULL DEFAULT 0
    CHECK (confirmed_by_user IN (0, 1));

-- Preserve bounded legacy provenance values on existing rows while restricting
-- new and changed membership sources to the supported source categories.
CREATE TRIGGER universe_memberships_provenance_insert
BEFORE INSERT ON universe_memberships
WHEN NEW.provenance NOT IN ('provider', 'rule', 'manual')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe membership provenance');
END;

CREATE TRIGGER universe_memberships_provenance_update
BEFORE UPDATE OF provenance ON universe_memberships
WHEN NEW.provenance NOT IN ('provider', 'rule', 'manual')
BEGIN
    SELECT RAISE(ABORT, 'invalid Universe membership provenance');
END;
