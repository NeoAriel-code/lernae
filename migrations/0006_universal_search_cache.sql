CREATE TABLE IF NOT EXISTS universal_search_cache (
    provider TEXT NOT NULL CHECK(length(provider) BETWEEN 1 AND 128),
    normalized_query TEXT NOT NULL CHECK(length(normalized_query) BETWEEN 1 AND 200),
    schema_version TEXT NOT NULL CHECK(length(schema_version) BETWEEN 1 AND 64),
    results_json TEXT NOT NULL,
    fetched_at_utc TEXT NOT NULL,
    expires_at_utc TEXT NOT NULL,
    PRIMARY KEY (provider, normalized_query, schema_version)
);

CREATE INDEX IF NOT EXISTS universal_search_cache_fetched_at_idx
    ON universal_search_cache(fetched_at_utc ASC, provider ASC, normalized_query ASC, schema_version ASC);

INSERT OR REPLACE INTO universal_search_cache (
    provider, normalized_query, schema_version, results_json, fetched_at_utc, expires_at_utc
)
SELECT provider, normalized_query, 'v1', results_json, fetched_at_utc, expires_at_utc
FROM metadata_search_cache;

DELETE FROM universal_search_cache
WHERE rowid NOT IN (
    SELECT rowid
    FROM universal_search_cache
    ORDER BY fetched_at_utc DESC, provider ASC, normalized_query ASC, schema_version ASC
    LIMIT 500
);

DELETE FROM metadata_search_cache;
