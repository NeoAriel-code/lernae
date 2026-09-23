# AUDIT_DECISIONS.md — Foundation v0.2

## Purpose

Record the disposition of the first independent Antigravity architecture audit before implementation starts.

## Blockers

### 1. Server vs Agent restore ownership — ACCEPTED

Server owns orchestration and durable Job state. Agent owns the playback machine's cache and physically executes restore/archive operations. MVP communication is via local UDS.

### 2. Interrupted-transfer cache corruption — ACCEPTED

All restores write into same-filesystem `.staging/<job_id>/`, validate transfer success + expected size, then atomically rename into the final cache path. Staging is never launchable. Startup recovery handles interrupted Jobs/stale staging.

### 3. Work/Edition conflation + multi-file gap — ACCEPTED WITH REFINEMENT

Work now uses broad `medium` plus `work_type`; platform/representation belongs to Edition. Audiobook/EPUB are Edition formats for a literature Work by default. Instead of placing part fields directly on Asset, Foundation adds `AssetPart`, keeping Asset as the logical launchable/preservable bundle while supporting CUE/BIN and multi-disc layouts cleanly.

## Non-blocking concerns

### SQLite contention — ACCEPTED

Require WAL, busy timeout, foreign keys, serialized writes and throttled durable progress updates.

### Orphaned Jobs after restart — ACCEPTED

Startup reconciliation marks stale active Jobs interrupted and cleans abandoned staging.

### Localhost Agent attack surface — ACCEPTED

Linux MVP uses Unix Domain Socket IPC with user-only permissions; remote network transport is deferred to a later ADR.

### RomM coupling — ACCEPTED

A versioned local inventory manifest is the deterministic MVP baseline. RomM implements the same inventory capability later and does not gate the first vertical slice.

## MVP simplifications

- local manifest first: accepted;
- UDS for Linux MVP: accepted;
- transfer success + expected byte size before launch: accepted; cryptographic hash may follow asynchronously;
- strict Dolphin runner instead of generic launcher framework: accepted.

## Architecture challenges

- provider silos: corrected to capability interfaces;
- storage execution placement: clarified as Agent responsibility;
- premature Universe resolver: confirmed deferred until Phase 3, with manual/rule-assisted grouping first.

## Result

Foundation v0.2 is cleared for Phase 0 repository scaffolding. This is not approval to implement the entire roadmap; Phase 0 remains documentation/contracts/scaffolding/CI only.
