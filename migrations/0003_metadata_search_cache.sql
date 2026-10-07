CREATE TABLE metadata_search_cache (
    provider TEXT NOT NULL CHECK (length(trim(provider)) > 0),
    normalized_query TEXT NOT NULL CHECK (length(trim(normalized_query)) > 0),
    results_json TEXT NOT NULL,
    fetched_at_utc TEXT NOT NULL,
    expires_at_utc TEXT NOT NULL,
    PRIMARY KEY (provider, normalized_query)
);
