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
