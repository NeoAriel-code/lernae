# Lernae

> One library. Your universe.

Lernae es un proyecto de biblioteca personal autoalojada para buscar, consumir y seguir juegos, vídeo, libros y audio desde una sola experiencia, con proveedores especializados reemplazables.

---

## Estado actual: Phase 0

Este repositorio contiene la base técnica, no una aplicación operativa ni el primer flujo completo del MVP.

| Componente | Estado en esta versión |
| :--- | :--- |
| **Web** | Página estática React/TypeScript con Vite, sin conexión al backend. |
| **Server y Agent** | Placeholders Go compilables; registran un mensaje y terminan, sin iniciar servicios. |
| **Contratos** | Tipos de dominio, capacidades de proveedores y comandos tipados del Agent, sin transporte ni ejecución. |
| **Inventario** | Esquema y ejemplo de manifiesto JSON; las comprobaciones solo validan su sintaxis. |

La búsqueda, adquisición, restauración, lanzamiento y persistencia descritos en los documentos de producto son objetivos, no funcionalidades disponibles. La configuración heredada de Homepage y sus recursos visuales no demuestran una integración operativa de Lernae.

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
- `docs/CONTRIBUTING.md` and `AGENTS.md` — contribution and agent rules
- `docs/ADR/` — architecture decisions
- `docs/MANIFEST.md`, `schemas/`, and `examples/` — inventory manifest contract

### Documentación local

Los planes, informes de fase, auditorías, prompts y notas de trabajo se conservan localmente, fuera de Git, según `.gitignore`. No forman parte de la documentación pública ni son necesarios para seguir este README.

### Product principle

A user should be able to search for a title, choose the version or medium, and take a human action such as **PLAY**, **WATCH**, **READ**, or **LISTEN**. Lernae is intended to coordinate availability, storage, preparation, launch, and personal-state updates behind that action. It complements specialist software rather than replacing tools such as RomM, Jellyfin, Kavita, media managers, or emulators.

## Developer setup

### Prerequisites

- Go 1.27 or later
- Node.js 22.12 or later and npm
- Python 3 (used only to check JSON syntax)

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

### Configuración y datos de ejecución

Phase 0 no crea configuración de la aplicación, una base de datos SQLite ni un socket del Agent; todavía no hay rutas de base de datos o socket que configurar. Los directorios de datos de ejecución son locales y están ignorados por Git. La configuración heredada bajo `config/homepage/` es independiente del frontend React.

## Límites de Phase 0

Esta base técnica no implementa el primer flujo completo: no hay API HTTP, inicialización SQLite ni migraciones, sistema persistente de jobs, parser ejecutable del manifiesto, transporte UDS Server–Agent, transferencias, restauración, lanzamiento ni panel de salud Web. El esquema y el manifiesto de ejemplo son artefactos documentales; los checks de Phase 0 solo confirman que ambos son JSON sintácticamente válido.

El flujo de producto pendiente se define en [MVP](docs/MVP.md), con responsabilidades y límites en [Arquitectura](docs/ARCHITECTURE.md) y [ADR](docs/ADR/).

## Legal and provider boundary

Lernae Core is an orchestration platform. Acquisition is provider-based and must support lawful sources, personal backups, existing media servers, digital storefronts and user-configured tools. Core should not embed infringing catalogs, credentials, source-specific bypasses or assumptions about where a user's files originate.
