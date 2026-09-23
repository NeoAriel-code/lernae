# ADR-002 — SQLite as Initial Database

Status: Accepted

## Context

Lernae targets self-hosting and initially a single user/small household. Requiring a separate database server increases installation and maintenance cost. The independent Foundation audit identified a realistic risk of background Job progress updates contending with ordinary writes.

## Decision

Use SQLite with versioned migrations and these baseline connection settings:

```text
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
```

Serialize writes through a shared write-capable database path/pool. Do not persist every raw transfer-progress tick; throttle durable progress updates while allowing more frequent in-memory/event updates for the UI.

## Why

- transactional and mature;
- easy backups;
- zero extra service for the user;
- WAL improves reader/writer coexistence for our expected load;
- busy timeout handles short lock contention without immediate user-facing errors;
- foreign-key enforcement protects relational integrity;
- sufficient for expected initial concurrency.

## Consequences

- write patterns must remain sensible;
- background jobs must use transactions carefully;
- startup Job reconciliation is required because durable rows may outlive in-process workers;
- if future scale proves SQLite insufficient, migration requires a new ADR and evidence rather than speculation.
