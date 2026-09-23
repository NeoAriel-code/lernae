# MVP.md — Lernae Minimum Viable Product

Version: 0.2

## 1. MVP definition

The MVP is not “all of Lernae with fewer features”.

It is the smallest implementation that proves the product's central promise end-to-end.

The first proof is the **Soulcalibur Vertical Slice**.

A vertical slice means one narrow workflow is implemented through every required layer: UI → metadata → inventory → storage → local agent → launcher → history.

## 2. Soulcalibur Test

### User story

As a user, I open Lernae, search for `Soul Calibur`, select `Soulcalibur II`, choose the GameCube edition and press **PLAY**. If my ROM is archived in my configured remote storage, Lernae restores it locally and launches Dolphin automatically. When I finish, Lernae records the session.

### Exact flow

```text
Lernae Home
  ↓ search "Soul Calibur"
Search API
  ↓
IGDB provider
  ↓
normalized results
  ↓
Soulcalibur II
  ↓ choose GameCube
InventorySource
  ↓
manifest baseline (RomM adapter may also provide same mapping)
  ↓
remote AssetLocation
  ↓ PLAY
Server creates Job: restore
  ↓ delegates over local UDS
Agent
  ↓
rclone copies remote Asset → <cache>/.staging/<job_id>/
  ↓ transfer exits successfully
verify expected byte size
  ↓
atomic rename staging → final cache path
  ↓
Server marks local AssetLocation ready
  ↓
Agent strict Dolphin runner
  ↓
Dolphin launches verified local Asset
  ↓
session starts
  ↓
session ends
  ↓
History updated
```

Server owns orchestration and durable Job state. Agent owns the physical local cache and transfer execution. A partial `.staging` file is never launchable.

## 3. MVP requirements

### UI

Must provide:

- Lernae identity;
- one search box;
- search results with artwork/title/year/platform information where available;
- Work details;
- GameCube edition selector for Soulcalibur II;
- PLAY button;
- preparation progress/status;
- clear failure state;
- basic History/Recent Activity entry after launch.

### Search

- IGDB is the first metadata provider.
- Search results are cached in Lernae database where appropriate.
- Provider failure must produce an understandable error.

### Catalog

- persist selected Work and Edition;
- persist IGDB external identity;
- map the user's existing Soulcalibur II asset to that Edition.

### Inventory

The **required baseline** is `ManifestInventorySource`, backed by the versioned local manifest defined in `docs/MANIFEST.md`. This makes the Soulcalibur proof deterministic, testable offline and independent of RomM API changes.

`RomMInventorySource` is the first real external inventory adapter and should implement the same capability interface. It may be added during Phase 1 once the manifest path proves the end-to-end flow, but RomM availability must not gate the first acceptance test.

The manifest is a development/bootstrap inventory source, not the intended final user experience.

### Storage

- one local cache path owned by Agent;
- one rclone remote configuration;
- Server creates/orchestrates restore Jobs;
- Agent executes rclone/local copy operations;
- every restore writes to `<cache>/.staging/<job_id>/` first;
- rclone/process success plus expected `size_bytes` validation is required for MVP;
- staging and final cache target must live on the same filesystem;
- successful content is promoted with atomic rename before it becomes launchable;
- no execution from remote mount;
- restore progress is reported to Server when available, but durable DB updates are throttled;
- minimum-free-space guard;
- cryptographic hashing may run later/asynchronously; it does not block the first MVP launch when transfer success + expected size match;
- no automatic eviction required in first slice unless trivial.

### Agent

- runs on the same Linux workstation for MVP;
- communicates with Server over a Unix Domain Socket (UDS) protected by user-only filesystem permissions;
- owns local staging/cache filesystem operations;
- executes delegated rclone restores;
- can confirm Dolphin availability;
- MVP uses one strict Dolphin runner rather than a generic launcher plugin framework;
- launches only the approved executable with structured process arguments equivalent to `dolphin-emu -b -e <verified-local-path>`;
- tracks child process start/end;
- never executes arbitrary shell commands supplied by metadata/UI.

### Recovery / resilience

- after Server restart, stale `running` / `waiting_on_agent` Jobs from the prior process become `interrupted`;
- Agent cleans abandoned staging directories for interrupted Jobs;
- existence of a partial file must never satisfy local-ready inventory;
- retrying PLAY after an interrupted restore starts from a safe state and must not launch partial content.

### History

After Dolphin exits, Lernae records:

- Work;
- Edition;
- launch time;
- end time;
- session duration.

No achievement or in-game completion tracking is required.

## 4. MVP acceptance test

The MVP passes when the following can be demonstrated from a normal user session without opening RomM, rclone or Dolphin manually:

1. Open Lernae.
2. Search `Soul Calibur`.
3. Select `Soulcalibur II`.
4. Select `GameCube`.
5. Press PLAY.
6. Lernae determines that the configured Asset is remote-only.
7. Lernae copies the full file to the local cache.
8. Lernae launches Dolphin with the correct file.
9. User closes Dolphin.
10. Lernae records the play session.
11. Repeating PLAY while the Asset is local launches without restoring again.
12. An intentionally interrupted restore leaves no launchable partial Asset; retry can safely restore and launch.
13. Restarting Server with a stale active Job reconciles it to `interrupted` instead of leaving PREPARING forever.

## 5. Explicit MVP exclusions

Not required for the Soulcalibur slice:

- automatic acquisition from internet sources;
- Radarr/Sonarr/Librarr;
- movies/TV/books/anime/manga;
- Universe grouping;
- favorites;
- external tracker synchronization;
- multi-user;
- multi-Agent device selection;
- NAS support;
- media streaming;
- automatic cache eviction;
- Windows/macOS;
- public internet exposure;
- mobile UI optimization beyond basic responsive behavior.

## 6. Second proof: Wheel of Time Test

After Soulcalibur works, add universal search across at least:

- books;
- TV.

Searching `The Wheel of Time` must make both media categories visible without requiring the user to choose a provider first.

This proves cross-media search, not acquisition/playback of every result.

## 7. Third proof: Evangelion Test

Search `Evangelion` and group related results across at least three available media categories into a user-facing Universe.

This proves the Resolver/Universe concept.

Manual confirmation is acceptable in the first implementation when automatic grouping is uncertain.
