CREATE TABLE works (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    medium TEXT NOT NULL CHECK (length(trim(medium)) > 0),
    work_type TEXT NOT NULL CHECK (length(trim(work_type)) > 0),
    title TEXT NOT NULL CHECK (length(trim(title)) > 0)
);

CREATE TABLE editions (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    work_id TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    label TEXT,
    platform TEXT,
    region TEXT,
    format TEXT NOT NULL CHECK (length(trim(format)) > 0),
    CHECK (label IS NULL OR length(trim(label)) > 0),
    CHECK (platform IS NULL OR length(trim(platform)) > 0),
    CHECK (region IS NULL OR length(trim(region)) > 0)
);

CREATE UNIQUE INDEX editions_natural_key
    ON editions(work_id, COALESCE(platform, ''), format, COALESCE(region, ''), COALESCE(label, ''));
CREATE INDEX editions_work_id ON editions(work_id, id);

CREATE TABLE assets (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    edition_id TEXT NOT NULL REFERENCES editions(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (length(trim(kind)) > 0),
    total_size_bytes INTEGER NOT NULL CHECK (total_size_bytes >= 0)
);

CREATE INDEX assets_edition_id ON assets(edition_id, id);

CREATE TABLE asset_parts (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    part_index INTEGER CHECK (part_index IS NULL OR part_index > 0),
    role TEXT NOT NULL CHECK (length(trim(role)) > 0),
    filename TEXT NOT NULL CHECK (length(trim(filename)) > 0),
    relative_path TEXT,
    size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
    CHECK (relative_path IS NULL OR length(trim(relative_path)) > 0)
);

CREATE UNIQUE INDEX asset_parts_asset_index
    ON asset_parts(asset_id, part_index) WHERE part_index IS NOT NULL;
CREATE INDEX asset_parts_asset_role ON asset_parts(asset_id, role, part_index);
CREATE INDEX asset_parts_asset_order ON asset_parts(asset_id, part_index, id);

CREATE TABLE asset_locations (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    storage_provider_id TEXT NOT NULL CHECK (length(trim(storage_provider_id)) > 0),
    locator TEXT NOT NULL CHECK (length(trim(locator)) > 0),
    location_class TEXT NOT NULL CHECK (location_class IN ('local_cache', 'archive', 'library')),
    CHECK (
        location_class <> 'local_cache'
        OR INSTR('/' || REPLACE(locator, char(92), '/') || '/', '/.staging/') = 0
    ),
    UNIQUE(asset_id, storage_provider_id, locator, location_class)
);

CREATE INDEX asset_locations_asset_id ON asset_locations(asset_id, id);

CREATE TABLE external_identities (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(trim(id)) > 0),
    work_id TEXT NOT NULL REFERENCES works(id) ON DELETE CASCADE,
    provider TEXT NOT NULL CHECK (length(trim(provider)) > 0),
    external_id TEXT NOT NULL CHECK (length(trim(external_id)) > 0),
    UNIQUE(work_id, provider),
    UNIQUE(provider, external_id)
);
