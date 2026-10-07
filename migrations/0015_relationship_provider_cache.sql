-- Parsed, allowlisted relationship metadata has a separate cache from search
-- results. The normalized JSON contains labels, aliases, and typed claims only.
CREATE TABLE relationship_provider_cache (
    provider TEXT NOT NULL CHECK (length(trim(provider)) BETWEEN 1 AND 64),
    external_id TEXT NOT NULL CHECK (length(trim(external_id)) BETWEEN 1 AND 128),
    entity_revision TEXT NOT NULL CHECK (length(trim(entity_revision)) BETWEEN 1 AND 128),
    parser_version TEXT NOT NULL CHECK (length(trim(parser_version)) BETWEEN 1 AND 64),
    fetched_at_utc TEXT NOT NULL,
    expires_at_utc TEXT NOT NULL,
    normalized_json TEXT NOT NULL CHECK (length(normalized_json) BETWEEN 2 AND 1048576),
    PRIMARY KEY (provider, external_id)
);

CREATE INDEX relationship_provider_cache_expiry
    ON relationship_provider_cache(expires_at_utc);
