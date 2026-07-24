<p align="center">
  <img width="1024" alt="Engram — One Brain. Local or Cloud." src="assets/branding/engram-banner.png" />
</p>

<p align="center">
  <strong>Persistent memory for AI coding agents</strong><br>
  <em>Agent-agnostic, single binary, zero dependencies. Local SQLite by default; MemoryLake per project when you want it.</em>
</p>

<p align="center">
  <a href="#quick-start">Quick Start</a> &bull;
  <a href="#storage-backends">Storage Backends</a> &bull;
  <a href="docs/AGENT-SETUP.md">Agent Setup</a> &bull;
  <a href="docs/ARCHITECTURE.md">Architecture</a> &bull;
  <a href="RELEASING.md">Releasing</a> &bull;
  <a href="DOCS.md">Full Docs</a>
</p>

---

> **engram** `/ˈen.ɡræm/` — _neuroscience_: the physical trace of a memory in the brain.

Your AI coding agent forgets everything when the session ends. Engram gives it a brain.

A **single Go binary** (pure-Go SQLite, `CGO_ENABLED=0`, no external services) exposed through four interfaces — **CLI, MCP server (stdio), HTTP API, and an interactive TUI**. Works with **any agent that speaks MCP**: Claude Code, OpenCode, Gemini CLI, Codex, VS Code (Copilot), Antigravity, Cursor, Windsurf, and more.

```
Agent (Claude Code / OpenCode / Gemini CLI / Codex / VS Code / ...)
    │  MCP stdio  (also: CLI · HTTP · TUI)
    ▼
Engram (single Go binary)  ──►  per-project backend routing
    ├─ default ──────────►  Local SQLite + FTS5   (~/.engram/engram.db)   ← source of truth
    └─ opt-in per project ►  MemoryLake V3 facts   (engram workspace)      ← semantic backend
```

Local SQLite is the source of truth. Everything else — MemoryLake for a project, cloud replication — is **opt-in** and never owns your data.

## Quick Start

### Install

Engram ships prebuilt, statically-linked binaries on the [Releases page](https://github.com/relytcloud/engram/releases) for **macOS / Linux / Windows × amd64 / arm64**. No Homebrew, no Node, no Python, no Docker — **one binary, one SQLite file**.

```bash
# macOS (Apple Silicon shown) / Linux — pick your os_arch from the releases page
VER=0.1.2
curl -fsSL -o engram.tar.gz \
  "https://github.com/relytcloud/engram/releases/download/v${VER}/engram_${VER}_darwin_arm64.tar.gz"
tar -xzf engram.tar.gz engram
install -m 0755 engram ~/.local/bin/engram      # ensure ~/.local/bin is on PATH
engram version
```

Build from source instead (needs Go 1.25+):

```bash
CGO_ENABLED=0 go build -o ~/.local/bin/engram ./cmd/engram
```

Full walkthrough (arch table, checksums, macOS Gatekeeper, MemoryLake config) → **[docs/INSTALL.zh-CN.md](docs/INSTALL.zh-CN.md)** (中文). Release process → **[RELEASING.md](RELEASING.md)**.

### Setup Your Agent

| Agent                | One-liner                                                                                 |
| -------------------- | ----------------------------------------------------------------------------------------- |
| Claude Code          | `engram setup claude-code`  (or: `claude plugin marketplace add relytcloud/engram && claude plugin install engram@engram`) |
| Pi                   | `engram setup pi`                                                                          |
| OpenCode             | `engram setup opencode`                                                                    |
| Gemini CLI           | `engram setup gemini-cli`                                                                  |
| Codex                | `engram setup codex`                                                                       |
| Antigravity CLI      | `engram setup antigravity-cli`                                                             |
| Windsurf             | `engram setup windsurf`                                                                    |
| Qwen Code            | `engram setup qwen`                                                                        |
| Kiro                 | `engram setup kiro`                                                                        |
| Cursor               | `engram setup cursor`                                                                      |
| VS Code (Copilot)    | `engram setup vscode-copilot`                                                              |
| Kilo Code            | `engram setup kilocode`                                                                    |
| Any other MCP client | See [docs/AGENT-SETUP.md](docs/AGENT-SETUP.md)                                             |

**What `engram setup` does** — writes the MCP config + plugin files for the chosen agent, then you restart the agent. For Claude Code it registers the `relytcloud/engram` plugin marketplace (hooks + skills + Memory Protocol) and writes a durable `~/.claude/mcp/engram.json` pointing at your binary's absolute path. No server to start manually.

> **Do I need to run `engram serve` or `engram mcp` myself?**
> For stdio agents (Claude Code, Gemini CLI, Codex, VS Code, Cursor, Windsurf) — **no**. The agent launches `engram mcp` automatically as a short-lived stdio subprocess. `engram serve` (HTTP, port 7437, binds `127.0.0.1`) is only used by the **OpenCode plugin** and **Pi extension** for session tracking.

## Storage Backends

Engram routes **per project**. By default every project uses local **SQLite** — the source of truth, with FTS5/BM25 full-text search, exact integer ids, and verbatim content. A project only uses **MemoryLake** if you explicitly enable it; unaffected projects behave exactly as before, and both backends run side-by-side on the same machine.

```bash
# 1) One-time connection config (persisted to ~/.engram/memorylake.json, 0600).
#    --url defaults to https://app.memorylake.ai/openapi/memorylake if omitted.
engram memorylake config --api-key sk-your-key
#    Precedence at runtime: env var > saved config > built-in default.
#    (Env equivalents: ENGRAM_MEMORYLAKE_BASE_URL / _API_KEY / _WORKSPACE / _ACTOR.)

# 2) Opt a project in / out, and inspect routing.
engram memorylake enable  --project my-project     # this project → MemoryLake
engram memorylake disable --project my-project     # back to local SQLite
engram memorylake status                           # every project + its backend

# Global safety valve: force ALL projects onto SQLite regardless of enablement.
export ENGRAM_BACKEND=sqlite
```

On a MemoryLake-backed project, memories are stored as **V3 facts** under the `engram` workspace: `mem_save` appends the content and MemoryLake's pipeline extracts a fact **asynchronously** (so a just-saved memory becomes searchable a moment later), search is **semantic**, and dedup / update / conflict / forgetting are handled by MemoryLake itself. Full behavior, limitations, and the differential parity harness → **[DOCS.md — MemoryLake Backend](DOCS.md#memorylake-backend)**.

## How It Works

```
1. Agent completes significant work (bugfix, architecture decision, etc.)
2. Agent calls mem_save → title, type, What/Why/Where/Learned
3. Engram persists it via the project's backend (SQLite FTS5, or MemoryLake facts)
4. Next session: agent searches memory, gets relevant context back
```

Session lifecycle, topic keys, and memory hygiene → [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## MCP Tools

| Category               | Tools                                                                                                            |
| ---------------------- | ---------------------------------------------------------------------------------------------------------------- |
| **Save & Update**      | `mem_save`, `mem_update`, `mem_delete`, `mem_suggest_topic_key`                                                  |
| **Search & Retrieve**  | `mem_search`, `mem_context`, `mem_timeline`, `mem_get_observation`                                               |
| **Session Lifecycle**  | `mem_session_start`, `mem_session_end`, `mem_session_summary`                                                    |
| **Conflict Surfacing** | `mem_judge`, `mem_compare`                                                                                       |
| **Lifecycle Review**   | `mem_review`                                                                                                      |
| **Utilities**          | `mem_save_prompt`, `mem_stats`, `mem_capture_passive`, `mem_merge_projects`, `mem_current_project`, `mem_doctor` |

On MemoryLake-backed projects the agent-facing surface is intentionally slimmer (MemoryLake auto-manages dedup/update/conflict) — see [DOCS.md — Agent tool surface on MemoryLake-backed projects](DOCS.md#memorylake-backend). Full tool reference → [DOCS.md](DOCS.md#mcp-tools-20-tools).

## Terminal UI

```bash
engram tui
```

<p align="center">
  <img src="assets/tui-dashboard.png" alt="TUI Dashboard" width="400" />
  <img src="assets/tui-detail.png" alt="TUI Observation Detail" width="400" />
  <img src="assets/tui-search.png" alt="TUI Search Results" width="400" />
</p>

**Navigation**: `j/k` vim keys, `Enter` to drill in, `c` to copy content (OSC 52), `/` to search, `Esc` back. Catppuccin Mocha theme.

## Git Sync (Local, Opt-In)

Share memories across machines using compressed chunks — no merge conflicts, no huge files. Local SQLite stays the source of truth.

```bash
engram sync                    # Export new memories as a compressed chunk into .engram/
git add .engram/ && git commit -m "sync engram memories"
engram sync --import           # On another machine: import new chunks
engram sync --status           # Check sync status
```

## Cloud (Opt-In Replication)

Cloud is optional and always project-scoped (`--project` is required; `engram sync --cloud --all` is blocked). Local SQLite stays authoritative; cloud is replication / shared access only.

```bash
docker compose -f docker-compose.cloud.yml up -d
engram cloud config --server http://127.0.0.1:18080
engram cloud enroll smoke-project
engram sync --cloud --project smoke-project
```

`ENGRAM_CLOUD_ALLOWED_PROJECTS` is required for `engram cloud serve`. Authenticated mode, upgrade flow, dashboard, reason codes, and full runtime/env details:

- [Engram Cloud docs landing](docs/engram-cloud/README.md) · [quickstart](docs/engram-cloud/quickstart.md)
- [DOCS.md — Cloud CLI reference](DOCS.md#cloud-cli-opt-in) · [Cloud Autosync](DOCS.md#cloud-autosync)

## CLI Reference

| Command                                          | Description                                                            |
| ------------------------------------------------ | --------------------------------------------------------------------- |
| `engram setup [agent]`                           | Install agent integration                                             |
| `engram memorylake config\|enable\|disable\|status` | Configure and route the per-project MemoryLake backend             |
| `engram serve [port]`                            | Start HTTP API (default: 7437, binds 127.0.0.1)                       |
| `engram mcp [--tools=PROFILE] [--project NAME]`  | Start MCP server (stdio transport)                                    |
| `engram tui`                                     | Launch terminal UI                                                    |
| `engram search <query>`                          | Search memories                                                       |
| `engram save <title> <msg>`                      | Save a memory                                                         |
| `engram delete <obs_id>`                         | Delete an observation (soft by default; `--hard` removes permanently) |
| `engram timeline <obs_id>`                       | Chronological context                                                 |
| `engram context [project]`                       | Recent session context                                                |
| `engram stats`                                   | Memory statistics                                                     |
| `engram export [file]` / `engram import <file>`  | Export / import JSON                                                  |
| `engram sync`                                    | Git sync export/import                                                 |
| `engram conflicts <sub>`                         | Inspect and manage memory conflict relations                          |
| `engram doctor`                                  | Run read-only operational diagnostics                                 |
| `engram cloud <subcommand>`                      | Opt-in cloud config/status/enrollment + cloud runtime (`serve`)       |
| `engram projects list\|consolidate\|prune`       | Manage project names                                                  |
| `engram version`                                 | Show version                                                          |

Full CLI with all flags → [docs/ARCHITECTURE.md#cli-reference](docs/ARCHITECTURE.md#cli-reference).

### Key Environment Variables

| Variable                       | Description                                                                                          | Default                          |
| ------------------------------ | --------------------------------------------------------------------------------------------------- | -------------------------------- |
| `ENGRAM_DATA_DIR`              | Override data directory                                                                             | `~/.engram`                      |
| `ENGRAM_PORT`                  | Override HTTP server port                                                                           | `7437`                           |
| `ENGRAM_BACKEND`              | Global safety valve. `sqlite` forces every project onto local SQLite regardless of enablement.     | (unset — per-project)            |
| `ENGRAM_MEMORYLAKE_BASE_URL`  | MemoryLake V3 API base URL (overrides saved config).                                               | `…/openapi/memorylake` (default) |
| `ENGRAM_MEMORYLAKE_API_KEY`   | MemoryLake API key (overrides saved config).                                                       | (unset)                          |
| `ENGRAM_MEMORYLAKE_WORKSPACE` | Workspace memories live under.                                                                     | `engram`                         |
| `ENGRAM_CLOUD_ALLOWED_PROJECTS` | Comma-separated project allowlist for `engram cloud serve`. `*` allows all.                       | (unset)                          |

Full environment variable reference → [DOCS.md#environment-variables](DOCS.md#environment-variables).

## Documentation

| Doc                                           | Description                                                    |
| --------------------------------------------- | -------------------------------------------------------------- |
| [Install (中文)](docs/INSTALL.zh-CN.md)        | Full install + Claude Code plugin + MemoryLake config walkthrough |
| [Releasing](RELEASING.md)                     | Tag-driven GitHub Release process                              |
| [Agent Setup](docs/AGENT-SETUP.md)            | Per-agent configuration + Memory Protocol                      |
| [Architecture](docs/ARCHITECTURE.md)          | How it works + MCP tools + project structure                   |
| [Codebase Guide](docs/CODEBASE-GUIDE.md)      | Repository structure, flows, and implementation landmarks      |
| [Plugins](docs/PLUGINS.md)                    | OpenCode & Claude Code plugin details                          |
| [Engram Cloud](docs/engram-cloud/README.md)   | Cloud landing page, quickstart, and deep links                 |
| [Full Docs](DOCS.md)                          | Complete technical reference (API, schema, CLI, env, MemoryLake) |
| [Contributing](CONTRIBUTING.md)               | Contribution workflow + standards                              |

> **Dashboard contributors**: if you modify `.templ` files in `internal/cloud/dashboard/`, run `make templ` to regenerate before committing.

## License

MIT — a fork of [Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram) adding the per-project MemoryLake backend. Originally inspired by [claude-mem](https://github.com/thedotmack/claude-mem).
