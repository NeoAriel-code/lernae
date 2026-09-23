# ROADMAP.md — Lernae Delivery Roadmap

Version: 0.2

This roadmap orders learning and risk reduction. Dates are deliberately omitted until the Foundation audit and first implementation estimates exist.

## Phase 0 — Foundation

Goal: create a repository agents can work in without inventing the product each turn.

Deliverables:

- product specification;
- architecture specification;
- domain/data model;
- MVP specification;
- ADRs;
- agent rules;
- skeleton repository;
- basic CI;
- lint/test commands;
- development environment documentation.

Exit condition: a new coding agent can explain the product, architecture boundaries and current task after reading repository docs only.

## Phase 1 — Soulcalibur Vertical Slice

Goal: prove Search → Inventory → Remote Restore → Launch → History.

Deliverables:

- Web search UI;
- Go Server;
- SQLite schema/migrations;
- IGDB metadata provider;
- `ManifestInventorySource` baseline and schema;
- RomM inventory adapter after the baseline path is proven;
- Agent-side rclone restore executor with atomic staging;
- Agent + Linux UDS transport;
- strict Dolphin runner;
- Job progress;
- session history.

Exit condition: `Soulcalibur II / GameCube / PLAY` passes `docs/MVP.md` acceptance test.

## Phase 2 — Universal Search

Goal: prove one search box can span unrelated media domains.

Initial providers:

- games metadata;
- books metadata;
- TV/film or anime metadata.

Deliverables:

- normalized search-result model;
- provider result merging;
- category presentation;
- provider caching/rate-limit handling;
- Wheel of Time Test.

Exit condition: one query can naturally expose at least book + TV results.

## Phase 3 — Universe Resolver

Goal: group related works across media.

Deliverables:

- Universe entities;
- Work-to-Universe relationships;
- provenance/confidence;
- manual correction UI;
- favorite Universe groundwork;
- Evangelion Test.

Exit condition: Evangelion appears as one Universe with multiple media categories and no obviously incorrect merges.

## Phase 4 — Acquisition Orchestration

Goal: a Work not currently owned can be requested from a configured lawful/user-managed acquisition provider without leaving Lernae.

Deliverables:

- acquisition-provider contract;
- at least one mature media acquisition integration;
- Job lifecycle;
- local staging;
- verify → use → archive flow;
- Activity UI.

Important: Core orchestrates providers and does not embed source-specific piracy behavior.

## Phase 5 — Personal Media State

Goal: Lernae becomes useful every day, not merely as a launcher.

Deliverables:

- Continue;
- history;
- medium-aware progress;
- Work favorites;
- Universe favorites;
- statuses;
- ratings;
- notes;
- lists;
- recent activity.

## Phase 6 — Consumption Backends

Goal: expand actual READ/WATCH/LISTEN coverage.

Possible integrations, chosen by maturity/need:

- Jellyfin-like video provider;
- Kavita/alternative reading provider;
- audiobook/music provider;
- additional emulators/PC game launchers.

Do not integrate every service just because it exists.

## Phase 7 — External State Sync

Goal: optional synchronization with third-party tracking services.

Deliverables may include:

- AniList-like connector;
- Trakt-like connector;
- conflict resolution;
- import/export;
- per-connector sync settings.

## Phase 8 — Public Alpha

Goal: another self-hoster can install Lernae without the original developer's personal environment.

Deliverables:

- supported Docker deployment;
- onboarding wizard;
- provider setup UI;
- backup/restore docs;
- authentication;
- diagnostics;
- sane sample configuration;
- migration policy;
- security review;
- public issue templates.

Exit condition: a technically competent self-hoster can install from documentation without private instructions.

## Future vision — intentionally unordered

- multiple Agents/devices;
- mobile companion;
- TV client;
- smart prefetch;
- recommendations;
- annual statistics;
- plugin/provider SDK ecosystem;
- NAS-specific packaging;
- optional hosted convenience service such as secure remote access and encrypted configuration backup;
- additional media types.

Future items do not enter active development until the current phase's exit condition is met.
