# Lernae

> One library. Your universe.

Lernae is a self-hosted personal library layer that brings games, models, media and books together while keeping heavy assets where they belong until they're needed.

---

## Current Stack

The current running infrastructure orchestrates the following services:

| Service | Container | Port | Role | Storage / Mount |
| :--- | :--- | :--- | :--- | :--- |
| **Homepage** | `lernae-homepage` | `3000` | Unified central dashboard | Local config & brand assets |
| **RomM** | `romm` | `8080` | Retro and modern gaming collection & emulation | Google Drive `/ROMs` |
| **Kavita** | `lernae-kavita` | `5000` | Manga, comics, light novels & book reader | Google Drive `/Mangas`, `/Libros`, `/Novelas` |
| **Suwayomi** | `lernae-suwayomi` | `4567` | On-demand manga extractor & downloader | Google Drive `/Mangas` (direct CBZ) |
| **Readarr** | `lernae-readarr` | `8787` | On-demand book & light novel manager | Google Drive `/Libros`, `/Novelas` |
| **Prowlarr** | `lernae-prowlarr` | `9696` | Indexer manager (Nyaa.si, etc.) | Internal config DB |
| **qBittorrent** | `lernae-qbittorrent` | `8085` | Local staging download client | Local NVMe `/downloads` |
| **Storage Mount** | `lernae-drive.service` | — | rclone FUSE mount with VFS caching | Google Drive `Lernae/` → `/home/neoariel/Lernae/media` |

---

## Branding & Visual Identity

The brand identity is derived from the mythical Lernaean waters—representing multiple branches, nodes, and digital collections converging into a single unified nucleus.

- **Name:** Lernae (never "Lernae Hub").
- **Tagline:** One library. Your universe.
- **Core Color Palette:**
  - **Nebula White:** `#EDE8FF` (Text / Highlights)
  - **Lernae Violet:** `#7C3AED` (Primary)
  - **Arc Purple:** `#A855F7` (Accent)
  - **Deep Azure:** `#38BDF8` (Secondary)
  - **Midnight Indigo:** `#0B0623` (Background)
  - **Phantom Violet:** `#1E1140` (Surfaces & Cards)
- **Typography:** Plus Jakarta Sans (fallback: Inter, system-ui, sans-serif).
- **Asset Locations:**
  - `config/homepage/images/lernae/`:
    - `lernae-logo.png` — Horizontal lockup
    - `lernae-mark.png` — Master emblem mark (1024x1024)
    - `favicon-16.png`, `favicon-32.png`, `favicon-48.png`, `favicon-64.png`, `favicon-128.png`, `favicon-192.png`, `favicon-512.png`
    - `favicon.ico` — Multi-resolution favicon
    - `apple-touch-icon.png` — iOS / Mobile web clip
- `config/homepage/images/background.jpg` — Midnight Indigo & Violet subtle wave atmosphere

---

## Foundation v0.2

The Foundation documents define the product boundaries and architecture before implementation. Phase 0 provides documentation, contracts, buildable scaffolds, and reproducible checks; it does not implement the application runtime.

- `docs/PRODUCT.md` — product vision and UX principles
- `docs/ARCHITECTURE.md` — system responsibilities and boundaries
- `docs/DATA_MODEL.md` — core entities and relationships
- `docs/MVP.md` — first product slice
- `docs/ROADMAP.md` — staged delivery plan
- `docs/CONTRIBUTING.md` and `AGENTS.md` — contribution and agent rules
- `docs/ADR/` — accepted architecture decisions
- `docs/AUDIT_DECISIONS.md` — accepted audit findings
- `docs/MANIFEST.md`, `schemas/`, and `examples/` — inventory manifest contract
- `ANTIGRAVITY_AUDIT_PROMPT.md` and `FOUNDATION_CHECKLIST.md` — audit record and foundation checklist
- `docs/PHASE_0_REPORT.md` — delivered scope, limitations, and verification

The existing Homepage-based dashboard remains a separate system dashboard and is preserved as-is. It is not the React application frontend.

### Product principle

A user should be able to search for a title, choose the version or medium, and take a human action such as **PLAY**, **WATCH**, **READ**, or **LISTEN**. Lernae is intended to coordinate availability, storage, preparation, launch, and personal-state updates behind that action. It complements specialist software rather than replacing tools such as RomM, Jellyfin, Kavita, media managers, or emulators.

### Current status

**Foundation / pre-implementation.** The repository now has buildable technical scaffolds, but no operational Server, Agent, or product frontend features. The existing personal infrastructure remains a separate reference environment.

## Developer setup

### Prerequisites

- Go 1.27 or later
- Node.js 22.12 or later and npm
- Python 3 (used only to check JSON syntax)
- Docker Compose v2 only if running the existing infrastructure

`make help` lists all Foundation commands. From the repository root:

```sh
make setup      # check tools and install Web dependencies from the lockfile
make dev        # run only the static Web scaffold at http://127.0.0.1:5173
make lint       # Go formatting check, go vet, and Web ESLint
make typecheck  # TypeScript type-check
make test       # Go tests and JSON syntax checks
make build      # compile Go scaffolds and build the Web application
```

The Web development page is not connected to a Server or Agent. The Go commands under `cmd/server` and `cmd/agent` are buildable placeholders and exit after identifying themselves as scaffolds; neither is an operational service yet.

### Existing Homepage infrastructure

The existing Homepage dashboard and supporting Compose configuration are independent from the Phase 0 application shell. To start only Homepage with the existing Compose file:

```sh
docker compose up -d lernae-homepage
```

Homepage is available at `http://localhost:3000`. The existing service configuration remains in `compose.yml` and `config/homepage/`; Phase 0 does not replace, merge, or reconfigure it.

### Configuration and runtime data

Phase 0 does not create application configuration, a SQLite database, or an Agent socket. There is no database or socket path to configure yet. The current Homepage configuration remains under `config/homepage/`. Runtime data directories are local and ignored by Git; do not copy them into the application repository.

## Phase 0 boundary

This is an infrastructure scaffold, not the first vertical slice. There is no HTTP API, SQLite initialization or migrations, persistent job system, executable manifest parser, Server–Agent UDS transport, transfer/restore/launcher behavior, or Web health dashboard. The schema and example manifest are present as documentation artifacts; the Phase 0 checks only confirm that both files are syntactically valid JSON. These runtime capabilities are deferred according to `docs/ROADMAP.md`.

For the exact implemented scope, verification, and known limitations, see `docs/PHASE_0_REPORT.md`.

## Legal and provider boundary

Lernae Core is an orchestration platform. Acquisition is provider-based and must support lawful sources, personal backups, existing media servers, digital storefronts and user-configured tools. Core should not embed infringing catalogs, credentials, source-specific bypasses or assumptions about where a user's files originate.
