# CONTRIBUTING.md — Working on Lernae

Lernae is currently an early-stage product. Correct boundaries and understandable changes matter more than speed.

## 1. Before coding

Read:

1. `PRODUCT.md`
2. `ARCHITECTURE.md`
3. `DATA_MODEL.md`
4. `MVP.md`
5. relevant ADRs
6. `AGENTS.md` if you are a coding agent

Do not infer major product behavior from existing implementation when it conflicts with the specifications.

## 2. Scope discipline

Every implementation task should have:

- problem statement;
- in-scope behavior;
- out-of-scope behavior;
- acceptance criteria;
- tests expected.

Do not perform unrelated refactors while delivering a feature.

## 3. Provider rule

Provider-specific behavior belongs behind provider interfaces.

Examples:

- IGDB authentication/query syntax belongs in IGDB provider code;
- rclone commands belong in rclone storage provider code;
- Dolphin CLI arguments belong in Dolphin launcher code.

Core domain code should not depend directly on a specific provider unless an ADR explicitly changes this rule.

## 4. Database changes

- use migrations from the first schema change;
- migrations are forward-moving and reviewed;
- never silently drop user data;
- avoid storing secrets directly in ordinary configuration tables;
- external IDs are not Lernae primary keys.

## 5. Error handling

User-facing errors should answer:

- what failed;
- what Lernae was trying to do;
- whether data is safe;
- what the user can do next.

Logs may contain technical detail but must not leak tokens/credentials.

## 6. Tests

Each provider should be testable without the real external service where practical using fixtures/fakes.

Critical workflows need integration tests.

The Soulcalibur acceptance path is a release-blocking test for MVP.

## 7. Agent-generated code

AI-generated changes receive the same review requirements as human-written code.

Agents must:

- inspect existing code before editing;
- state assumptions;
- run relevant tests;
- review their diff;
- avoid claiming success based only on compilation;
- document unresolved risks.

## 8. Commit/PR style

La documentación pública conserva producto, arquitectura, modelo de datos, contratos y ADR. Los registros de fases, auditorías, planes, tareas y notas locales de configuración quedan fuera de Git y no deben enlazarse desde referencias públicas. Si contienen una decisión vigente, resume su contrato en el documento público correspondiente, sin trasladar el historial ni afirmar aceptación.

Las reglas de `.gitignore` no eliminan archivos ya rastreados ni sus copias físicas. El mantenimiento del índice requiere autorización independiente; conserva siempre los documentos locales y el trabajo pendiente.

Prefer small, coherent changes.

Good:

- `feat(search): add IGDB provider contract`
- `feat(agent): launch Dolphin with structured args`
- `test(storage): cover failed remote restore`

Avoid giant commits named `implement lernae`.

## 9. Legal boundary

Contributions to Core must not add built-in infringing catalogs, DRM circumvention, credential theft, malware-like downloading or provider-term bypasses.

Acquisition integrations should target lawful/user-configured systems and remain separable from Core.
