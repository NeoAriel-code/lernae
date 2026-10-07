ALTER TABLE universes
    ADD COLUMN naming_confidence REAL NOT NULL DEFAULT 0.0
    CHECK (naming_confidence >= 0.0 AND naming_confidence <= 1.0);

ALTER TABLE universes
    ADD COLUMN naming_confidence_level TEXT NOT NULL DEFAULT 'none'
    CHECK (naming_confidence_level IN ('none', 'moderate', 'high'));

ALTER TABLE universes
    ADD COLUMN naming_evidence TEXT NOT NULL DEFAULT 'legacy_unknown'
    CHECK (length(naming_evidence) <= 64);

UPDATE universes
SET naming_confidence = 1.0,
    naming_confidence_level = 'high',
    naming_evidence = 'user_authored_title'
WHERE provenance = 'manual';

ALTER TABLE universe_memberships
    ADD COLUMN evidence TEXT NOT NULL DEFAULT ''
    CHECK (length(evidence) <= 64);

ALTER TABLE universe_memberships
    ADD COLUMN reason TEXT NOT NULL DEFAULT ''
    CHECK (length(reason) <= 512);
