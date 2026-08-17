<p align="center">
  <img width="1024" alt="Engram — persistent memory for AI coding agents" src="assets/branding/engram-banner.png" />
</p>

<p align="center">
  <strong>Persistent memory for AI coding agents</strong><br>
  <em>Agent-agnostic · single Go binary · zero dependencies · local SQLite by default, MemoryLake per project when you want it.</em>
</p>

<p align="center">
  <a href="#what-this-fork-changes">Fork Changes</a> &bull;
  <a href="#architecture">Architecture</a> &bull;
  <a href="#install">Install</a> &bull;
  <a href="#connect-your-agent">Connect Your Agent</a> &bull;
  <a href="#usage">Usage</a> &bull;
  <a href="#storage-backends">Backends</a> &bull;
  <a href="DOCS.md">Full Docs</a>
</p>

---

> **engram** `/ˈen.ɡræm/` — _neuroscience_: the physical trace of a memory in the brain.

Your AI coding agent forgets everything when the session ends. Engram gives it a brain: a persistent memory store that any MCP-speaking agent can read and write, so decisions, bug fixes, and conventions survive across sessions, compaction, and machines.

This is **`relytcloud/engram`** — a fork of [`Gentleman-Programming/engram`](https://github.com/Gentleman-Programming/engram) that adds a **pluggable, per-project [MemoryLake](https://app.memorylake.ai) backend** alongside the original local SQLite store. Everything the upstream binary does still works; MemoryLake is strictly opt-in, per project.

## What This Fork Changes

Three things differ from upstream. If you have used upstream engram before, these are the deltas:

| Area | Upstream (`Gentleman-Programming`) | This fork (`relytcloud`) |
| --- | --- | --- |
| **Storage** | Local SQLite only | Local SQLite **+ optional per-project MemoryLake backend** (`engram memorylake …`) |
| **Install** | `brew install gentleman-programming/tap/engram` | **GitHub Release archives** (no Homebrew tap) — download or build from source |
| **Claude Code / Codex marketplace** | `claude plugin marketplace add Gentleman-Programming/engram` | `claude plugin marketplace add relytcloud/engram` — wired into `engram setup claude-code` |

Details: [Install](#install) · [Connect Your Agent](#connect-your-agent) · [Storage Backends](#storage-backends).

## Architecture

Engram is **one Go binary** (`cmd/engram`, pure-Go SQLite via `modernc.org/sqlite`, `CGO_ENABLED=0` — no CGO, no external runtime). It exposes the same memory store through **four interfaces**, and routes each call to a backend **per project**.

```
   ┌── CLI            engram save / search / context / tui / sync ...
   ├── MCP (stdio)    engram mcp   ← agents launch this automatically (Claude Code, Gemini, Codex, ...)
   ├── HTTP API       engram serve (127.0.0.1:7437)  ← OpenCode plugin + Pi extension only
   └── TUI            engram tui   (Bubbletea, Catppuccin Mocha)
                 │
                 ▼
        per-project backend routing  (internal/mcp.BackendSelector)
                 │
        ┌────────┴─────────────────────────────┐
        ▼                                        ▼
  Local SQLite + FTS5                     MemoryLake V3 facts
  (~/.engram/engram.db)                   (engram workspace, cloud)
  DEFAULT · source of truth               OPT-IN per project · semantic
  exact ids · verbatim · FTS5/BM25        async extraction · vector search
```

**The one rule that organizes the repo:** local SQLite is the source of truth. MemoryLake (for an enabled project) and cloud sync are opt-in layers on top — they never own the data.

Where responsibilities live (Go packages):

| Package | Owns |
| --- | --- |
| `internal/store` | SQLite + FTS5, observations/sessions/prompts, dedupe, migrations |
| `internal/mcp` | `mem_*` tools for agents + per-project `BackendSelector` routing |
| `internal/memorylake` | The MemoryLake V3 backend: client, config, fact mapping, search |
| `internal/server` | Local JSON HTTP API (`engram serve`) |
| `internal/tui` | Terminal UI |
| `internal/setup` | `engram setup <agent>` — writes MCP config + plugin files |
| `internal/sync` · `internal/cloud/*` | Git-friendly chunk sync + optional cloud replication |

Full ownership map and flows → [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) · [docs/CODEBASE-GUIDE.md](docs/CODEBASE-GUIDE.md).

## Install

Engram ships prebuilt, statically-linked binaries on the **[Releases page](https://github.com/relytcloud/engram/releases)** for **macOS / Linux / Windows × amd64 / arm64**. No Homebrew, no Node, no Python, no Docker — **one binary, one SQLite file**.

Pick your `os_arch` from the release assets (`uname -sm` tells you which):

```bash
VER=0.2.0      # latest tag on the Releases page

# macOS Apple Silicon shown; swap darwin_arm64 for darwin_amd64 / linux_amd64 / linux_arm64
curl -fsSL -o engram.tar.gz \
  "https://github.com/relytcloud/engram/releases/download/v${VER}/engram_${VER}_darwin_arm64.tar.gz"
tar -xzf engram.tar.gz engram
install -m 0755 engram ~/.local/bin/engram      # make sure ~/.local/bin is on your PATH
engram version                                   # → 0.2.0
```

> macOS binaries are ad-hoc signed (not notarized). If Gatekeeper blocks the first run:
> `xattr -d com.apple.quarantine ~/.local/bin/engram`

**Build from source** instead (needs Go 1.25+; produces the same static binary):

```bash
git clone https://github.com/relytcloud/engram.git && cd engram
CGO_ENABLED=0 go build -o ~/.local/bin/engram ./cmd/engram
```

Full walkthrough (arch table, checksums, PATH, MemoryLake config) → **[docs/INSTALL.zh-CN.md](docs/INSTALL.zh-CN.md)** (中文). How releases are built → **[RELEASING.md](RELEASING.md)**.

## Connect Your Agent

One command wires an agent to Engram — it writes the MCP config + plugin files, then you restart the agent. No server to start manually.

| Agent                | Command                                                                                   |
| -------------------- | ----------------------------------------------------------------------------------------- |
| **Claude Code**      | `engram setup claude-code`                                                                 |
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

### Claude Code plugin (marketplace change)

`engram setup claude-code` installs the plugin from **this fork's marketplace** and registers the MCP server:

```bash
engram setup claude-code
# equivalent to, and this is the marketplace change vs upstream:
#   claude plugin marketplace add relytcloud/engram      # ← relytcloud, not Gentleman-Programming
#   claude plugin install engram@engram                  # hooks + skills + Memory Protocol
#   + writes ~/.claude/mcp/engram.json → { command: "<abs path to engram>", args: ["mcp","--tools=agent"] }
```

The marketplace supplies the **plugin** (hooks for proactive save + compaction recovery, skills). The actual memory engine — including the MemoryLake backend — is **your local binary**, referenced by absolute path in `~/.claude/mcp/engram.json`, so it survives plugin auto-updates. After setup, **restart Claude Code**.

> Already added the upstream marketplace before? Both are named `engram`; remove the old one first:
> `claude plugin marketplace remove engram && engram setup claude-code`

> **Do I run `engram serve` / `engram mcp` myself?** For stdio agents (Claude Code, Gemini CLI, Codex, VS Code, Cursor, Windsurf) — **no**, the agent launches `engram mcp` as a short-lived subprocess. `engram serve` (HTTP, `127.0.0.1:7437`) is only used by the OpenCode plugin and Pi extension for session tracking.

## Usage

Agents call the `mem_*` MCP tools automatically per the Memory Protocol, but everything is also available from the CLI:

```bash
# Save & search
engram save "Use topic keys for upserts" "Same project+scope+topic_key updates in place." \
  --type convention --project my-app
engram search "topic key" --project my-app
engram context my-app            # recent session context for a project
engram timeline <obs_id>         # chronological neighbours of an observation

# Browse
engram tui                       # interactive terminal UI (j/k, Enter, / search, c copy, Esc)
engram stats                     # counts by type/project

# Share across machines (opt-in, local, git-friendly)
engram sync                      # export new memories as a compressed chunk into .engram/
git add .engram/ && git commit -m "sync engram memories"
engram sync --import             # on another machine
```

**MCP tools** (agent-facing): save/update (`mem_save`, `mem_update`, `mem_delete`, `mem_suggest_topic_key`), search/retrieve (`mem_search`, `mem_context`, `mem_timeline`, `mem_get_observation`), session lifecycle (`mem_session_start/end/summary`), conflict surfacing (`mem_judge`, `mem_compare`), review (`mem_review`), and utilities (`mem_save_prompt`, `mem_stats`, `mem_capture_passive`, `mem_merge_projects`, `mem_current_project`, `mem_doctor`). Full reference → [DOCS.md](DOCS.md#mcp-tools-20-tools).

## Storage Backends

Engram routes **per project**. By default every project uses local **SQLite** — the source of truth, with FTS5/BM25 full-text search, exact integer ids, and verbatim content. A project only uses **MemoryLake** if you explicitly enable it; other projects are untouched, and both backends run side-by-side on one machine.

```bash
# 1) Configure the connection once (persisted to ~/.engram/memorylake.json, mode 0600).
#    --url defaults to https://app.memorylake.ai/openapi/memorylake when omitted.
engram memorylake config --api-key sk-your-key
engram memorylake config                 # print effective config (api key masked)
#    Precedence at runtime: env var > saved config > built-in default.
#    Env equivalents: ENGRAM_MEMORYLAKE_BASE_URL / _API_KEY / _WORKSPACE / _ACTOR.

# 2) Opt a project in / out, and inspect routing.
engram memorylake enable  --project my-app     # → MemoryLake; on FIRST enable it also
                                               #   auto-migrates existing SQLite memories
                                               #   (verbatim, idempotent; --no-migrate to skip).
engram memorylake disable --project my-app     # back to local SQLite
engram memorylake status                       # every project + its current backend

# Global safety valve: force ALL projects onto SQLite regardless of enablement.
export ENGRAM_BACKEND=sqlite
```

On a MemoryLake-backed project, memories are stored as **V3 facts** under the `engram` workspace. `mem_save` writes the observation **verbatim and synchronously** via MemoryLake's direct fact-add endpoint — the content is stored as-is (title preserved), a **real fact id** comes back immediately, and it's queryable right away (no asynchronous extraction). Search is **semantic** (vector). Note the trade-off: the direct write does **no** server-side dedup / `topic_key` upsert / conflict decision, so each save is a new fact. Behavior, limitations, and the differential parity harness → **[DOCS.md — MemoryLake Backend](DOCS.md#memorylake-backend)**.

Enabled projects can additionally opt into per-turn conversation sync
(`engram memorylake conversations enable --project <name>`), which appends each
completed turn to MemoryLake and lets its extraction pipeline mint memories
without the agent having to call `mem_save`. See "Per-turn conversation sync"
in `DOCS.md`.

## Cloud (Opt-In Replication)

Cloud is optional and always project-scoped (`--project` required; `engram sync --cloud --all` is blocked). Local SQLite stays authoritative; cloud is replication / shared access only.

```bash
docker compose -f docker-compose.cloud.yml up -d
engram cloud config --server http://127.0.0.1:18080
engram cloud enroll smoke-project
engram sync --cloud --project smoke-project
```

Authenticated mode, upgrade flow, dashboard, and env details → [Engram Cloud docs](docs/engram-cloud/README.md) · [DOCS.md — Cloud CLI](DOCS.md#cloud-cli-opt-in).

## CLI & Environment Reference

| Command | Description |
| --- | --- |
| `engram setup [agent]` | Install agent integration (writes MCP config + plugin) |
| `engram memorylake config\|enable\|disable\|status` | Configure and route the per-project MemoryLake backend |
| `engram mcp [--tools=PROFILE] [--project NAME]` | Start MCP server (stdio) — agents launch this |
| `engram serve [port]` | Start HTTP API (default 7437, binds 127.0.0.1) |
| `engram tui` | Launch terminal UI |
| `engram save` / `search` / `context` / `timeline` / `stats` | Core memory operations |
| `engram sync [--import\|--status\|--cloud]` | Git chunk sync / optional cloud sync |
| `engram conflicts <sub>` · `engram doctor` | Conflict relations · read-only diagnostics |
| `engram cloud <sub>` · `engram projects <sub>` | Opt-in cloud · project-name management |
| `engram version` | Show version |

| Key env var | Description | Default |
| --- | --- | --- |
| `ENGRAM_DATA_DIR` | Data directory | `~/.engram` |
| `ENGRAM_BACKEND` | `sqlite` forces every project onto local SQLite | (per-project) |
| `ENGRAM_MEMORYLAKE_BASE_URL` | MemoryLake API base URL (overrides saved config) | `…/openapi/memorylake` |
| `ENGRAM_MEMORYLAKE_API_KEY` | MemoryLake API key (overrides saved config) | (unset) |
| `ENGRAM_MEMORYLAKE_WORKSPACE` | Workspace memories live under | `engram` |

Full CLI + env reference → [DOCS.md](DOCS.md#environment-variables).

## Documentation

| Doc | Description |
| --- | --- |
| [Install (中文)](docs/INSTALL.zh-CN.md) | Full install + Claude Code plugin + MemoryLake config walkthrough |
| [Releasing](RELEASING.md) | Tag-driven GitHub Release process |
| [Agent Setup](docs/AGENT-SETUP.md) | Per-agent configuration + Memory Protocol |
| [Architecture](docs/ARCHITECTURE.md) · [Codebase Guide](docs/CODEBASE-GUIDE.md) | How it works, package ownership, flows |
| [Plugins](docs/PLUGINS.md) | OpenCode & Claude Code plugin details |
| [Engram Cloud](docs/engram-cloud/README.md) | Cloud landing page + quickstart |
| [Full Docs](DOCS.md) | Complete reference (API, schema, CLI, env, **MemoryLake Backend**) |
| [Contributing](CONTRIBUTING.md) | Contribution workflow + standards |

> **Dashboard contributors**: after editing `.templ` files in `internal/cloud/dashboard/`, run `make templ` to regenerate before committing.

## License

MIT — a fork of [Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram) adding the per-project MemoryLake backend. Originally inspired by [claude-mem](https://github.com/thedotmack/claude-mem).
