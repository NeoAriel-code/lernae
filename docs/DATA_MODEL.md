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

Universe membership is a cross-media umbrella relationship; it does not imply
that its Works are ordered or belong to one Series. A Work can belong to more
than one ordered grouping, so Series membership is stored as a separate typed
Work relation.

## 8.1 WorkCollection and WorkRelation

`WorkCollection` provides a durable identity for a grouping such as a Series.
`WorkRelation` links one source Work to exactly one target Work or one
WorkCollection. `part_of_series` targets a WorkCollection; relationship types
such as `adaptation_of`, `sequel_of`, `follows`, `spin_off_of`, `companion_to`,
and `franchise_member` describe explicit edges. Titles are not evidence for
creating these edges.

Suggested WorkRelation fields:

- `work_id`
- `relation_type`
- `target_work_id` or `target_collection_id` (exactly one)
- `ordinal` nullable text, so provider sequence values such as `5.0` are not
  coerced or lost by integer parsing
- `provenance`, `provider_version`, `confidence`, and bounded `evidence`
- `confirmed_by_user`

Automatic provider refreshes may update automatic relations, but cannot replace
a manually confirmed relation. The relation's target kind and type are stored
independently of title text and relevance ranking.

Work Details may include one local Series detail record for each explicit
`part_of_series` relation found through the requested exact `(provider,
external_id)` Work identity. Each record preserves the opaque local collection
ID and title, nullable ordinal and known total, plus a bounded list of other
Works in that same Series. The total counts distinct persisted Series members
(including the current Work), not the capped peer list. Peers are ordered by
explicit ordinal before unknown ordinals, then by stable provider/external ID;
same-author Works and other relation types are not Series members. Each Work
is capped at 20 Series records and each peer list at 20 entries, with explicit
truncation flags.

## 8.2 UniverseAlias

`UniverseAlias` is a localized alternate label for an existing Universe, not a
relationship between Works or collections. Its normalized value collapses
surrounding and repeated whitespace while preserving localized spelling and
diacritics. `language_code` is nullable when the language is unknown. Store
provenance, optional provider version, confidence, and `confirmed_by_user`
separately; an automatic refresh cannot replace a manually confirmed alias.
The same normalized text may be stored for distinct language codes.
Universe Details exposes the persisted alias value, nullable language,
allowlisted provenance, safe confidence, and confirmation state on the same
opaque Universe ID. The response is bounded to 100 aliases and reports
truncation; series/collection labels remain separate from umbrella aliases.

## 8.3 Parsed relationship provider cache

`relationship_provider_cache` is separate from Search-result caching. It is
keyed by provider and exact external entity ID, and stores the provider's
entity revision, parser version, fetch time, expiry, and bounded normalized
JSON containing localized labels, aliases, allowlisted parsed claims, and
provider identifiers only. Parser version `wikidata-relationship-v3` makes
previous snapshots miss until they are refreshed.
Provider response bodies and arbitrary fields are not durable data. A cache
entry is a miss when its TTL expires or its parser version differs.

Migration 0016 adds `work_collection_external_identities`, keyed uniquely by
`(provider, external_id)`, without changing existing collection or relation
rows. A QID-backed WorkCollection keeps a generated opaque local collection
ID; repeated create-or-get calls reuse that ID and do not replace its title or
type with provider refresh metadata.

The initial reader uses Wikidata's [MediaWiki Action API](https://www.mediawiki.org/wiki/Wikibase/API)
at `https://www.wikidata.org/w/api.php`. `wbgetentities` reads one exact QID;
`wbsearchentities` returns candidates only and never chooses or merges an
ambiguous result. Parsed properties are limited to P179 (series membership),
P1545 (a textual ordinal qualifier when explicitly attached to P179), P527
(part candidates that remain suggestions), P155 (an explicit `follows` edge
with no ordinal), and P144 (an explicit `adaptation_of` edge only when its
target is an exact local Work). P155 never establishes canonical order. This
provider cache is input to explicit relationship resolution; it does not
itself create a WorkRelation, UniverseAlias, or Search-ranking change.

Wikidata entity identifiers are separately parsed through a strict
provider-ID allowlist: P8600 is a positive decimal TVMaze ID; P648 is an Open
Library Work ID matching `OL[1-9][0-9]*W`; and numeric P9043 is accepted only
as a qualifier of P5794, then mapped to the exact IGDB decimal ID. A P5794
slug, an Open Library Edition/Author ID, and other properties are never local
identity evidence. Local Work lookup remains exact `(provider, external_id)`;
zero matches or identifiers pointing to multiple distinct local Works fail
closed. `wbsearchentities` labels and titles remain candidates, never identity.
These Wikidata identifiers may be absent or incomplete, so absence means
unresolved rather than non-membership.

Each exact-entity lookup issues one request and reads one entity; candidate
search returns at most ten suggestions. Both operations bound responses to
1 MiB, parse at most 256 claims for an entity, use a five-second default
timeout (30-second hard maximum), and share a process-wide maximum of three
concurrent Wikidata requests. The optional Server environment setting
`LERNAE_WIKIDATA_CONTACT_EMAIL` is passed to the client as its contact-bearing
User-Agent identity. With no valid contact value, local functionality and
startup remain available, but the relationship client is not injected.

Graph expansion is a separate, explicit `POST /api/v1/universes/relationships/resolve`
operation. Its request supplies exactly one existing Universe ID or an exact
normalized local title/alias (and optional language). Search results and
per-card rendering never trigger it. The exact local anchor must select exactly
one Wikidata candidate with a matching normalized label; zero or multiple
candidates fail closed. Provider labels are still only candidates: local Work
identity is resolved exclusively through the allowlisted exact provider-ID
crosswalk. One resolution is capped at depth 2, 12 distinct entity nodes, 32
claims/edges, and 8 provider requests including the candidate lookup. Parsed
entity claims are cached with the parser version and TTL.

P179 plus its P1545 text qualifier persists an exact-QID-backed
`part_of_series` relation to a distinct WorkCollection. P155 persists only an
explicit `follows` Work edge, with no ordinal, and P144 persists an explicit
`adaptation_of` Work edge only after exact identity resolution. P527 remains a
review-needed Universe suggestion and never creates a strong membership by
itself. Provider refresh cannot replace manually confirmed relations, localized
Universe aliases, accepted Universe memberships, or exclusions. Only localized
labels/aliases on the selected umbrella Universe entity are stored as
UniverseAliases on the existing Universe ID; a separate series collection's
localized label is not promoted to an umbrella alias.

## 8.4 TVMaze Details season summary

The TVMaze provider exposes a Details-scoped method that makes one
`GET /shows/:id/seasons` request and returns only the number of known season
records in that response. A TV Work Details request makes one additional
bounded seasons request; Search and cards do not call it. A malformed or
unavailable optional season response leaves otherwise-valid Work Details
intact, and a valid empty season array reports zero known seasons. It does not
fetch episode lists or expose `episodeOrder` as an aired or total episode
count. No complete episode total is available from this bounded helper, so the
API intentionally has no episode-count field.

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
