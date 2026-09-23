# DATA_MODEL.md — Core Domain Model

Version: 0.2
Status: Approved for Phase 0 after independent audit

## 1. Purpose

Lernae needs a model that can represent the same creative property across different media without pretending they are identical.

The core hierarchy is:

```text
Universe → Work → Edition → Asset
```

These terms describe product concepts, not database-table names that can never change.

## 2. Universe

A user-facing grouping of related Works across media.

Examples:

- Evangelion
- Pokémon
- The Wheel of Time

Suggested fields:

- `id`
- `title`
- `sort_title`
- `description`
- `artwork`
- `favorite`
- `created_at`
- `updated_at`

Universe membership needs provenance/confidence because some relationships may be inferred or manually corrected.

## 3. Work

A distinct creative work.

Examples:

- `Soulcalibur II`
- `Neon Genesis Evangelion` (anime series)
- `The Eye of the World` (novel)

Work identity describes the creative work, not a file format or playback platform. To avoid conflating those concepts, Work has a broad `medium` plus a more useful `work_type`.

Suggested fields:

- `id`
- `universe_id` nullable
- `medium`
- `work_type`
- `title`
- `original_title`
- `release_date/year`
- `description`
- `artwork`
- `metadata_status`
- timestamps

Initial broad `medium` values may include:

- `game`
- `video`
- `literature`
- `audio`

`work_type` supplies the user-facing/domain subtype when needed, for example:

- `game`
- `movie`
- `tv_series`
- `anime_series`
- `novel`
- `manga`
- `comic`
- `album`
- `podcast`

This keeps `anime`, `movie`, `manga`, etc. useful to the product without incorrectly treating `audiobook` or `EPUB` as separate Works. Do not build behavior around a closed enum that can never evolve.

## 4. Edition

A consumable/released version of a Work. Edition carries platform or format distinctions.

Examples:

For `Soulcalibur II`:

- GameCube edition (`platform=gamecube`, `format=disc_image`)
- PlayStation 2 edition
- Xbox edition

For `The Eye of the World`:

- EPUB edition (`format=epub`)
- audiobook edition (`format=audiobook`)
- PDF/scanned edition where appropriate

An audiobook rendition is therefore an Edition of the literature Work unless product evidence later requires a separate Work relationship (for example, a dramatized adaptation).

For video, Edition may later capture resolution/cut/language distinctions only when useful. Avoid over-modeling early.

Suggested fields:

- `id`
- `work_id`
- `edition_label` nullable
- `platform` nullable
- `region` nullable
- `language` nullable
- `format`
- `preferred`
- timestamps

## 5. Asset

A logical launchable/preservable unit belonging to an Edition. An Asset may be backed by one file or by multiple related files.

Examples:

- one GameCube ISO;
- one EPUB;
- one MKV;
- a CUE/BIN image set;
- a multi-disc game bundle;
- a directory/game bundle.

Suggested fields:

- `id`
- `edition_id`
- `kind`
- `total_size_bytes`
- `content_hash` nullable (aggregate/manifest hash when meaningful)
- `integrity_algorithm` nullable
- `state`
- `created_at`
- `updated_at`

Location is not a single path on Asset. The same Asset may have multiple locations.

## 5.1 AssetPart

Represents the physical members of a multi-file Asset while keeping the Asset itself as the logical unit Lernae restores and launches. Single-file Assets simply have one part.

Suggested fields:

- `id`
- `asset_id`
- `part_index` integer nullable
- `role` string (`rom`, `disc_1`, `disc_2`, `cue`, `bin`, `data`, etc.)
- `filename`
- `relative_path` nullable
- `size_bytes`
- `content_hash` nullable
- `integrity_algorithm` nullable

The launcher must never guess boot order from filesystem ordering when explicit part/role metadata exists.

## 6. AssetLocation

Represents where an Asset currently exists. A location normally points to the root of the logical Asset; `AssetPart.relative_path` identifies files within that root for multi-file bundles.

Examples:

- local cache path;
- Drive/rclone remote key;
- NAS path.

Suggested fields:

- `id`
- `asset_id`
- `storage_provider_id`
- `locator`
- `location_class` (`local_cache`, `archive`, `library`, etc.)
- `verified_at`
- `exists_status`
- timestamps

A local-cache AssetLocation is created/marked ready only after staging verification and atomic promotion. A `.staging` path is never a valid launchable AssetLocation.

## 7. ExternalIdentity

Maps a Lernae entity to an external provider record.

Examples:

```text
Work Soulcalibur II
├── IGDB: 1234
└── RomM: 9876
```

Suggested fields:

- `id`
- `entity_type`
- `entity_id`
- `provider`
- `external_id`
- `external_url` optional
- `metadata_json` optional/minimal
- timestamps

Unique constraint should prevent duplicate provider/entity IDs where appropriate.

## 8. UniverseMembership

Links Work to Universe with provenance.

Suggested fields:

- `universe_id`
- `work_id`
- `source` (`provider`, `rule`, `manual`)
- `confidence` nullable
- `confirmed_by_user` boolean

Manual decisions should override automatic regrouping unless explicitly reset.

## 9. UserState

Personal state attached to a Work or Universe.

Separate universe-level and work-level state instead of overloading one ambiguous record.

Work state may include:

- status
- favorite
- rating
- started_at
- completed_at
- last_activity_at
- notes

Universe state may include:

- favorite
- followed
- notes

## 10. Progress

Progress needs a common envelope and medium-specific payload.

Suggested fields:

- `id`
- `work_id` / edition where needed
- `progress_kind`
- `progress_value_json`
- `source`
- `updated_at`

Examples:

TV/anime:

```json
{"season": 1, "episode": 19, "position_seconds": 841}
```

Manga:

```json
{"chapter": "131", "page": 12}
```

Ebook:

```json
{"percentage": 0.47, "locator": "reader-specific-locator"}
```

Game:

```json
{"playtime_seconds": 49320, "last_session_seconds": 3060}
```

Do not force all media into a single percentage.

## 11. Session

Represents a consumption session.

Suggested fields:

- `id`
- `work_id`
- `edition_id` nullable
- `agent_id` nullable
- `session_type`
- `started_at`
- `ended_at`
- `duration_seconds`
- `launch_provider`
- `exit_status` optional

Sessions drive history and playtime without requiring an external tracker.

## 12. List

User-defined collections.

Examples:

- Play this year
- Mecha
- Comfort movies

Suggested tables/concepts:

- `List`
- `ListItem`

ListItem should be capable of pointing to a Work or Universe initially.

## 13. ProviderConfig

Stores non-secret provider configuration and references to secrets.

Never place raw secrets into logs or user-visible diagnostics.

## 14. Job

Long-running background operation.

Suggested fields:

- `id`
- `kind`
- `entity_type`
- `entity_id`
- `status`
- `progress_current`
- `progress_total`
- `message`
- `error_code`
- `error_detail`
- timestamps
- `attempt_count`

Initial statuses:

- queued
- running
- waiting_on_agent
- interrupted
- succeeded
- failed
- cancelled

## 15. Agent

Represents a machine capable of local actions.

MVP only needs one Agent but should not hard-code that assumption into every table.

Suggested fields:

- `id`
- `name`
- `platform`
- `last_seen_at`
- `capabilities_json`

## 16. Design rules

- External IDs are references, not our primary keys.
- File paths are locations, not identity.
- User state survives provider removal.
- Asset availability can change without deleting the Work.
- Manual relation corrections must be preserved.
- Schema migrations must be versioned from day one.
