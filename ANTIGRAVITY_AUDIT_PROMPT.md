> Audit status: completed for Foundation v0.1. Retained for provenance/re-audit.

# Prompt — Lernae Foundation Architecture Audit

You are acting as an independent senior architecture/product auditor for a new open-source self-hosted project called **Lernae**.

Do NOT implement code yet.
Do NOT rewrite the documents merely to express stylistic preferences.
Do NOT expand scope with fashionable infrastructure.

Read, in this order:

1. README.md
2. docs/PRODUCT.md
3. docs/ARCHITECTURE.md
4. docs/DATA_MODEL.md
5. docs/MVP.md
6. docs/ROADMAP.md
7. docs/CONTRIBUTING.md
8. docs/ADR/*.md
9. AGENTS.md

## Product context

Lernae should let a user search across multiple entertainment media types, identify what they want, determine whether it is locally available/archived/not yet owned, prepare or request it through configured providers, launch it with the correct runtime, and retain personal state such as history/progress/favorites.

The first vertical slice is intentionally narrow:

`Search Soul Calibur → Soulcalibur II → GameCube → remote asset → restore complete local copy → launch Dolphin → record session.`

Initial personal environment is Linux CachyOS/Omarchy with existing RomM, rclone/Google Drive and Dolphin, but Lernae must not hard-code that environment as the universal architecture.

## Your audit goals

Find concrete risks before implementation starts.

Review specifically:

### A. Product/domain model
- Is Universe → Work → Edition → Asset sufficient for the first years of likely use?
- Are any concepts conflated?
- Are there obvious media types that break the model?
- Is Personal Media State separated correctly from provider state?

### B. Server/Agent boundary
- Is the split justified?
- Is responsibility misplaced?
- What is the minimum secure protocol needed for MVP on one machine?

### C. Provider architecture
- Are provider categories too broad or too fragmented?
- Could one provider implement multiple capabilities cleanly?
- Where could provider-specific details leak into Core?

### D. Storage lifecycle
- Audit the complete-file restore approach for games/ROMs.
- Identify risks around partial copies, interrupted transfers, disk pressure, duplicate assets, checksum cost and archive consistency.
- Confirm that the design avoids using Google Drive as a random-access execution filesystem.

### E. SQLite/job model
- Is in-process durable job execution a reasonable MVP choice?
- What must be persisted to recover safely after Server restart?
- Identify concurrency traps without proposing a separate queue unless necessary.

### F. Security
- Review launcher/process execution, path traversal, Agent auth, provider credentials and local network exposure.
- Separate MVP-localhost risks from public-alpha requirements.

### G. MVP scope
- Is the Soulcalibur vertical slice genuinely minimal?
- Identify anything included that should be deferred.
- Identify anything missing that would prevent an honest end-to-end demo.

### H. Open-source maintainability
- Would another contributor understand where to add a provider?
- Identify architectural decisions likely to create painful breaking changes later.

## Required output

Return a structured audit with only these sections:

1. **Executive assessment** — maximum 10 lines.
2. **Blockers before coding** — issues that should be fixed in Foundation first.
3. **Important but non-blocking concerns**.
4. **MVP simplifications you recommend**.
5. **Architecture decisions you agree with and why**.
6. **Architecture decisions you challenge and why**.
7. **Exact documentation changes recommended** — reference file and section.
8. **Go / No-Go for Phase 0 implementation**.

For every blocker/concern include:

- severity: Critical / High / Medium / Low;
- concrete failure scenario;
- smallest reasonable mitigation.

Do not score the project numerically.
Do not propose Kubernetes, microservices, Kafka, Redis or PostgreSQL without showing a concrete requirement that Foundation cannot satisfy otherwise.
