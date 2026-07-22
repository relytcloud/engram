# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What Engram Is

Persistent memory for AI coding agents. A single Go binary (`cmd/engram`) backed by SQLite + FTS5, exposed through four interfaces: CLI, MCP server (stdio), HTTP API, and a Bubbletea TUI. Agent-agnostic — anything speaking MCP can use it.

**The one sentence that organizes the whole repo:** Local SQLite (`~/.engram/engram.db`) is the source of truth. Cloud is opt-in replication and shared access, *not* the owner of the data. Any change that blurs this boundary is wrong.

## Commands

```bash
go build ./cmd/engram              # build the binary (CGO_ENABLED=0 — pure-Go sqlite via modernc.org/sqlite)
go test ./...                      # unit tests (all tests NOT tagged //go:build e2e)
go test -tags e2e ./internal/server/...   # e2e tests (separate CI job; required check)
go test ./internal/store/ -run TestName   # single package / single test
make templ                         # regenerate templ → *_templ.go after editing dashboard .templ files
./setup.sh                         # link repo skills/* into .claude/skills, .codex/skills, .gemini/skills
```

CI (`.github/workflows/ci.yml`) runs exactly `go test ./...` and the e2e job — no separate lint step. There is no Makefile build target; `make` only wraps templ generation.

## Skills Are Mandatory

`AGENTS.md` is a skill index. **Before writing code for a task, load the matching skill(s)** from `skills/*/SKILL.md` and follow their rules. High-frequency ones: `architecture-guardrails` (boundaries/ownership), `project-structure` (new files/packages), `server-api` (routes/handlers), `tui-quality` (TUI model/update/view), `dashboard-htmx` + `visual-language` (dashboard), `testing-coverage`, `memory-protocol`. Skills use a hybrid format (structured rules + If/Then cookbook).

## Contribution Rules (enforced by CI)

- **Issue-first workflow.** PRs require a linked issue with the `status:approved` label (`Closes #N`) and exactly one `type:*` label. PRs without an approved issue are auto-blocked.
- **Conventional commits.** `feat(scope):`, `fix(scope):`, etc. Types map to labels (`feat`→`type:feature`, `fix`→`type:bug`, …). Use `!` / `BREAKING CHANGE:` for breaking changes.
- **Do NOT add `Co-Authored-By` trailers to commits** (repo-specific rule).
- Update docs in the same PR when behavior changes; do not reference endpoints/scripts that don't exist in code.
- npm deps in `plugin/pi` and `plugin/obsidian`: install via `npq`, honor `.npmrc` (`ignore-scripts`, `allow-git=none`, `min-release-age=3`) — never edit `.npmrc` to bypass.

## Architecture — where responsibilities live

Data flows: **agent → `cmd/engram` (CLI + runtime dispatch) → `internal/store` (SQLite) → optional sync/cloud.** `cmd/engram/main.go` is a hand-rolled subcommand switch (`serve`, `mcp`, `tui`, `search`, `save`, `sync`, `cloud`, `setup`, …) — no cobra/flag framework; args are parsed manually per subcommand.

| Package | Owns | Open first |
|---|---|---|
| `internal/store` | SQLite + FTS5, observations, sessions, prompts, relations, sync mutations, migrations, dedupe/topic-key upserts | `store.go` |
| `internal/mcp` | `mem_*` tools for agents (the primary interface) | `mcp.go`, `write_queue.go` |
| `internal/server` | Local JSON HTTP API (`engram serve`, port 7437, binds 127.0.0.1 only) | `server.go` |
| `internal/tui` | Bubbletea terminal UI (Elm-style model/update/view, Catppuccin Mocha) | `model.go` |
| `internal/setup` | `engram setup <agent>` — writes MCP config + plugin files per agent | `registry.go`, `agents.go`, `setup.go` |
| `internal/sync` | git-friendly compressed chunks + transport (local sync) | `sync.go`, `transport.go` |
| `internal/cloud/autosync` | optional background push/pull orchestration | `manager.go` |
| `internal/cloud/cloudstore` | Postgres materialization, org-wide controls | — |
| `internal/cloud/cloudserver` | HTTP contract + enforcement for `engram cloud serve` | — |
| `internal/cloud/dashboard` | server-rendered HTML/HTMX browser UI (templ) | `dashboard.go`, `*.templ` |
| `internal/llm` | LLM provider adapters (claude, opencode) + cost/prompt | `factory.go` |
| `internal/project` | project detection from cwd | `detect.go` |

**Boundary decision rules** (from `architecture-guardrails`): local-only → `internal/store`; cloud materialization → `cloudstore`; HTTP enforcement → `cloudserver`; browser UX → `dashboard`; background orchestration → `autosync`. Keep plugin/adapter layers thin — real behavior belongs in Go packages, never hidden in helpers or templates.

## Interface & memory-model gotchas

- **MCP transport is stdio only.** `engram mcp` is a short-lived subprocess the agent launches; there is no network MCP endpoint. Agents don't run it manually.
- `engram serve` (HTTP API) is only needed by the **OpenCode plugin** and **Pi extension** for session tracking — not by stdio agents (Claude Code, Gemini, Codex, Cursor, Windsurf).
- **Cloud is always project-scoped**: `--project` is required; `engram sync --cloud --all` is intentionally blocked. `ENGRAM_CLOUD_ALLOWED_PROJECTS` is required for `engram cloud serve`.
- **`topic_key` makes `mem_save` an upsert** (same `project+scope+topic_key` updates in place, `revision_count++`). Format: slash-separated lowercase kebab-case (`architecture/auth-model`), max 2 levels — this matters for FTS5 tokenization. Without it, every save is a new row.
- Dedupe (hash+project+scope+type+title) prevents repeated inserts; `mem_delete` is soft-delete by default; search/context/timeline ignore soft-deleted rows.
- Dashboard `.templ` files are compiled — after editing them run `make templ` and commit the regenerated `*_templ.go`.
- **MemoryLake is a per-project opt-in backend** (`internal/memorylake`, routed via `internal/mcp.BackendSelector`): default is SQLite for every project; only projects explicitly enabled via `engram memorylake enable --project <name>` route to MemoryLake. `ENGRAM_BACKEND=sqlite` is a global safety-valve override. Enabled-project memories live under the `engram` workspace at your configured MemoryLake tenant (default org: zbyte) — see "MemoryLake Backend" in `DOCS.md` for config, limitations, and the `internal/paritytest` differential test harness.

## Reference docs

`docs/CODEBASE-GUIDE.md` (ownership/guardrails, with `docs/codebase/*.md` deep dives), `docs/ARCHITECTURE.md` (session lifecycle, memory hygiene, topic keys), `DOCS.md` (full API/schema/CLI/env-var reference — the exhaustive source), `openspec/changes/*` (specs for large features). `CONTRIBUTING.md` has the full label/workflow system.
