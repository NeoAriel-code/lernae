-- Re-key legacy aliases by normalized value and locale. Whitespace-equivalent
-- duplicates retain the manual-authority row, provenance, and timestamp.
ALTER TABLE universe_aliases RENAME TO universe_aliases_legacy;
DROP INDEX IF EXISTS universe_aliases_alias;

CREATE TABLE universe_aliases (
    id INTEGER PRIMARY KEY,
    universe_id TEXT NOT NULL REFERENCES universes(id) ON DELETE CASCADE,
    alias TEXT NOT NULL CHECK (length(trim(alias)) BETWEEN 1 AND 256),
    language_code TEXT CHECK (language_code IS NULL OR length(trim(language_code)) BETWEEN 1 AND 35),
    provenance TEXT NOT NULL CHECK (length(trim(provenance)) BETWEEN 1 AND 64),
    provider_version TEXT CHECK (provider_version IS NULL OR length(provider_version) <= 128),
    confidence REAL NOT NULL DEFAULT 0.0 CHECK (confidence >= 0.0 AND confidence <= 1.0),
    confirmed_by_user INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_by_user IN (0, 1)),
    created_at_utc TEXT NOT NULL
);

WITH RECURSIVE normalized_aliases AS (
    SELECT rowid AS legacy_id, universe_id, alias, provenance, created_at_utc, trim(alias) AS normalized_alias
    FROM universe_aliases_legacy
    UNION ALL
    SELECT legacy_id, universe_id, alias, provenance, created_at_utc,
           replace(normalized_alias, '  ', ' ')
    FROM normalized_aliases
    WHERE instr(normalized_alias, '  ') > 0
), ranked_aliases AS (
    SELECT legacy_id, universe_id, alias, provenance, created_at_utc, normalized_alias,
           row_number() OVER (
               PARTITION BY universe_id, normalized_alias
               ORDER BY CASE WHEN provenance = 'manual' THEN 0 ELSE 1 END, created_at_utc, legacy_id
           ) AS duplicate_rank
    FROM normalized_aliases
    WHERE instr(normalized_alias, '  ') = 0
)
INSERT INTO universe_aliases (
    universe_id, alias, language_code, provenance,
    provider_version, confidence, confirmed_by_user, created_at_utc
)
SELECT universe_id, normalized_alias, NULL, provenance, NULL,
       CASE WHEN provenance = 'manual' THEN 1.0 ELSE 0.0 END,
       CASE WHEN provenance = 'manual' THEN 1 ELSE 0 END,
       created_at_utc
FROM ranked_aliases
WHERE duplicate_rank = 1;

DROP TABLE universe_aliases_legacy;

CREATE UNIQUE INDEX universe_aliases_identity
    ON universe_aliases(universe_id, alias, IFNULL(language_code, ''));

CREATE INDEX universe_aliases_alias
    ON universe_aliases(alias, universe_id);

CREATE TRIGGER universe_aliases_manual_authority
BEFORE UPDATE ON universe_aliases
WHEN (OLD.confirmed_by_user = 1 OR OLD.provenance = 'manual')
  AND NEW.provenance <> 'manual'
BEGIN
    SELECT RAISE(ABORT, 'automatic alias refresh cannot replace manual alias authority');
END;

CREATE TABLE work_collections (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) BETWEEN 1 AND 128),
    title TEXT NOT NULL CHECK (length(trim(title)) BETWEEN 1 AND 512),
    collection_type TEXT NOT NULL CHECK (collection_type IN ('series')),
    created_at_utc TEXT NOT NULL
);

CREATE TABLE work_relations (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) BETWEEN 1 AND 80),
    work_id TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    relation_type TEXT NOT NULL CHECK (relation_type IN (
        'part_of_series', 'adaptation_of', 'sequel_of', 'follows',
        'spin_off_of', 'companion_to', 'franchise_member'
    )),
    target_work_id TEXT REFERENCES works(id) ON DELETE CASCADE,
    target_collection_id TEXT REFERENCES work_collections(id) ON DELETE CASCADE,
    ordinal TEXT CHECK (ordinal IS NULL OR length(trim(ordinal)) BETWEEN 1 AND 64),
    provenance TEXT NOT NULL CHECK (provenance IN ('provider', 'rule', 'manual', 'automatic')),
    provider_version TEXT CHECK (provider_version IS NULL OR length(provider_version) <= 128),
    confidence REAL NOT NULL CHECK (confidence >= 0.0 AND confidence <= 1.0),
    evidence TEXT NOT NULL DEFAULT '' CHECK (length(evidence) <= 2048),
    confirmed_by_user INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_by_user IN (0, 1)),
    created_at_utc TEXT NOT NULL,
    CHECK ((target_work_id IS NOT NULL AND target_collection_id IS NULL)
        OR (target_work_id IS NULL AND target_collection_id IS NOT NULL)),
    CHECK (relation_type <> 'part_of_series' OR target_collection_id IS NOT NULL)
);

CREATE UNIQUE INDEX work_relations_work_target
    ON work_relations(work_id, relation_type, target_work_id)
    WHERE target_work_id IS NOT NULL;

CREATE UNIQUE INDEX work_relations_collection_target
    ON work_relations(work_id, relation_type, target_collection_id)
    WHERE target_collection_id IS NOT NULL;

CREATE INDEX work_relations_collection_order
    ON work_relations(target_collection_id, ordinal, work_id)
    WHERE target_collection_id IS NOT NULL;

CREATE TRIGGER work_relations_manual_authority
BEFORE UPDATE ON work_relations
WHEN (OLD.confirmed_by_user = 1 OR OLD.provenance = 'manual')
  AND NEW.provenance <> 'manual'
BEGIN
    SELECT RAISE(ABORT, 'automatic relationship refresh cannot replace manual authority');
END;
