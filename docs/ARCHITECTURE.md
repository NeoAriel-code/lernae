# ARCHITECTURE.md — Lernae Foundation Architecture

Version: 0.2
Status: Approved for Phase 0 after independent audit

## 1. Architecture goal

Lernae must present one stable product while delegating specialist work to replaceable external systems.

The first implementation targets a single Linux machine running CachyOS/Omarchy, while preserving a path toward ordinary Docker-based self-hosting.

## 2. Major components

```text
                    ┌────────────────────┐
                    │     Lernae Web     │
                    │   React/TypeScript │
                    └─────────┬──────────┘
                              │ REST/events
                    ┌─────────▼──────────┐
                    │   Lernae Server    │
                    │        Go          │
                    ├────────────────────┤
                    │ Search             │
                    │ Resolver           │
                    │ Catalog            │
                    │ Inventory          │
                    │ Acquisition Router │
                    │ Personal State     │
                    │ Job Orchestrator   │
                    │ Provider Registry  │
                    └───────┬────────────┘
                            │ local authenticated API
                    ┌───────▼────────────┐
                    │   Lernae Agent     │
                    │        Go          │
                    ├────────────────────┤
                    │ Cache              │
                    │ Local filesystem   │
                    │ Restore/archive    │
                    │ Launch processes   │
                    │ Session tracking   │
                    └───────┬────────────┘
                            │
          ┌─────────────────┼────────────────────┐
          │                 │                    │
       Dolphin           RetroArch             rclone
          │                                      │
        local                                 remote
```

### Lernae Web

The browser UI. It contains no provider-specific business logic beyond presentation.

### Lernae Server

The central application service. It owns catalog identity, personal state, orchestration, provider configuration and durable Job state.

The Server **plans and delegates** machine-local work. It does not assume that its own filesystem is the playback machine's filesystem. In particular, it does not restore a remote game into a Server-local path and then hand that path to another machine.

### Lernae Agent

A trusted process installed on a machine capable of launching local applications. It owns that machine's local cache and executes machine-local work that a browser/server container should not perform directly, such as:

- invoking rclone/local copy operations delegated by Server;
- writing into local staging/cache paths;
- verifying transfer completion;
- atomically promoting staged content into the ready cache;
- starting Dolphin or another approved runtime;
- tracking the lifetime of launched processes.

For MVP, Server and Agent run on the same Linux workstation, but their boundary remains explicit so later self-hosted topologies do not require a rewrite.

## 3. Why Server + Agent

A remote/self-hosted web server cannot safely assume it can execute GUI programs on the user's desktop.

The Agent gives us a controlled interface for:

- launching applications;
- checking local file availability;
- maintaining local cache;
- restoring archived assets;
- reporting sessions;
- later supporting more than one playback device.

MVP supports one Agent only.

## 4. Provider architecture

A **provider** is an adapter that translates between Lernae concepts and an external system.

The categories below are **capabilities, not mutually exclusive provider classes**. One concrete integration may implement several capabilities. For example, a Jellyfin integration could provide inventory plus metadata, while RomM could provide inventory plus metadata mappings. Core depends on small capability interfaces rather than a single monolithic `Provider` type.

Provider capabilities:

### Metadata providers

Examples: IGDB, TMDB, AniList, Open Library.

Responsibilities:

- search;
- retrieve canonical metadata;
- provide external IDs/relations when available.

### Inventory providers

Examples: RomM, Jellyfin, Kavita or local filesystem scanners.

Responsibilities:

- tell Lernae what the user already has;
- map external library entries to Lernae Works/Editions/Assets.

### Acquisition providers

Examples: Radarr, Sonarr, book/media managers, storefront integrations or other user-configured lawful acquisition systems.

Responsibilities:

- accept a request for an Edition/Work;
- expose job status;
- report resulting assets.

Core does not encode source-specific acquisition behavior.

### Storage capabilities

Examples: local filesystem, rclone remote, NAS share.

Storage is intentionally split across orchestration and execution:

**Server-side responsibilities**

- store provider configuration and stable remote locators;
- know expected size/integrity metadata when available;
- decide that a restore/archive Job is required;
- delegate the Job to the selected Agent;
- persist durable Job state.

**Agent-side responsibilities**

- execute the physical transfer on the playback machine;
- own local staging/cache paths;
- report progress back to Server;
- verify transfer completion;
- atomically promote a completed staged Asset into the ready cache.

This prevents a Server running on a NAS/container from restoring an Asset onto the wrong filesystem.

### Launcher providers

Examples: Dolphin, RetroArch, PCSX2, mpv, Foliate, Heroic.

Responsibilities:

- determine whether the runtime is available;
- construct a safe launch request;
- launch an Edition/Asset;
- report session start/end where possible.

### Tracker connectors

Examples: AniList/Trakt-like services.

Responsibilities:

- map Lernae state to/from an external tracker;
- respect provider rate limits and conflict rules.

Not required for MVP.

## 5. Core services

### Search

Runs one user query across configured metadata providers, normalizes results and returns early partial results when appropriate.

### Resolver

Connects related external records to Lernae entities.

Resolver must retain confidence/provenance. It must not silently merge uncertain records.

### Catalog

Owns persisted Universes, Works, Editions, Assets and external IDs.

### Inventory

Answers:

- do we own/have this Edition?
- where is the Asset?
- is it local and launchable?
- is it remote/archive-only?

### Acquisition Router

Chooses a configured acquisition provider capable of satisfying a user request.

### Job Orchestrator

Tracks long-running operations such as:

- acquisition;
- restore;
- verify;
- archive;
- provider scans.

A Job must have a stable ID, status, progress when available, error information and timestamps.

For machine-local operations, Server creates/orchestrates the Job and Agent executes it. Progress may be streamed to the UI frequently, but database persistence must be throttled so transfer ticks do not generate excessive SQLite writes.

### Personal State

Owns favorites, Universe favorites, history, sessions, progress, ratings, notes and lists.

## 6. Asset lifecycle

Initial lifecycle:

```text
UNKNOWN
  ↓
AVAILABLE          known metadata, not currently owned
  ↓ acquire
ACQUIRING
  ↓
LOCAL_STAGING      bytes exist only in isolated staging
  ↓ verify
LOCAL_READY        valid local asset promoted atomically
  ↓ archive copy
LOCAL_ARCHIVED     local + remote copy
  ↓ local eviction
REMOTE_ONLY
  ↓ restore request
RESTORING
  ↓ Agent writes to staging
LOCAL_STAGING
  ↓ verify + atomic rename
LOCAL_ARCHIVED     local + remote copy
```

`LOCAL_STAGING` is never launchable. A partially transferred file must never appear at the final cache path.

For MVP, every restore follows this rule:

1. Server creates a restore Job.
2. Server delegates it to Agent.
3. Agent writes into `<cache>/.staging/<job_id>/` on the **same filesystem** as the final cache target.
4. Agent requires the transfer process to exit successfully and validates expected byte size when known.
5. Agent promotes the staged Asset with an atomic filesystem rename.
6. Only after that rename may Lernae create/mark a launchable local-cache location.

Keeping staging and final cache on the same filesystem is required because atomic rename guarantees do not apply across filesystems.

If transfer, process or machine failure occurs, the existing known-good remote location remains valid and the partial staging data remains non-launchable. Startup reconciliation cleans abandoned staging directories associated with interrupted Jobs.

For newly acquired content, prefer:

```text
acquisition → local staging → verify → atomic promote → usable immediately
                                              └→ background archive
```

Do not download a new Asset to remote storage only to immediately download it again for local use.

## 7. Remote-storage rule

Heavy workloads that require random access must not execute directly against Google Drive/rclone mounts.

Initial policy:

- games: complete local copy before launch;
- local-AI models: not part of MVP; if ever supported, complete local copy before inference;
- small books/ROMs: complete local copy before consumption when practical;
- sequential media streaming may use a deliberate streaming/VFS path in a later phase.

The goal is to prevent one workload from exhausting remote API requests and harming other libraries.

## 8. API style

Initial recommendation: HTTP REST for commands/resources plus server-sent events or WebSocket only where live progress genuinely benefits from push updates.

Prefer boring, inspectable interfaces over clever distributed infrastructure.

Examples conceptually:

```text
GET  /api/search?q=soul+calibur
GET  /api/works/{id}
POST /api/editions/{id}/play
GET  /api/jobs/{id}
GET  /api/home
POST /api/universes/{id}/favorite
```

Exact API design belongs to implementation specs, not Foundation.

## 9. Persistence

SQLite is the initial database.

Required connection policy from day one:

```text
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
```

Writes must be serialized through a shared write-capable database path/pool. Long-running transfer progress must not persist every raw progress tick; persist on a sensible interval or meaningful delta while live UI progress may remain more frequent in memory/events.

Why:

- single-user/small-household self-hosting does not require an external database server;
- backups are straightforward;
- development is simple;
- WAL permits readers to coexist better with background writes;
- a busy timeout avoids turning short lock contention into immediate HTTP 500 errors;
- foreign-key enforcement catches relationship corruption early.

Do not introduce PostgreSQL, Redis, Kafka or a message broker until measured requirements justify them.

## 10. Concurrency

Long operations must not block HTTP requests.

The Server needs an internal job queue backed by durable database state. MVP may execute orchestration in-process; the design must reconcile durable Job rows after restart.

On Server startup:

- active transient states such as `running` or `waiting_on_agent` from the previous process are marked `interrupted`;
- the associated Asset must not be treated as local-ready unless a final promoted cache path exists and passes validation;
- Agent cleans stale `.staging/<job_id>` data for interrupted Jobs;
- queued work may be resumed only when its operation is explicitly defined as resumable/idempotent.

Do not introduce a separate queue product in MVP.

## 11. Security baseline

MVP Web/Server may initially bind to localhost during development.

On Linux MVP, **Server ↔ Agent IPC uses a Unix Domain Socket (UDS)** with user-only filesystem permissions (`0600` on the socket or enclosing permission model that provides equivalent protection). A UDS is a local process-to-process communication channel represented by a filesystem socket; it avoids exposing an Agent HTTP port to browsers or the LAN.

A later remote/multi-device Agent transport requires a separate ADR and authenticated network protocol. Do not expose the MVP Agent as an unauthenticated localhost HTTP service.

Public-alpha requirements later include:

- authentication;
- CSRF/session protections as appropriate;
- Agent authentication;
- secret storage strategy;
- least-privilege provider credentials;
- no shell-command construction from unsanitized metadata;
- explicit allowed launcher executables;
- path validation against traversal.

Agent launch requests must use structured arguments rather than arbitrary shell strings.

## 12. Repository shape

```text
lernae/
├── apps/
│   └── web/
├── cmd/
│   ├── server/
│   └── agent/
├── internal/
│   ├── catalog/
│   ├── search/
│   ├── resolver/
│   ├── inventory/
│   ├── acquisition/
│   ├── storage/        # server-side storage intent/metadata contracts
│   ├── agentops/       # Agent-side transfer/cache execution contracts
│   ├── launcher/
│   ├── tracking/
│   ├── jobs/
│   └── providers/
├── migrations/
├── docs/
├── deploy/
├── tests/
├── AGENTS.md
└── docker-compose.yml
```

This is a monorepo: one repository contains Web, Server, Agent and docs so changes can be coordinated atomically.

## 13. Observability

For development, every important operation should expose understandable logs with a correlation/job ID.

Example:

```text
job=abc123 work=soulcalibur2 edition=gamecube action=restore status=started
```

A user-facing Activity page later translates those events into plain language.

## 14. Extensibility rule

Core business concepts must not import concrete provider packages directly.

Core depends on small capability interfaces/contracts. A concrete external integration may implement one or many of them; it must not be forced into a single exclusive provider category. Shared clients/configuration may be reused inside one integration without duplicating adapters.

Universe resolution is deliberately simple/manual/rule-assisted until Phase 3. Do not attempt broad automatic cross-media entity clustering during Foundation or the Soulcalibur slice.

This is the most important architecture rule for long-term maintainability.
