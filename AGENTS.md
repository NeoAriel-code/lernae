# AGENTS.md — Rules for Coding Agents

You are implementing Lernae, not redesigning it from first principles.

## Mandatory reading order

1. `docs/PRODUCT.md`
2. `docs/ARCHITECTURE.md`
3. `docs/DATA_MODEL.md`
4. `docs/MVP.md`
5. relevant `docs/ADR/*`
6. current task/issue

## Non-negotiable principles

- Lernae is one user-facing product above replaceable specialist providers.
- Core concepts must not become coupled to Google Drive, RomM, Dolphin or another concrete provider.
- User intent is expressed as PLAY/WATCH/READ/LISTEN, not infrastructure verbs.
- Personal state belongs to Lernae.
- New heavy assets are usable locally before background archival where the flow permits it.
- Random-access workloads must not run directly against Google Drive/rclone remote mounts.
- Server must not execute arbitrary shell commands from UI/metadata.
- Server orchestrates machine-local Jobs; Agent owns the local cache and physically executes restores/launches.
- Restores must use isolated staging + verification + atomic promotion before content becomes launchable.
- On Linux MVP, Server↔Agent communication uses the documented UDS boundary; do not expose an unauthenticated localhost Agent HTTP API.
- External integrations are capability-based; one integration may implement multiple capabilities.
- Do not introduce distributed infrastructure without measured need.

## Scope discipline

For every task:

1. inspect relevant code;
2. restate the implementation plan briefly;
3. modify only task-relevant areas;
4. add/update tests;
5. run relevant tests/lint/build;
6. review the final diff;
7. report assumptions and remaining risks.

Do not perform speculative refactors.

## Stop conditions

Stop and report instead of improvising when:

- a requested change contradicts an ADR/product spec;
- data loss could occur;
- credentials/secrets are unavailable;
- the task would require changing public behavior outside scope;
- a provider's API/terms do not support the requested operation;
- tests reveal an architectural conflict that the task cannot resolve safely.

## Foundation phase

During Foundation, create scaffolding and contracts only when explicitly tasked. Do not prematurely implement every provider named in documentation.
