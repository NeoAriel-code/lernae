-- QID/provider identity is an external crosswalk, not a local collection ID.
-- Keep existing collections and relationships untouched while adding a unique
-- provider/external-ID lookup for future QID-backed collections.
CREATE TABLE work_collection_external_identities (
    provider TEXT NOT NULL CHECK (length(trim(provider)) BETWEEN 1 AND 64),
    external_id TEXT NOT NULL CHECK (length(trim(external_id)) BETWEEN 1 AND 128),
    collection_id TEXT NOT NULL REFERENCES work_collections(id) ON DELETE CASCADE,
    PRIMARY KEY (provider, external_id)
);

CREATE INDEX work_collection_external_identities_collection
    ON work_collection_external_identities(collection_id);
