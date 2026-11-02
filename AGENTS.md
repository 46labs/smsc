# AGENTS.md

## Project

SMPP 3.4 SMSC mock for local development and tests. Takes binds, answers submits, sends receipts and MO, records every PDU, and exposes a `/mock` control API.

## Architecture

`cmd/main.go` reads env and serves `pkg/smsc`: the SMPP listener and the control API. State is in memory. The codec is gosmpp.

## Setup

```bash
go mod download
```

## Run

```bash
just run      # SMPP :2775, control :8080
```

## Test

```bash
just ci
```

## Principles

- Library-first: prefer existing packages over custom implementations. Review for duplication.
- No code without a plan. Investigation and implementation are separate steps.
- Own the problem. Never dismiss environment issues.
- Research before action: check current best practices and latest stable versions before making changes.
- Run linters and tests before completing work.

## Code Conventions

### Git

- Feature branches from main: `feat/`, `fix/`, `chore/`, `hotfix/`
- Never commit to main. Never push without explicit ask.
- Conventional commit prefixes: `feat:`, `fix:`, `docs:`, `test:`, `chore:`, `refactor:`
- Single line, concise, imperative subjects.
- No Co-Authored-By trailers.
- Squash merge preferred.

### Testing

- Great test coverage is a goal, not an afterthought.
- Testcontainers for integration tests where applicable.
- No sleep-based timing in tests.

### Frontend Design

For any UI work (components, pages, landing, dashboards), use the universal `impeccable` skill for the design pass before writing implementation code. Install via `npx skills add pbakaus/impeccable -g`. Project context lives in `PRODUCT.md` and `DESIGN.md` at the repo root; run `impeccable teach` and `impeccable document` once per project to scaffold them.

Subcommands: `craft` (new UI), `shape` (refine), `audit` (review), `live` (browser iteration).

<!-- Add language-specific sections as needed:
### Go
### TypeScript
### Python
### C++
-->

## README Policy

- For human consumption. Concise. No em-dashes. No AI prose.
- Content: what it is, how to set up, how to run, how to test.
- Mermaid diagrams where they help.
- Refreshed at end of implementation cycles before push.

## Specs & Plans

- Design specs: `docs/specs/YYYY-MM-DD-<topic>-design.md`
- Implementation plans: `docs/plans/YYYY-MM-DD-<feature-name>.md`

## Review Protocol

Specs use a Claude-orchestrated review cycle. Claude owns the document; other agents (Codex, etc.) are invoked as review oracles.

- Claude drafts and holds the spec. Codex is called via `codex exec` for scoped review passes.
- Codex returns assessments; Claude integrates them. Only Claude writes to disk.
- Marker blocks (git merge conflict syntax with `|||` threads) are **preserved in the final doc** as decision records — they are not stripped on finalize.
- Vocabulary: `||| agent [action]` where action is `gap`, `recommend`, `question`, `accept`, `accept-partial`, `reject`, or `edit`.
- Bottom block (between `=======` and `>>>>>>>`) is the final agreed content. Sign-off line: `>>>>>>> claude` (solo) or `>>>>>>> claude+codex` (joint).
- Record a block when codex input materially changed the outcome. Skip for trivial confirmations.
- `## Decisions` section indexes choices: flat bullets for solo/confirmed, marker blocks for cross-agent influenced.

### Conflict-Marker Tooling

Final specs and plans intentionally contain markers. Exclude these paths from conflict-marker checks:

- **Pre-commit**: exclude `docs/specs/**` and `docs/plans/**` on conflict-marker hooks
- **CI**: same path exclusion on grep/lint steps that detect `<<<<<<<`
- **Editor**: suppress merge-conflict warnings for these paths

## Reference Files

- When project instructions point to local reference files or folders, resolve relative paths from the file that mentions them first.
- Read only the reference files relevant to the current task. Do not load an entire reference tree by default.
- Treat reference files as supporting context. Validate version-specific behavior against the active project code, config, and installed tool versions.

## Notes

<!-- Project-specific context, decisions, quirks -->
