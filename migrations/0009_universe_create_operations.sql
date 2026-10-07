-- Opaque client-generated operation IDs make create retries safe without
-- conflating Universes that happen to share a display title.
CREATE TABLE universe_create_operations (
    operation_id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(operation_id)) BETWEEN 1 AND 128),
    request_digest TEXT NOT NULL CHECK (length(request_digest) = 64),
    -- Keep this opaque result ID after Universe deletion so retries cannot
    -- silently create a replacement under an already-consumed operation key.
    universe_id TEXT NOT NULL,
    created_at_utc TEXT NOT NULL
);

CREATE INDEX universe_create_operations_universe_id
    ON universe_create_operations(universe_id);
