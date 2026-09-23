# PRODUCT.md — Lernae Product Definition

Version: 0.1
Status: Foundation

## 1. Product statement

**Lernae is the place from which you search, obtain, consume and track all of your digital entertainment.**

The user should not need to know whether a file lives on local storage, Google Drive, a NAS or another configured backend, nor whether RomM, Jellyfin, Kavita, an *arr service, an emulator or another application is involved underneath.

The fundamental interaction is:

1. Search for something.
2. Choose the work, medium or edition.
3. Press **PLAY**, **WATCH**, **READ** or **LISTEN**.
4. Lernae handles the rest.

## 2. North Star

> Search anything. Choose the version you want. Enjoy it.

A feature belongs in Lernae when it reduces the distance between **intent** and **consumption**, or helps the user retain meaningful state around that consumption.

## 3. What Lernae is not

Lernae is not:

- a replacement for every specialist media server;
- a torrent client;
- a piracy catalog;
- an emulator;
- a NAS operating system;
- a dashboard containing links to twenty self-hosted apps;
- a metadata source of record for the whole internet.

Specialized applications remain replaceable providers behind a stable Lernae experience.

## 4. Primary user experience

### Home

The default page should prioritize:

- Universal Search
- Continue
- Favorite Universes
- Recently Added
- Up Next / new releases where relevant
- Recent activity only when useful

Infrastructure is not homepage content.

### Universal Search

One search box must be able to return multiple media types.

Example: `Evangelion`

- Anime
- Movies
- Manga
- Books / novels when available
- Games

Example: `The Wheel of Time`

- Book series
- TV series

The interface should make media type obvious without forcing the user to know which provider supplied the result.

### Universe

A **Universe** groups related works across media.

Example:

```text
Evangelion
├── Anime
├── Movies
├── Manga
└── Games
```

A user can favorite an entire Universe independently from favoriting one Work inside it.

Universe grouping may be imperfect initially. Lernae must prefer transparent, correct grouping over aggressive guesses.

### Library

The Library shows content the user owns or has imported, regardless of physical location.

The primary state presented to the user should be human-readable:

- Ready
- Archived
- Preparing
- Getting
- Unavailable
- Error

Terms such as hydrate, VFS, remote mount and provider job belong in advanced/system views.

### Activity

Activity is the place for operational detail:

- acquisition progress;
- local preparation;
- archive upload;
- storage restore;
- launch sessions;
- provider errors.

The normal user flow should not require opening Activity.

### System

The current Homepage-style dashboard belongs here.

System can expose:

- service status;
- CPU/RAM/storage;
- RomM/Kavita/other provider links;
- Google Drive/rclone status;
- Tailscale/network status;
- logs and diagnostics.

System is for maintenance, not everyday consumption.

## 5. Human actions

Lernae should use verbs that reflect user intent:

- PLAY
- WATCH
- READ
- LISTEN
- CONTINUE

Avoid exposing internal workflow actions in the primary UX:

- Download
- Import
- Scan
- Hydrate
- Mount
- Sync

If preparation is required, the intent button can temporarily become `PREPARING 42%` and launch automatically when ready.

## 6. Personal Media State

Lernae owns a local record of the user's relationship with media.

Core state includes:

- status: planned / in progress / completed / paused / abandoned;
- favorite Work;
- favorite Universe;
- rating;
- private notes;
- lists;
- started date;
- completion date;
- last activity;
- session history;
- medium-specific progress.

Progress is media-aware:

- TV/anime: episode + playback position;
- film: playback position;
- manga/comic: chapter + page where available;
- ebook: locator/page/percentage according to reader capabilities;
- audiobook: timestamp;
- game: sessions, playtime, last played, optional achievements/save metadata. Do not invent a completion percentage when no trustworthy signal exists.

Lernae should remain the local source of truth. External trackers are synchronization targets/sources through connectors.

## 7. External synchronization

Future connectors may synchronize with services such as AniList, MyAnimeList, Trakt, Goodreads-like book services, Steam and others where APIs/terms permit.

Rules:

- external services are optional;
- Lernae remains usable without cloud accounts;
- sync conflicts must be visible and recoverable;
- connectors must respect provider terms and rate limits;
- do not scrape or automate around a provider's restrictions when an approved interface is required.

## 8. Storage independence

Lernae must not assume Google Drive.

Storage is abstracted through providers. Long-term targets include:

- local filesystem;
- rclone-compatible remotes;
- NAS shares such as SMB/NFS;
- object storage where appropriate.

For the initial development environment, Google Drive via rclone is the first remote-storage implementation.

## 9. Product quality principles

1. **Intent over infrastructure** — show what the user wants to do, not which daemon does it.
2. **One front door** — normal use happens in Lernae.
3. **Specialists underneath** — reuse mature software instead of rebuilding it without reason.
4. **Local-first personal state** — favorites, history and progress should survive provider changes.
5. **Replaceable providers** — no service should become impossible to swap out.
6. **Safe automation** — background operations need clear failure states and recovery.
7. **Fast perceived response** — show a meaningful result immediately, even if preparation continues.
8. **No fake intelligence** — if relationships/progress are uncertain, say so rather than guess.
9. **Self-hosting first** — a capable local deployment is the core product.
10. **Simple installation eventually** — public success depends on onboarding, not only architecture.

## 10. Out of scope for the first MVP

- native mobile apps;
- smart TV apps;
- multiple playback devices;
- recommendation engine;
- AI features;
- social graph;
- universal achievements;
- plugin marketplace UI;
- Windows/macOS support guarantees;
- custom ebook/video players;
- comprehensive external tracker synchronization;
- advanced multi-user permissions.

These may exist in the long-term vision but cannot block the first vertical slice.
