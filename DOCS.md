[← Back to README](README.md)

# Engram — Technical Reference

**Persistent memory for AI coding agents**

This is the complete technical reference for Engram. For getting started, see the [README](README.md). For per-agent setup, see [Agent Setup](docs/AGENT-SETUP.md).

---

## Quick Navigation

| Section                                                   | What you'll find                                             |
| --------------------------------------------------------- | ------------------------------------------------------------ |
| [Database Schema](#database-schema)                       | Tables, FTS5, SQLite config                                  |
| [HTTP API](#http-api-endpoints)                           | All REST endpoints with request/response details             |
| [MCP Tools](#mcp-tools-20-tools)                          | Detailed reference for all 20 memory tools                   |
| [MCP Project Resolution](#mcp-project-resolution)         | Auto-detection algorithm, response envelope, tool categories |
| [Memory Protocol](#memory-protocol)                       | When/how agents should use the tools                         |
| [Project Name Normalization](#project-name-normalization) | Auto-detection, normalization, similar-project warnings      |
| [Features](#features)                                     | FTS5 search, timeline, privacy, git sync, compression        |
| [TUI](#terminal-ui-tui)                                   | Screens, navigation, architecture                            |
| [Running as a Service](#running-as-a-service)             | systemd setup                                                |
| [MemoryLake Backend](#memorylake-backend)                 | Optional per-project cloud backend, config, known limitations |
| [Design Decisions](#design-decisions)                     | Why Go, why SQLite, why no raw auto-capture                  |

For other docs:

| Doc                                         | Description                                                                                   |
| ------------------------------------------- | --------------------------------------------------------------------------------------------- |
| [Installation](docs/INSTALLATION.md)        | All install methods + platform support                                                        |
| [Engram Cloud](docs/engram-cloud/README.md) | Cloud landing page, quickstart path, branding, and reference links                            |
| [Agent Setup](docs/AGENT-SETUP.md)          | Per-agent configuration + compaction survival                                                 |
| [Codebase Guide](docs/CODEBASE-GUIDE.md)    | Definitive guide to repository structure, package ownership, flows, and maintainer guardrails |
| [Architecture](docs/ARCHITECTURE.md)        | How it works, session lifecycle, CLI reference, project structure                             |
| [Plugins](docs/PLUGINS.md)                  | OpenCode & Claude Code plugin details                                                         |
| [Team Usage](docs/TEAM-USAGE.md)            | Scope conventions, language strategy, and sync behavior for collaborative teams               |
| [Comparison](docs/COMPARISON.md)            | Why Engram vs claude-mem                                                                      |

---

## Database Schema

### Tables

- **sessions** — `id` (TEXT PK), `project`, `directory`, `started_at`, `ended_at`, `summary`, `status`
- **observations** — `id` (INTEGER PK AUTOINCREMENT), `session_id` (FK), `type`, `title`, `content`, `tool_name`, `project`, `scope`, `topic_key`, `normalized_hash`, `revision_count`, `duplicate_count`, `last_seen_at`, `created_at`, `updated_at`, `deleted_at`
- **observations_fts** — FTS5 virtual table synced via triggers (`title`, `content`, `tool_name`, `type`, `project`)
- **user_prompts** — `id` (INTEGER PK AUTOINCREMENT), `session_id` (FK), `content`, `project`, `created_at`
- **prompts_fts** — FTS5 virtual table synced via triggers (`content`, `project`)
- **sync_chunks** — `target_key` (TEXT), `chunk_id` (TEXT), `imported_at`; composite PK (`target_key`, `chunk_id`) for target-scoped chunk tracking
- **memory_relations** — stores conflict-surfacing verdicts from `mem_judge`; columns include `id` (INTEGER PK AUTOINCREMENT), `sync_id` (TEXT UNIQUE), `source_id`, `target_id`, `relation`, `judgment_status` (`pending` | `judged` | `orphaned` | `ignored`), `reason`, `evidence`, `confidence`, `marked_by_actor`, `marked_by_kind`, `marked_by_model`, `session_id`. The SQLite table does not store a `project` column; project is carried in relation sync payloads and derived from joined observations for project-scoped listing. Syncs across machines via local chunks and via cloud autosync when the project is enrolled.
- **sync_apply_deferred** — holds pulled mutations that could not be applied locally due to a missing FK dependency (e.g. relation references an observation not yet present); columns: `sync_id` (TEXT PK), `entity`, `payload`, `apply_status` (`deferred` | `applied` | `dead`), `retry_count`, `last_error`, `last_attempted_at`, `first_seen_at`. Rows with `apply_status='dead'` have exceeded the retry cap (5 attempts) and will not be retried automatically.

### SQLite Configuration

- WAL mode for concurrent reads
- Busy timeout 5000ms
- Synchronous NORMAL
- Foreign keys ON

---

## HTTP API Endpoints

Engram exposes two different runtimes. Keep routes split by runtime:

- **Local runtime (`engram serve`, JSON on `127.0.0.1:7437`)**
  - `GET /health` (local service health)
  - includes memory CRUD/search/context endpoints documented below
  - includes `GET /sync/status` (local node sync status)
- **Cloud runtime (`engram cloud serve`)**
  - `GET /health` (cloud service health)
  - `GET /sync/pull`, `GET /sync/pull/{chunkID}`, `POST /sync/push`, `POST /sync/mutations/push`, `GET /sync/mutations/pull` (cloud sync transport)
  - `GET /dashboard/*` HTML routes (browser dashboard)

Dashboard route tree (`engram cloud serve`):

- Public
  - `GET /dashboard/health` — dashboard subsystem health
  - `GET /dashboard/login` — login surface (authenticated mode), redirects to `/dashboard/` when already authenticated
  - `POST /dashboard/login` — login submit (authenticated mode), redirect-only no-op in insecure mode
  - `POST /dashboard/logout` — clear session cookie and redirect to login
  - `GET /dashboard/static/*` — embedded CSS/JS assets
- Protected (requires dashboard session in authenticated mode; open in insecure mode)
  - `GET /dashboard` and `GET /dashboard/` — dashboard overview
  - `GET /dashboard/stats`
  - `GET /dashboard/activity`
  - `GET /dashboard/browser`
  - `GET /dashboard/browser/observations` (`HX-Request: true` returns fragment; plain GET returns full page)
  - `GET /dashboard/browser/sessions` (`HX-Request: true` returns fragment; plain GET returns full page)
  - `GET /dashboard/browser/sessions/{sessionID}`
  - `GET /dashboard/browser/prompts` (`HX-Request: true` returns fragment; plain GET returns full page)
  - `GET /dashboard/projects`
  - `GET /dashboard/projects/list` — HTMX partial; paginated project list with "Paused" badges
  - `GET /dashboard/projects/{project}`
  - `GET /dashboard/projects/{name}/observations` — HTMX partial for project detail
  - `GET /dashboard/projects/{name}/sessions` — HTMX partial for project detail
  - `GET /dashboard/projects/{name}/prompts` — HTMX partial for project detail
  - `GET /dashboard/contributors`
  - `GET /dashboard/contributors/list` — HTMX partial; paginated contributor list
  - `GET /dashboard/contributors/{contributor}`
  - `GET /dashboard/admin` (also requires admin token/session)
  - `GET /dashboard/admin/projects`
  - `GET /dashboard/admin/users` (admin-gated)
  - `GET /dashboard/admin/users/list` (admin-gated; HTMX partial)
  - `GET /dashboard/admin/health` (admin-gated)
  - `POST /dashboard/admin/projects/{name}/sync` (admin-gated; toggle sync enabled/disabled)
  - `GET /dashboard/admin/projects/{name}/sync/form` (admin-gated; HTMX partial)
  - `GET /dashboard/admin/audit-log` (admin-gated)
  - `GET /dashboard/admin/audit-log/list` (admin-gated; HTMX partial)
  - `GET /dashboard/sessions/{project}/{sessionID}` — session detail with observations + prompts sub-lists
  - `GET /dashboard/observations/{project}/{sessionID}/{syncID}` — observation detail
  - `GET /dashboard/prompts/{project}/{sessionID}/{syncID}` — prompt detail

Engram is local-first: local SQLite is authoritative; cloud features are optional replication/shared access and enrollment controls.

### Health

- Local runtime (`engram serve`): `GET /health` — Returns `{"status": "ok", "service": "engram", "version": "0.1.0"}`
- Cloud runtime (`engram cloud serve`): `GET /health` — Returns `{"status": "ok", "service": "engram-cloud"}`

### Sessions

- `POST /sessions` — Create session. Body: `{id, project, directory}`
- `POST /sessions/{id}/end` — End session. Body: `{summary}`
- `GET /sessions/recent` — Recent sessions. Query: `?project=X&limit=N`
- `GET /sessions/{id}` — Get single session by ID
- `DELETE /sessions/{id}` — Delete session
  - `200` when deleted
  - `404` when session does not exist
  - `409` when session still has observations (delete/migrate observations first)
  - For cloud-enrolled projects: returns `200` and additionally enqueues a `session/delete` mutation that propagates the deletion to cloud replicas

### Observations

- `POST /observations` — Add observation. Body: `{session_id, type, title, content, tool_name?, project?, scope?, topic_key?}`. Response `id` is the observation's **sync_id** (an opaque string, e.g. `obs-1a2b3c`) — use it for every subsequent by-id call below.
- `GET /observations` — Recent observations compatibility endpoint. Query: `?project=X&scope=project|personal|global&limit=N&sort=created_at:desc`
- `GET /observations/recent` — Recent observations. Query: `?project=X&scope=project|personal|global&limit=N`
- `GET /observations/{id}` — Get single observation by sync_id
- `PATCH /observations/{id}` — Update fields by sync_id. Body: `{title?, content?, type?, project?, scope?, topic_key?}`
- `DELETE /observations/{id}` — Delete observation by sync_id (`?hard=true` for hard delete, soft delete by default)
  - `200` when deleted
  - `404` when observation does not exist

  `{id}` is an opaque sync_id string, not a numeric row id (this is what lets a MemoryLake-enabled project's observations — which have no int64 id — be addressed the same way as sqlite's). Any string is syntactically valid; an unknown one is a `404`, never a `400`.

### Review

- `GET /review` — List observations due for local review. Query: `?project=X&limit=N`
- `POST /review/mark_reviewed` — Reset one observation's local review cycle. Body: `{observation_id}` (sync_id string); legacy `{id}` is accepted.
  - `200` with the refreshed observation payload when marked reviewed
  - `400` when `observation_id`/`id` is missing or the JSON body is invalid
  - `404` when the observation does not exist
  - Local-only: updating `review_after` does not enqueue a sync mutation or propagate to other machines.

### Search

- `GET /search` — FTS5 search. Query: `?q=QUERY&type=TYPE&project=PROJECT&scope=SCOPE&limit=N`

### Timeline

- `GET /timeline` — Chronological context. Query: `?observation_id=<sync_id>&before=5&after=5`

### Prompts

- `POST /prompts` — Save user prompt. Body: `{session_id, content, project?}`
- `GET /prompts/recent` — Recent prompts. Query: `?project=X&limit=N`
- `GET /prompts/search` — Search prompts. Query: `?q=QUERY&project=X&limit=N`
- `DELETE /prompts/{id}` — Delete prompt
  - `200` when deleted
  - `400` for invalid prompt id
  - `404` when prompt does not exist

### Context

- `GET /context` — Formatted context. Query: `?project=X&scope=project|personal|global`

### Passive Capture

- `POST /observations/passive` — Extract structured learnings from text. Body: `{content, session_id, project?}`

### Export / Import

- `GET /export` — Export all data as JSON
  - Optional `?project=<name>` for project-scoped export
  - `400` when `project` is provided but blank/whitespace
- `POST /import` — Import data from JSON. Body: ExportData JSON

### Stats / Diagnostics

- `GET /stats` — Memory statistics
- `GET /doctor` — Read-only operational diagnostics. Query: `?project=X&check=CHECK_CODE`
  - Returns the same diagnostic report envelope as `engram doctor --json` and MCP `mem_doctor`
  - `project` and `check` are optional; omitted `project` uses current project detection
  - Unknown explicit projects return `404` with `{error, code:"unknown_project", available_projects:[...]}`

### Project Detection / Migration

- `GET /project/current` — Detect the current project. Query: `?cwd=/path/to/repo`
  - Always returns a success envelope with `{project, project_source, project_path, cwd, available_projects}` plus optional `warning`/`error_hint`
- `POST /projects/migrate` — Migrate observations between project names. Body: `{old_project, new_project}`

### Conflict Audit (admin — local runtime only)

These endpoints are served by `engram serve` on the local runtime only. They are not exposed on the cloud runtime. All routes are additive — no existing routes changed.

#### GET /conflicts

List `memory_relations` rows with optional filters.

Query params: `project` (string), `status` (string — raw `judgment_status`, currently `pending` | `judged` | `orphaned` | `ignored`), `since` (RFC3339), `limit` (int, default 50, max 500 — silently clamped), `offset` (int, default 0).

Response:

```json
{
  "total": 80,
  "limit": 50,
  "offset": 0,
  "relations": [
    {
      "id": 42,
      "sync_id": "rel-abc123",
      "relation": "conflicts_with",
      "judgment_status": "pending",
      "source_id": "obs-source123",
      "source_title": "Original architecture decision",
      "target_id": "obs-target456",
      "target_title": "Updated architecture decision",
      "created_at": "2026-01-15 12:00:00",
      "updated_at": "2026-01-15 12:30:00"
    }
  ]
}
```

#### POST /conflicts/judge

Record a verdict on an existing pending relation surfaced by memory conflict detection.

Body:

```json
{
  "judgment_id": "rel-abc123",
  "relation": "related|compatible|scoped|conflicts_with|supersedes|not_conflict",
  "reason": "optional explanation",
  "evidence": "optional JSON or text evidence",
  "confidence": 0.9,
  "session_id": "optional-session-id"
}
```

Response:

```json
{ "relation": { "sync_id": "rel-abc123", "judgment_status": "judged" } }
```

Status codes:

- `200` when judged
- `400` for invalid JSON, missing required fields, unknown relation, or invalid relation state

#### POST /conflicts/compare

Persist an agent-supplied semantic verdict for two observations, addressed by sync_id.

Body:

```json
{
  "memory_id_a": "obs-abc123",
  "memory_id_b": "obs-def456",
  "relation": "related|compatible|scoped|conflicts_with|supersedes|not_conflict",
  "confidence": 0.99,
  "reasoning": "brief explanation",
  "model": "optional-model-id"
}
```

Response:

```json
{ "sync_id": "rel-abc123" }
```

`not_conflict` is a no-op verdict and returns an empty `sync_id`.

Status codes:

- `200` when accepted
- `400` for invalid JSON, missing required fields, invalid relation, invalid confidence, or cross-project pairs
- `404` when either observation ID does not exist

#### GET /conflicts/{relation_id}

Get full detail for one relation row, including source and target observation snippets.

- `200` with full relation + `source_snippet` + `target_snippet`
- `404` with a JSON `error` containing the not-found message when `relation_id` does not exist
- `400` with JSON error body when `relation_id` is not a valid integer

#### GET /conflicts/stats

Aggregate counts for the project (or global when `project` query param is omitted).

Response:

```json
{
  "project": "my-project",
  "by_relation": {
    "conflicts_with": 3,
    "supersedes": 1
  },
  "by_judgment_status": {
    "pending": 3,
    "judged": 1
  },
  "deferred": 4,
  "dead": 1
}
```

#### POST /conflicts/scan

Run conflict candidate scan for a project. Synchronous.

Request body:

```json
{
  "project": "my-project",
  "apply": false,
  "max_insert": 100,
  "semantic": false,
  "concurrency": 5,
  "timeout_per_call_seconds": 60,
  "max_semantic": 100
}
```

- `apply: false` (default) — dry-run for the non-semantic lexical scan; reports candidates without inserting pending rows
- `apply: true` — non-semantic lexical scan inserts new pending relation rows up to `max_insert` cap (default 100)
- `semantic: true` — after FTS5 lexical scan, run LLM-judge semantic detection on the candidate pairs returned by `FindCandidates`. It does not discover totally lexically unrelated pairs on its own. Requires `ENGRAM_AGENT_CLI` to be set on the server to `claude` or `opencode`.
- Semantic scans can persist non-`not_conflict` judged relations through `JudgeBySemantic` even when `apply: false`; `not_conflict` verdicts are not inserted.
- `concurrency` — worker pool size for parallel LLM calls when `semantic: true` (default 5, range 1–20)
- `timeout_per_call_seconds` — per-LLM-call timeout in seconds when `semantic: true` (default 60, range 1–600)
- `max_semantic` — hard cap on LLM calls per scan (default 100); scan stops collecting new pairs once reached
- Missing `project` field returns `400`
- With `semantic: true`, `concurrency` outside [1, 20] or `timeout_per_call_seconds` outside [1, 600] returns `400`

Response:

```json
{
  "project": "my-project",
  "inspected": 25,
  "candidates_found": 5,
  "already_related": 2,
  "inserted": 0,
  "capped": false,
  "dry_run": true,
  "semantic_judged": 0,
  "semantic_skipped": 0,
  "semantic_errors": 0
}
```

`semantic_judged`, `semantic_skipped`, and `semantic_errors` are always present (zero when `semantic: false`).

When any scan cap is reached, including `max_insert` for lexical apply scans or `max_semantic` for semantic scans, a `warning` field is included:

```json
{
  "project": "my-project",
  "inspected": 250,
  "candidates_found": 150,
  "already_related": 0,
  "inserted": 50,
  "capped": true,
  "dry_run": false,
  "semantic_judged": 0,
  "semantic_skipped": 0,
  "semantic_errors": 0,
  "warning": "cap reached: not all candidates were inserted"
}
```

#### GET /conflicts/deferred

List rows from `sync_apply_deferred`. Query params: `status` (string — `deferred` | `dead` | `applied`), `limit` (int, default 50, max 500), `offset` (int, default 0; accepted for pagination but not echoed in the response envelope).

Response:

```json
{
  "total": 3,
  "limit": 50,
  "rows": [
    {
      "sync_id": "rel-abc123",
      "entity": "relation",
      "payload": {
        "sync_id": "rel-abc123",
        "source_id": "obs-source123",
        "target_id": "obs-target456",
        "relation": "conflicts_with",
        "judgment_status": "pending",
        "project": "my-project",
        "created_at": "2026-01-15 12:00:00",
        "updated_at": "2026-01-15 12:00:00"
      },
      "payload_raw": "{\"sync_id\":\"rel-abc123\",\"source_id\":\"obs-source123\",\"target_id\":\"obs-target456\",\"relation\":\"conflicts_with\",\"judgment_status\":\"pending\",\"project\":\"my-project\",\"created_at\":\"2026-01-15 12:00:00\",\"updated_at\":\"2026-01-15 12:00:00\"}",
      "payload_valid": true,
      "apply_status": "deferred",
      "retry_count": 2,
      "last_error": "source FK not found",
      "last_attempted_at": "2026-01-15 12:05:00",
      "first_seen_at": "2026-01-15 12:00:00"
    }
  ]
}
```

#### POST /conflicts/deferred/replay

Call `ReplayDeferred()` synchronously. Returns counts of rows processed.

Response:

```json
{
  "retried": 4,
  "succeeded": 3,
  "failed": 0,
  "dead": 1
}
```

### Sync Status (local runtime only)

- `GET /sync/status` — Runtime sync-state status for the local node (`engram serve` only).
- In `engram serve`, sync status is wired to persisted SQLite sync state (project-scoped for detected/current project).
- Response fields when provider is injected:
  - `enabled`
  - `phase`
  - `last_error`
  - `consecutive_failures`
  - `backoff_until`
  - `last_sync_at`
  - `reason_code`
  - `reason_message`
  - `deferred_count` — number of pulled mutations awaiting retry (FK dependency not yet local)
  - `dead_count` — number of pulled mutations that exhausted retries (5 failures) and will not be retried
  - `upgrade` (nested object)
    - `stage`
    - `reason_code`
    - `reason_message`
- `enabled` semantics:
  - `true` when cloud runtime is configured for the resolved + enrolled project, or when meaningful persisted sync state exists for that resolved project while runtime is not configured.
  - `false` when no explicit project scope resolves, cloud runtime is malformed/missing, or enrollment/status checks fail.
- Generic/embedded local server usage may return the fallback `enabled=false` response if no provider is injected.

### Environment Variables

| Variable                        | Description                                                                                                                                                                                                                                               | Default              |
| ------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------- |
| `ENGRAM_DATA_DIR`               | Override data directory                                                                                                                                                                                                                                   | `~/.engram`          |
| `ENGRAM_PORT`                   | Override HTTP server port                                                                                                                                                                                                                                 | `7437`               |
| `ENGRAM_PROJECT`                | Process-level default project override. For `engram serve`: used as the fallback when `GET /sync/status` receives no `project` query param. For `engram mcp`: sets `MCPConfig.DefaultProject`, which takes precedence over cwd detection for all read and write tools for the lifetime of that MCP process. When unset, cwd detection is used as the fallback. | cwd-detected project |
| `ENGRAM_HTTP_TOKEN`             | Optional Bearer auth for the local HTTP server. When set, the following routes require `Authorization: Bearer <token>`: `DELETE /sessions/{id}`, `DELETE /observations/{id}`, `DELETE /prompts/{id}`, `GET /export`, `POST /import`, `POST /projects/migrate`. Comparison is constant-time. Token is read at request time (no restart needed). When unset, all routes are open (zero-config default). | (unset — open) |
| `ENGRAM_TIMEZONE`               | Timezone for timestamp display in the TUI and cloud dashboard. Accepts any IANA zone name (e.g. `America/New_York`, `Europe/Berlin`). Falls back to system local time when unset or invalid.                                                               | system local         |
| `ENGRAM_AGENT_CLI`              | LLM runner name used by `engram conflicts scan --semantic` and the HTTP `/conflicts/scan` endpoint. Accepted values: `claude`, `opencode`.                                                                                                                | (unset)              |
| `ENGRAM_MCP_AUDIT_LOG`          | Opt-in audit trail for `engram mcp`. When set to a file path, one `<RFC3339> <tool>` line is appended per `mem_*` tool **call arrival** — it counts attempts, not successful writes, so a call that later errors or is rejected still produces a line. Best-effort: any file/permission failure is silently ignored and never fails the tool call. Primarily intended for the eval harness to measure per-session `mem_*` usage. | (unset — disabled)   |
| `ENGRAM_BACKEND`                | Global safety valve for the [MemoryLake backend](#memorylake-backend). Set to `sqlite` to force every project onto local SQLite regardless of per-project enablement. Any other value (or unset) defers to the per-project enablement list.             | (unset — per-project) |
| `ENGRAM_MEMORYLAKE_BASE_URL`    | MemoryLake V3 API base URL. Required to enable the MemoryLake backend for any project.                                                                                                                                                                   | (unset)              |
| `ENGRAM_MEMORYLAKE_API_KEY`     | MemoryLake API key (`Authorization: Bearer <key>`). Required to enable the MemoryLake backend for any project.                                                                                                                                            | (unset)              |
| `ENGRAM_MEMORYLAKE_WORKSPACE`   | MemoryLake workspace custom_id or `ws-...` id that MemoryLake-backed memories are stored under.                                                                                                                                                           | `engram`             |
| `ENGRAM_MEMORYLAKE_ACTOR`       | MemoryLake actor custom_id used for writes/reads from this machine. Falls back to hostname when unset.                                                                                                                                                   | (unset — hostname)   |
| `ENGRAM_MEMORYLAKE_TIMEOUT_MS`  | Per-request HTTP timeout for the MemoryLake client.                                                                                                                                                                                                       | `30000`              |
| `ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS` | Poll interval while waiting for MemoryLake to extract a fact from a newly written message.                                                                                                                                                            | `2000`               |
| `ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS` | Max total time to wait for fact extraction before returning a provisional (pending) result.                                                                                                                                                       | `30000`              |
| `ENGRAM_CLOUD_AUTOSYNC`         | Set to `1` to enable background autosync. Requires `ENGRAM_CLOUD_TOKEN` and `ENGRAM_CLOUD_SERVER` to also be set.                                                                                                                                         | (unset — disabled)   |
| `ENGRAM_CLOUD_SERVER`           | Cloud server URL used by the autosync manager and `engram sync --cloud`.                                                                                                                                                                                  | (unset)              |
| `ENGRAM_DATABASE_URL`           | Postgres DSN for `engram cloud serve`.                                                                                                                                                                                                                    | (unset)              |
| `ENGRAM_CLOUD_HOST`             | Bind host for `engram cloud serve`.                                                                                                                                                                                                                       | `127.0.0.1`          |
| `ENGRAM_CLOUD_MAX_PUSH_BYTES`   | Max cloud push payload bytes.                                                                                                                                                                                                                             | `8388608`            |
| `ENGRAM_CLOUD_TOKEN`            | Bearer token required in authenticated `engram cloud serve` mode.                                                                                                                                                                                         | (unset)              |
| `ENGRAM_CLOUD_INSECURE_NO_AUTH` | Set to `1` for local insecure cloud serve (no auth). Cannot be combined with `ENGRAM_CLOUD_TOKEN`.                                                                                                                                                        | (unset)              |
| `ENGRAM_CLOUD_ALLOWED_PROJECTS` | Comma-separated project allowlist enforced by `engram cloud serve`. Required in both token-auth and insecure modes. Use `*` to allow all projects (dev/internal deploys) — bypasses per-project name enforcement while still requiring a non-empty project on each request. | (unset) |
| `ENGRAM_JWT_SECRET`             | Required in authenticated cloud serve mode. Must be explicitly set to a non-default value.                                                                                                                                                                | (unset)              |
| `ENGRAM_CLOUD_ADMIN`            | Optional admin-only dashboard token in authenticated cloud serve mode. Ignored/rejected in insecure mode.                                                                                                                                                 | (unset)              |
| `ENGRAM_CLOUD_TOKEN_PEPPER`     | Dedicated secret used to hash managed cloud tokens. Required both to issue tokens via `engram cloud bootstrap admin --issue-token` (and the admin API/dashboard) and to enable managed-token authentication on `engram cloud serve`. Distinct from `ENGRAM_JWT_SECRET` on purpose — see [Managed users, tokens, and CLI bootstrap](#managed-users-tokens-and-cli-bootstrap). | (unset)              |

### Conflict Audit CLI (admin)

The `engram conflicts` sub-command provides admin/maintainer access to the conflict layer. It is NOT for end users — end users interact with conflicts via the normal agent conversation flow.

When `--project` is omitted, the cwd-detected project is used.

```
engram conflicts list [--project <name>] [--status <pending|judged|orphaned|ignored>] [--since <RFC3339>] [--limit <N>]
```

List `memory_relations` rows. Output: label-colon aligned columns (`id`, `sync_id`, `relation`, `judgment_status`, `source`, `target`, `created_at`).

```
engram conflicts show <relation_id>
```

Show full detail for one relation: relation_id, sync_id, relation, judgment_status, created_at, updated_at, source_id, source_title, target_id, target_title. Exits non-zero when relation_id does not exist.

```
engram conflicts stats [--project <name>]
```

Print aggregate grouped `judgment_status` counts (`pending` | `judged` | `orphaned` | `ignored`) plus deferred and dead queue sizes. When relation counts exist, also prints `By relation type` counts.

```
engram conflicts scan [--project <name>] [--dry-run] [--apply] [--max-insert <N>]
                      [--since <RFC3339>]
                      [--semantic] [--concurrency <N>] [--timeout-per-call <N>]
                      [--max-semantic <N>] [--yes]
```

Walk observations for the project, run FindCandidates, and report or insert new pending relation rows.

- `--dry-run` (default): for non-semantic lexical scans, reports candidates found with 0 pending rows inserted.
- `--apply`: inserts up to `--max-insert` (default 100) new rows; prints WARNING when cap is reached.
- `--since RFC3339`: scan only observations created at or after the timestamp.
- `--semantic`: enable LLM-judge semantic detection on FTS5 candidate pairs returned by `FindCandidates`. It can improve verdict quality for candidates that share lexical terms, but it does not discover totally lexically unrelated pairs on its own. Requires `ENGRAM_AGENT_CLI=claude` or `ENGRAM_AGENT_CLI=opencode`.
- With `--semantic`, non-`not_conflict` verdicts are persisted by `JudgeBySemantic` even in the default `--dry-run` mode; `not_conflict` verdicts remain no-op.
- `--concurrency N`: worker pool size for parallel LLM calls (default 5, max 20).
- `--timeout-per-call N`: per-LLM-call timeout in seconds (default 60).
- `--max-semantic N`: hard cap on LLM calls per scan run (default 100).
- `--yes`: skip the cost-estimate confirmation prompt before LLM calls.

```
engram conflicts deferred [--status <deferred|dead|applied>] [--limit <N>] [--inspect <sync_id>] [--replay]
```

Inspect or replay the `sync_apply_deferred` queue.

- Default: list rows with sync_id, apply_status, retry_count, first_seen_at.
- `--inspect <sync_id>`: print full decoded payload for one row; exits non-zero when not found.
- `--replay`: call `ReplayDeferred()` and print retried/succeeded/failed/dead counts.

### Cloud CLI (opt-in)

- `engram cloud status` — show current cloud config state plus auth/sync readiness without mutating local state. When cloud is configured, also probes the local `engram serve` daemon at `127.0.0.1:7437` (respects `ENGRAM_PORT`) and prints a `Local daemon:` line (`running` / `not running` / `unreachable`) so you can detect a silently dead autosync. Exit code is unaffected; the line is informational
- `engram cloud enroll <project>` — enroll one project for cloud replication
- `engram cloud config --server <url>` — persist cloud server URL to `~/.engram/cloud.json`
- `engram cloud serve` — run cloud backend API + dashboard (`/dashboard`) using Postgres config from env
- `engram cloud upgrade doctor --project <project>` — deterministic read-only readiness diagnosis (`ready|blocked`, class/reason)
- `engram cloud upgrade repair --project <project> [--dry-run|--apply]` — deterministic local-safe repair planner/apply (no remote mutation)
- `engram cloud upgrade bootstrap --project <project> [--resume]` — resumable checkpointed enroll/push/verify flow
- `engram cloud upgrade status --project <project>` — show upgrade stage/class/reason
- `engram cloud upgrade rollback --project <project>` — restore pre-upgrade local snapshot before `bootstrap_verified`; blocked afterwards
- `engram cloud repair materialize-mutations --project <project> (--dry-run|--apply)` — explicit server-side Postgres repair that backfills existing `cloud_mutations` into compatible `cloud_chunks` without deleting remote data
- `engram cloud bootstrap admin --username <name> [--email <email>] [--grant-project <project>]... [--issue-token [name]]` — create the first managed admin (see [Managed users, tokens, and CLI bootstrap](#managed-users-tokens-and-cli-bootstrap))

Cloud auth token is provided at runtime via `ENGRAM_CLOUD_TOKEN` (not by a dedicated CLI subcommand).
Cloud server startup fails closed when the token is missing unless `ENGRAM_CLOUD_INSECURE_NO_AUTH=1` is explicitly set for local insecure development.
`ENGRAM_CLOUD_INSECURE_NO_AUTH=1` cannot be combined with `ENGRAM_CLOUD_TOKEN`.
Cloud server always requires `ENGRAM_CLOUD_ALLOWED_PROJECTS` (comma-separated), including insecure mode, so project scope remains server-enforced.
`ENGRAM_CLOUD_TOKEN` + `ENGRAM_CLOUD_ALLOWED_PROJECTS` are server-side requirements for authenticated mode and must be configured before `engram cloud serve` (or compose startup).
Authenticated mode also requires an explicit non-default `ENGRAM_JWT_SECRET`; implicit development defaults are rejected.
Dashboard requests support browser login in authenticated mode: use `/dashboard/login` to exchange the bearer token for an HttpOnly dashboard cookie scoped to `/dashboard`. Protected `/dashboard/*` HTML routes require that cookie and do **not** treat raw `Authorization: Bearer ...` headers as an authenticated browser session. Sync API routes (`/sync/pull`, `/sync/pull/{chunkID}`, `/sync/push`, `/sync/mutations/push`, `/sync/mutations/pull`) remain header-auth only. In insecure mode (`ENGRAM_CLOUD_INSECURE_NO_AUTH=1` + no `ENGRAM_CLOUD_TOKEN`), dashboard auth is bypassed and `/dashboard/login` redirects to `/dashboard/`.

`ENGRAM_CLOUD_ADMIN` is optional in authenticated mode; when set, `/dashboard/admin` is allowed only for sessions established with that exact token.
`ENGRAM_CLOUD_ADMIN` is rejected in insecure mode (`ENGRAM_CLOUD_INSECURE_NO_AUTH=1`) to avoid an incoherent admin/browser auth path.

Cloud runtime bind host is controlled by `ENGRAM_CLOUD_HOST`:

- default: `127.0.0.1` (local-only, safer default)
- container/compose: set `ENGRAM_CLOUD_HOST=0.0.0.0` so published host ports can reach the cloud server

Cloud runtime envs for `engram cloud serve`:

| Variable                        | Required                 | Notes                                                                                 |
| ------------------------------- | ------------------------ | ------------------------------------------------------------------------------------- |
| `ENGRAM_DATABASE_URL`           | yes                      | Postgres DSN for cloud chunk storage/dashboard read model                             |
| `ENGRAM_PORT`                   | no                       | Runtime port (default `8080`)                                                         |
| `ENGRAM_CLOUD_HOST`             | no                       | Bind host (default `127.0.0.1`; use `0.0.0.0` for containers)                         |
| `ENGRAM_CLOUD_MAX_PUSH_BYTES`   | no                       | Max chunk/mutation push request body bytes (default `8388608`)                        |
| `ENGRAM_CLOUD_ALLOWED_PROJECTS` | yes                      | Comma-separated allowlist; always required (authenticated + insecure modes). Use `*` to allow all projects (dev/internal deploys) — bypasses per-project name enforcement while still requiring a non-empty project on each request. |
| `ENGRAM_CLOUD_TOKEN`            | yes (authenticated mode) | Enables bearer auth mode                                                              |
| `ENGRAM_JWT_SECRET`             | yes (authenticated mode) | Must be explicitly set and non-default when token mode is enabled                     |
| `ENGRAM_CLOUD_INSECURE_NO_AUTH` | no                       | Set to `1` only for local insecure mode; cannot be combined with `ENGRAM_CLOUD_TOKEN` |
| `ENGRAM_CLOUD_ADMIN`            | no                       | Optional admin dashboard token in authenticated mode; rejected in insecure mode       |
| `ENGRAM_CLOUD_TOKEN_PEPPER`     | no (required to enable managed-token auth) | Dedicated managed-token hashing secret. Must differ from `ENGRAM_JWT_SECRET`. Required both by `engram cloud bootstrap admin --issue-token` and by `engram cloud serve` to accept managed tokens at runtime (see below). |

### Managed users, tokens, and CLI bootstrap

`engram cloud bootstrap admin` creates the first **managed admin** — a principal record stored in the cloud Postgres database (`cloud_principals` / `cloud_human_users`), independent from the legacy `ENGRAM_CLOUD_TOKEN` / `ENGRAM_CLOUD_ADMIN` env-token model:

```bash
# Create the first managed admin (safe: refuses to create a duplicate first admin)
engram cloud bootstrap admin --username alice

# Also grant project access and issue a sync token in the same command
engram cloud bootstrap admin --username alice \
  --grant-project my-project \
  --issue-token first-token
```

- `--username` is required; `--email` is optional.
- `--grant-project <project>` may be repeated to grant one or more projects (managed principals are deny-by-default: no grants means no sync access).
- `--issue-token [name]` issues a managed bearer token and prints the **raw token exactly once** in the command output. It is never logged, persisted, or re-printed — store it immediately. Issuing a token requires `ENGRAM_CLOUD_TOKEN_PEPPER` to be set to a dedicated secret (distinct from `ENGRAM_JWT_SECRET`); the command fails clearly, before creating anything, if the pepper is missing.
- Running the command again once a managed admin already exists is rejected (no silent duplicate first-admin creation); the attempt is still recorded as a denied `bootstrap.cli` audit event.
- Every bootstrap attempt (accepted or denied) writes a `bootstrap.cli` audit event to `cloud_auth_audit_log`, with the same non-secret metadata rules (no raw tokens, hashes, or bearer headers) as every other cloud auth audit event.
- Grant/role/duplicate-admin validation reuses the exact same `cloudstore` methods and last-admin guard used by the dashboard's own first-admin bootstrap flow — there is no parallel/looser bootstrap path.

**Runtime authentication:** `engram cloud serve` resolves managed tokens first, then falls back to the legacy env-token credentials (`ENGRAM_CLOUD_TOKEN` for sync, `ENGRAM_CLOUD_ADMIN` for dashboard bootstrap/admin), on every `/sync/*`, `/admin/*`, and dashboard-login request:

- Set `ENGRAM_CLOUD_TOKEN_PEPPER` to enable managed-token authentication. A token issued by `engram cloud bootstrap admin --issue-token` (or by the dashboard/`/admin/*` token-create routes) then authenticates directly against `/sync/*` and `/admin/*`, and can log into the dashboard as its resolved principal/role.
- If `ENGRAM_CLOUD_TOKEN_PEPPER` is not set, managed-token authentication is simply disabled: the server still starts normally, and `ENGRAM_CLOUD_TOKEN` / `ENGRAM_CLOUD_ADMIN` continue to authenticate exactly as before (legacy-only mode).
- Managed principals are deny-by-default for project sync: a managed token only reaches projects explicitly granted via `--grant-project` (or the dashboard/`/admin/*` grant routes). Legacy `ENGRAM_CLOUD_TOKEN` keeps its existing `ENGRAM_CLOUD_ALLOWED_PROJECTS` allowlist behavior, unaffected by managed grants.
- Disabled managed users, revoked managed tokens, and revoked project grants stop authenticating/authorizing on the very next request — no server restart required.
- No rollback action is required to keep using legacy credentials: legacy `ENGRAM_CLOUD_TOKEN` / `ENGRAM_CLOUD_ADMIN` behavior is unchanged and remains fully supported whether or not `ENGRAM_CLOUD_TOKEN_PEPPER` is configured.

Cloud sync is still local-first and explicit:

```bash
# Explicit cloud sync call
engram sync --cloud --project my-project

# Optional env toggle for cloud mode in sync command
ENGRAM_CLOUD_SYNC=1 engram sync --status --project my-project
```

When `engram sync --cloud --project <project>` or autosync hits a known repairable cloud sync/upsert/canonicalization failure, Engram preserves the original error and appends guidance to run:

### Cloud Upgrade Flow

```bash
engram cloud upgrade doctor --project <project>
engram cloud upgrade repair --project <project> --dry-run
engram cloud upgrade repair --project <project> --apply
engram sync --cloud --project <project>
```

Sync/autosync never auto-applies repairs; only the explicit `repair --apply` command mutates local repairable upgrade state.

For cloud servers that already accepted mutation pushes before mutation payloads were materialized into chunk history, run the server-side backfill against the Postgres DSN used by `engram cloud serve`:

```bash
ENGRAM_DATABASE_URL='postgres://...' engram cloud repair materialize-mutations --project <project> --dry-run
ENGRAM_DATABASE_URL='postgres://...' engram cloud repair materialize-mutations --project <project> --apply
```

The backfill is project-scoped, non-destructive, and idempotent: it inserts missing compatible chunks and leaves existing `cloud_mutations` and chunks in place.

`engram cloud serve` also runs this materialization repair automatically for every configured `ENGRAM_CLOUD_ALLOWED_PROJECTS` entry at startup. The explicit repair command remains available for operator verification, dry-runs, and re-running a project after an upgrade.

### Local Cloud Bring-Up (Docker + Postgres)

```bash
# 1) SERVER-SIDE startup requirements (configure before startup)
# docker-compose.cloud.yml includes defaults for browser-demo smoke usage:
# ENGRAM_CLOUD_INSECURE_NO_AUTH=1
# ENGRAM_CLOUD_ALLOWED_PROJECTS=smoke-project
docker compose -f docker-compose.cloud.yml up -d

# source-run flow (without compose): set BOTH token + allowlist before startup
# ENGRAM_DATABASE_URL="postgres://engram:engram_dev@127.0.0.1:5433/engram_cloud?sslmode=disable" \
# ENGRAM_JWT_SECRET="replace-with-32+-byte-random-secret" \
# ENGRAM_CLOUD_TOKEN="your-token" \
# ENGRAM_CLOUD_ALLOWED_PROJECTS="my-project" \
# engram cloud serve

# 2) CLIENT-SIDE CLI setup
# compose runtime flow: published :18080
engram cloud config --server http://127.0.0.1:18080
# compose runtime default is insecure local-dev mode; keep token unset
# client sync preflight only requires the configured cloud server URL; no
# client-side ENGRAM_CLOUD_INSECURE_NO_AUTH flag is required for compose flow
unset ENGRAM_CLOUD_TOKEN

# 3) Enroll project + run explicit cloud sync
engram cloud enroll smoke-project
engram cloud upgrade doctor --project smoke-project
engram cloud upgrade repair --project smoke-project --dry-run
engram cloud upgrade repair --project smoke-project --apply
engram cloud upgrade bootstrap --project smoke-project --resume
engram cloud upgrade status --project smoke-project
engram sync --cloud --status --project smoke-project

# source-run client endpoint (without compose): default :8080
# engram cloud config --server http://127.0.0.1:8080

# cloud mode enforces a single explicit project scope
# engram sync --cloud --all  # blocked by design
```

Deterministic reason codes shared across store/CLI/server:

- `blocked_unenrolled`
- `auth_required`
- `cloud_config_error`
- `policy_forbidden`
- `paused`
- `transport_failed`

### Cloud Status Visibility Matrix

Cloud failure visibility must stay deterministic across supported surfaces:

| Scenario                                                                                               | Expected deterministic reason        | Surfaces                    |
| ------------------------------------------------------------------------------------------------------ | ------------------------------------ | --------------------------- |
| Unconfigured cloud sync preflight (missing server URL)                                                 | `cloud_config_error`                 | CLI stderr                  |
| Cloud runtime not configured in status provider (takes precedence even if project scope is unresolved) | `cloud_not_configured`               | `/sync/status`              |
| `/sync/status` project cannot be resolved (no query/default project) while cloud runtime is configured | `project_required`                   | `/sync/status`              |
| Unenrolled project cloud sync                                                                          | `blocked_unenrolled`                 | CLI stderr + `/sync/status` |
| Runtime auth/policy failure from remote API                                                            | `auth_required` / `policy_forbidden` | CLI stderr + `/sync/status` |
| Explicit paused state                                                                                  | `paused`                             | `/sync/status`              |
| Remote/network failure                                                                                 | `transport_failed`                   | CLI stderr + `/sync/status` |

`engram sync --cloud --status --project <name>` is read-only: it does **not** mutate `/sync/status` lifecycle fields.

Machine-actionable validation/policy failures from cloud sync routes include:

- `error_class` (`repairable` | `blocked` | `policy` | `invalid_request`)
- `error_code` (stable deterministic code)
- `error` (human-readable message)

This envelope is used consistently by `/sync/push` validation/control failures and by `/sync/pull` / `/sync/pull/{chunkID}` project-required or policy failures. `/sync/mutations/push` uses the envelope for empty batches, empty projects, project policy failures, and pause-control failures; relation-payload validation currently returns `error`, `reason_code`, and `invalid` instead. `/sync/mutations/pull` success responses include the project envelope, but internal listing errors currently use plain `http.Error`.

---

## MCP Project Resolution

Engram resolves the project at MCP tool call time. The default source is the **server process working directory** (cwd), not MCP startup state, but some write tools have stronger context: `mem_session_start(directory=...)` resolves from the provided directory, and `mem_save` may use a validated explicit `project` or an existing `session_id` project before falling back to cwd detection. The explicit field is treated as a **validated selection**, not a free-form creation hint. This eliminates project drift caused by agents supplying different names for the same repo.

### Detection algorithm

| Case | Condition                                                                                 | Source            | Project                            |
| ---- | ----------------------------------------------------------------------------------------- | ----------------- | ---------------------------------- |
| 1    | nearest `.engram/config.json` exists within the enclosing git root, or at cwd outside git | `config`          | `project_name` from config         |
| 2    | cwd is a git root with `origin` remote                                                    | `git_remote`      | repo name from remote URL          |
| 3    | cwd is inside a git repo (subdirectory)                                                   | `git_root`        | git root's directory basename      |
| 4    | cwd has exactly one git-repo child                                                        | `git_child`       | child repo name (warning included) |
| 5    | cwd has multiple git-repo children                                                        | `ambiguous` error | — write tools fail fast            |
| 6    | no git repo near cwd                                                                      | `dir_basename`    | basename of cwd                    |

Child scan constraints: depth=1, max 20 entries, 200ms timeout, skips hidden dirs and noise dirs (`node_modules`, `vendor`, `.venv`, `__pycache__`, `target`, `dist`, `build`, `.idea`, `.vscode`).

### Response envelope

Most successful MCP tool responses use this envelope:

```json
{
  "project": "engram",
  "project_source": "git_remote",
  "project_path": "/home/user/engram",
  "result": "...(tool output)..."
}
```

Error responses include `available_projects` when the error is `ambiguous_project` or `unknown_project`.

Exceptions:

- `mem_current_project` returns detection fields directly (`project`, `project_source`, `project_path`, `cwd`, `available_projects`, optional `warning` / `error_hint`) and does not wrap them in `result`.
- `mem_doctor` returns the same JSON report shape as `engram doctor --json`; it uses read-project resolution before running diagnostics but does not wrap the report in the common MCP envelope.

### Write tools (explicit/session/cwd project resolution)

`mem_session_start` resolves from its explicit `directory` argument when supplied; otherwise it auto-detects from cwd. `mem_session_end` and `mem_capture_passive` auto-detect project from cwd; any `project` argument the LLM sends to them is ignored. `mem_session_summary` supports explicit project override (`project`, `project_choice_reason`, `recovery_token`) matching `mem_save`'s project resolution.

`mem_update` uses ID-based updates and auto-detects project only for response envelope metadata. Its public schema does not expose `project`; raw legacy clients may still send a non-empty `project` argument, and the handler tolerates it as an observation project update for compatibility.

`mem_save` resolves writes by precedence: validated explicit `project`, project already associated with `session_id`, repo/cwd detection (nearest `.engram/config.json` within the enclosing git root, git remote/root/child), then directory-basename fallback.

Guardrails:

- Invalid explicit `project` names fail loudly instead of silently falling back.
- Valid-looking explicit `project` names are accepted only when backed by known context: an existing local project in the store, a matching existing session project, the nearest resolvable `.engram/config.json`, or exact ambiguous-project recovery after the user selected one available project.
- An unbacked explicit `project` fails loudly and does not create a new bucket.
- If a non-empty `session_id` is supplied and no session exists, `mem_save` fails with a structured error and does not write.
- If both explicit `project` and `session_id` are supplied, they must resolve to the same normalized project or `mem_save` fails with a structured error and does not write.
- `project_choice_reason=user_selected_after_ambiguous_project` is only honored when cwd resolution is actually ambiguous. On a non-ambiguous cwd, stale recovery flags do not override explicit-project precedence or session mismatch validation.
- If ambiguous-project recovery is active, `project` must exactly match one of the previously returned `available_projects`; invented or normalized guesses are rejected.
- Exact ambiguous-project choices can still fail with `project_name_collision` when multiple available names collapse to the same stored project bucket after normalization. Rename or disambiguate the colliding projects before retrying.
- Ordinary explicit `mem_save(project=...)` calls can also fail with `project_name_collision` when the raw explicit name collapses into an existing config-backed, session-backed, or store-backed project bucket, such as `foo--bar` colliding with `foo-bar`.

For monorepos, detection now honors the **nearest** `.engram/config.json` at or below the enclosing git root. That lets `repo/backend/.engram/config.json` and `repo/frontend/.engram/config.json` behave as independent projects without letting `~/.engram/config.json` leak into nested workspaces.

`mem_save_prompt` keeps the older cwd/default behavior by default and only uses `project` for the narrow ambiguous-project recovery override: after a previous `ambiguous_project` error, the agent may retry with `project=<one of available_projects>` and `project_choice_reason=user_selected_after_ambiguous_project`.

### Read tools (optional project override)

`mem_search`, `mem_context`, `mem_timeline`, `mem_stats`, `mem_doctor` — `project` is an optional argument. If supplied, it is validated against the store via `ProjectExists`. Unknown project names return a structured error with `available_projects`. `mem_get_observation` resolves project from cwd for envelope metadata and does not accept a project override.

### Admin tools

`mem_delete` is ID-based and requires `id`; optional `hard_delete=true` permanently deletes the observation. It does not accept or auto-detect `project`.

`mem_merge_projects` requires `from` (comma-separated source project names) and `to` (canonical target project name). It does not accept or auto-detect `project`.

### mem_current_project

Use `mem_current_project` as the first call in a session to inspect the detection result:

```json
{
  "project": "engram",
  "project_source": "git_remote",
  "project_path": "/home/user/engram",
  "cwd": "/home/user/engram",
  "available_projects": [],
  "warning": ""
}
```

Returns success even when cwd is ambiguous — empty `project` + non-empty `available_projects` signals the agent to navigate to a specific repo before writing.

---

## MCP Tools (20 tools)

### mem_search

Search persistent memory across all sessions. Supports FTS5 full-text search with type/project/scope/limit filters.

Results are returned as a **reference-first index**, not as observation bodies. One line per hit:

```
1. [decision] auth model — Auth uses JWT with 15m expiry. (id: obs-1f2e…, ~299 tok, score -0.00)
```

The summary is the hit's first sentence (capped at 160 runes, plus an ellipsis when truncated), `id` is the `sync_id` to pass to `mem_get_observation` for the full body, and `~N tok` is the approximate cost of that expansion (`ceil(bytes/4)`). The budget bounds the whole agent-visible payload: the optional `budget` argument caps the index's approximate token size (default 600), and hits that do not fit are dropped from the text with an explicit `(+K more omitted — raise budget or refine query)` line — never silently. Omitted hits are dropped from the structured `results` array too, so they cost nothing; `results` lists exactly the shown hits (ids/titles/metadata only, no bodies). Raise `budget` or refine the query to see the rest.

Set `all_projects: true` to search across every project instead of the resolved one. This bypasses project detection entirely and ignores the `project` argument, so an agent can recall a decision logged elsewhere without knowing the project key. The response envelope reports `project_source: "all_projects"` and an empty `project` to reflect the cross-project scope.

Scope values accepted by the `scope` parameter: `project` (default), `personal`, `global`. When `scope: personal` is passed without an explicit `project` override, the project filter is cleared and personal observations are searched across all projects (cross-project personal scope).

Each structured search result includes lifecycle metadata: `state` (`active` or `needs_review`) and, when set, `review_after`. Lifecycle state, created-at, project, and scope are carried by the structured `results` entries; the text index line itself stays one line per hit.

When an observation has judged relations in `memory_relations`, the result entry includes annotation lines immediately after that hit's index line:

```
supersedes: #<id> (<title>)       — this memory supersedes another
superseded_by: #<id> (<title>)    — another memory supersedes this one
conflicts: #<id> (<title>)        — judged conflict with another memory
conflict: contested by #<id> (pending)  — pending (not yet judged)
```

Multiple annotation lines appear when multiple relations apply — one per related observation. Titles are retrieved via JOIN (no N+1 queries). When the related observation has been deleted, `(deleted)` replaces the title. Agent parsers should match by prefix — these prefixes are stable across versions (REQ-012).

Pending relations (from `mem_save` conflict surfacing, before `mem_judge` is called) produce the `conflict: contested by #<id> (pending)` form. Judged relations produce the enriched form with title.

### mem_save

Save structured observations. The tool description teaches agents the format:

- **title**: Short, searchable (e.g. "JWT auth middleware")
- **type**: `decision` | `architecture` | `bugfix` | `pattern` | `config` | `discovery` | `learning`
- **scope**: `project` (default) | `personal` | `global` — see [Team Usage](docs/TEAM-USAGE.md) for conventions and sync caveats
- **topic_key**: optional canonical topic id (e.g. `architecture/auth-model`) used to upsert evolving memories
- **capture_prompt**: optional boolean, default `true`; when current prompt context is available in the same MCP process for the same project/session, Engram best-effort records it alongside the observation. If that process-local context is unavailable or prompt capture fails, `mem_save` still succeeds. Automated pipeline saves such as SDD artifacts should pass `false`.
- **content**: Structured with `**What**`, `**Why**`, `**Where**`, `**Learned**`; required unless the legacy `observation` alias is provided
- **observation**: backward-compatible alias for `content` for older/raw MCP clients; prefer `content` for new integrations

Exact duplicate saves are deduplicated in a rolling time window using a normalized content hash + project + scope + type + title.
When `topic_key` is provided, `mem_save` upserts the latest observation in the same `project + scope + topic_key`, incrementing `revision_count`.
Save responses include lifecycle metadata for the saved observation: computed `state` (`active` or `needs_review`) and `review_after` when the observation type has a review cycle.

### mem_update

Update an observation by ID. Public schema supports partial updates for `title`, `content`, `type`, `scope`, and `topic_key`. For legacy/raw MCP clients, a non-empty `project` argument is still tolerated by the handler even though it is not exposed in the schema.

### mem_review

Review observation lifecycle state. Available in the `agent` profile (`engram mcp --tools=agent`).

Actions:

- `action: "list"` — returns observations whose `review_after` has passed. Optional parameters: `project` and `limit` (default 10).
- `action: "mark_reviewed"` — requires `observation_id`; resets that observation's local review cycle using its type decay policy. The legacy `id` alias is accepted for compatibility.

`mark_reviewed` is local-only for now: `review_after` is intentionally not part of sync payloads in this phase, so resetting the review cycle does not enqueue a sync mutation or propagate to other machines.

### mem_suggest_topic_key

Suggest a stable `topic_key` from `type + title` (or content fallback). Uses family heuristics like `architecture/*`, `bug/*`, `decision/*`, etc. Use before `mem_save` when you want evolving topics to upsert into a single observation.

### mem_delete

Delete an observation by ID. Uses soft-delete by default (`deleted_at`); optional hard-delete for permanent removal.

### mem_save_prompt

Save user prompts — records what the user asked so future sessions have context about user goals.
When called in the same MCP process, this also feeds process-local current prompt context used by later `mem_save` calls with `capture_prompt=true`. The same MCP process lifecycle must receive the prompt context before the later save; prompt capture is best-effort and `mem_save` still succeeds when no context is available.

Prompts are **write-only over MCP**: `mem_save_prompt` is the only prompt tool the MCP server registers, so stdio agents (Claude Code, Codex, Gemini, Cursor, Windsurf) can save prompts but cannot read them back. Reading is via `engram serve` HTTP (`GET /prompts/recent`, `GET /prompts/search`) — the OpenCode plugin and Pi extension already run it — or the cloud dashboard's prompt views. This is the intended Wave-1 posture, not a gap.

### mem_context

Get recent memory context from previous sessions, with optional scope filtering for observations.

The block is layered and budgeted (~400 tokens / 1600 bytes hard cap) so it is cheap to call at every session start and after every compaction:

```
## Memory Context

### Pinned
- [decision] **title**: <content, cut to 300 runes>

### Recent Sessions
- <started_at> <summary first sentence | (active) | (no summary)>   (max 5)

### Recent Observations
- [type] **title**: <content first sentence, cut to 160 runes>      (max 3)

Full bodies: mem_search / mem_get_observation.
```

Overflow past the cap drops the oldest observation lines first, then the oldest session lines; pinned content is never dropped. Sections with no rows are omitted entirely, and an empty project returns an empty string. In a cross-project render (blank project filter — e.g. `scope: personal` with no `project` override) each session line is prefixed with its project: `- <started_at> [<project>] <summary>`. The MemoryLake backend renders the identical layout minus `### Recent Sessions` (it has no session tracking).

Recent user prompts are **not** part of this block (they cost more than they buy inside the budget). The intended Wave-1 posture is that **prompts are write-only for stdio agents**: Claude Code, Codex, Gemini, Cursor, and Windsurf reach Engram over stdio MCP, which exposes `mem_save_prompt` but no prompt-read tool. Saved prompts are readable only through the `engram serve` HTTP API (`GET /prompts/recent`, `GET /prompts/search`) — used by the OpenCode plugin and the Pi extension, which already run `engram serve` — and through the cloud dashboard. Prompts still feed `mem_save --capture_prompt` within the same MCP process, so stdio agents get their value indirectly rather than by reading them back.

Scope values accepted by the `scope` parameter: `project` (default), `personal`, `global`. When `scope: personal` is passed without an explicit `project` override, the project filter is cleared and personal observations are returned across all projects (cross-project personal scope).

### mem_stats

Show memory system statistics — sessions, observations, prompts, projects.

### mem_timeline

Progressive disclosure: after searching, drill into chronological context around a specific observation. Shows N observations before and after within the same session.

### mem_get_observation

Get full untruncated content of a specific observation by ID.

### mem_session_summary

Save comprehensive end-of-session summary:

```
## Goal
## Instructions
## Discoveries
## Accomplished
## Next Steps
## Relevant Files
```

### mem_session_start

Register the start of a new coding session.

### mem_session_end

Mark a session as completed with optional summary.

### mem_capture_passive

Extract structured learnings from text output. Looks for `## Key Learnings:` sections and saves each numbered/bulleted item as a separate observation. Duplicates are automatically skipped.

### mem_merge_projects

**Admin tool.** Merge multiple project name variants into a single canonical name. Requires `from` as a comma-separated list of source project names and `to` as the target canonical name. All observations, sessions, and prompts from the source projects are reassigned to the canonical project.

### mem_current_project

Detect the current project from the working directory. Returns `project`, `project_source`, `project_path`, `cwd`, `available_projects`, and `warning`. Never returns an error — even on ambiguous cwd it returns success with an empty `project` and non-empty `available_projects`. Recommended as the first call when starting a session.

### mem_doctor

Run read-only operational diagnostics. Returns the same JSON report shape as `engram doctor --json`, with optional `project` and `check` filters. The optional `project` override is validated with read-project resolution before diagnostics run.

### mem_judge

Record a verdict on a pending memory conflict. When `mem_save` returns `candidates[]` and `judgment_required: true`, the agent inspects the candidates and calls `mem_judge` to mark the relation between the saved memory and a candidate.

Parameters:

- **judgment_id** (required): the `judgment_id` returned by `mem_save`
- **relation** (required): `related` | `compatible` | `scoped` | `conflicts_with` | `supersedes` | `not_conflict`
- **reason** (optional): short text explaining the verdict
- **evidence** (optional): free-form text or JSON the agent can use to justify the call (e.g., quoted excerpts from both memories)
- **confidence** (optional, default 1.0): 0.0–1.0; if the value is below 0.7 the agent SHOULD ask the user before calling

Re-judging an existing relation overwrites it (deliberate revision). Two agents judging the same pair persist as separate rows — Phase 1 surfaces both; cross-actor reconciliation is Phase 2.

Search results subsequently expose annotation lines like `supersedes: #<id> (<title>)`, `superseded_by: #<id> (<title>)`, and `conflicts: #<id> (<title>)` so the recalling agent sees relevant verdicts at-a-glance. For enrolled projects with autosync enabled, judgments propagate to other machines via the cloud mutation pipeline — the annotation appears in `mem_search` results on any machine that has pulled the relevant mutations.

### mem_compare

Records a verdict on a semantic comparison between two memories. The agent reads both memories, judges the relationship using its LLM reasoning, and calls `mem_compare` to persist the verdict. Unlike `mem_judge` (which resolves a pre-existing `pending` candidate surfaced by `mem_save`), `mem_compare` creates a new relation row directly — useful for proactive semantic analysis that goes beyond FTS5 lexical matching.

Available in the `agent` profile (`engram mcp --tools=agent`).

Parameters:

- **memory_id_a** (required): string — sync_id of the first memory
- **memory_id_b** (required): string — sync_id of the second memory
- **relation** (required): string — one of `conflicts_with` | `supersedes` | `scoped` | `related` | `compatible` | `not_conflict`
- **confidence** (required): float 0.0..1.0
- **reasoning** (required): string — explanation of the verdict (max 200 chars)
- **model** (optional): string — model name for provenance (e.g. `"claude-haiku-4-5"`)

Behavior:

- Persists a relation row via `JudgeBySemantic` with system provenance (`marked_by_kind="system"`, `marked_by_actor="engram"`)
- Idempotent: the same `(source_id, target_id)` pair updates the existing row rather than inserting a duplicate
- `not_conflict` verdicts are no-ops — acknowledged but not persisted, matching the scan flow contract
- Cross-project relations are rejected with an error

---

## Memory Protocol

The Memory Protocol teaches agents **when** and **how** to use Engram's MCP tools. Without it, the agent has the tools but no behavioral guidance. Add this to your agent's prompt file (see [Agent Setup](docs/AGENT-SETUP.md) for per-agent locations).

### SAVING — silent, batched, never instead of answering

Save decisions, bug root causes, conventions, gotchas, and user preferences:

- Bug fix completed
- Architecture or design decision made
- Non-obvious discovery about the codebase
- Configuration change or environment setup
- Pattern established (naming, structure, convention)
- User preference or constraint learned

Saving never replaces answering. The agent's final reply must contain the complete
answer itself — memory serves future sessions, never this reply. Agents must never
narrate saves ("I've saved this to memory"), and should batch saves at task end
rather than interrupting the work to save each item as it happens. Searching is
once, up front: one `mem_search` at task start; on a miss, proceed normally.

Format for `mem_save`:

- **title**: Verb + what — short, searchable (e.g. "Fixed N+1 query in UserList", "Chose Zustand over Redux")
- **type**: `bugfix` | `decision` | `architecture` | `discovery` | `pattern` | `config` | `preference`
- **scope**: `project` (default) | `personal` | `global`
- **topic_key** (optional, recommended for evolving decisions): stable key like `architecture/auth-model`
- **content**:
  ```
  **What**: One sentence — what was done
  **Why**: What motivated it (user request, bug, performance, etc.)
  **Where**: Files or paths affected
  **Learned**: Gotchas, edge cases, things that surprised you (omit if none)
  ```

### Topic update rules (mandatory)

- Different topics must not overwrite each other (e.g. architecture vs bugfix)
- Reuse the same `topic_key` to update an evolving topic instead of creating new observations
- If unsure about the key, call `mem_suggest_topic_key` first and then reuse it
- Use `mem_update` when you have an exact observation ID to correct

### SEARCHING — once, up front

When the user asks to recall something — any variation of "remember", "recall", "what did we do", "how did we solve", "recordar", "acordate", or references to past work:

1. First call `mem_context` — checks recent session history (fast, cheap)
2. If not found, call `mem_search` with relevant keywords (FTS5 full-text search)
3. If you find a match, use `mem_get_observation` for full untruncated content

Search once at task start — on a miss, proceed normally rather than re-running the same query. Also search at task start when:

- Starting work on something that might have been done before
- The user mentions a topic you have no context on — check if past sessions covered it

### SESSION CLOSE PROTOCOL (mandatory)

Before ending a session or saying "done" / "listo" / "that's it", you MUST call `mem_session_summary` with this structure:

```
## Goal
[What we were working on this session]

## Instructions
[User preferences or constraints discovered — skip if none]

## Discoveries
- [Technical findings, gotchas, non-obvious learnings]

## Accomplished
- [Completed items with key details]

## Next Steps
- [What remains to be done — for the next session]

## Relevant Files
- path/to/file — [what it does or what changed]
```

This is NOT optional. If you skip this, the next session starts blind.

### PASSIVE CAPTURE

When completing a task, include a `## Key Learnings:` section at the end of your response with numbered items. Engram will automatically extract and save these as observations.

Example:

```
## Key Learnings:

1. bcrypt cost=12 is the right balance for our server performance
2. JWT refresh tokens need atomic rotation to prevent race conditions
```

You can also call `mem_capture_passive(content)` directly with any text that contains a learning section.

### AFTER COMPACTION

If you see a message about compaction or context reset:

1. IMMEDIATELY call `mem_session_summary` with the compacted summary content
2. Then call `mem_context` to recover additional context from previous sessions
3. Only THEN continue working

Do not skip step 1. Without it, everything done before compaction is lost from memory.

---

## Project Name Normalization

Engram automatically prevents project name drift — the same project saved under different names (`"engram"` vs `"Engram"` vs `"engram-memory"`) by different clients or users.

### Automatic normalization

All project names are normalized on write and read: **lowercase**, **trimmed**, **collapsed hyphens/underscores**. If a name is changed during normalization, a warning is included in the response.

### Auto-detection

MCP tools resolve project names at call time using the shared detection chain:

1. Nearest `.engram/config.json` `project_name` within the enclosing git root, or at cwd outside git
2. Git remote origin URL (extracts repo name)
3. Git repository root directory name
4. Single git-repo child of cwd
5. Multiple git-repo children of cwd returns `ambiguous_project` with `available_projects`
6. Current working directory basename

`engram mcp` accepts a process-level default project via `--project <name>` / `--project=<name>` or `ENGRAM_PROJECT=<name>`. This override takes precedence over cwd detection for all read and write tools throughout the lifetime of that MCP process. It is a trusted startup-time value — use it when the host cannot supply a reliable cwd (VS Code, WSL, CI, Docker).

### Similar-project warnings

When saving to a project that doesn't exist yet, Engram checks for similar existing project names (Levenshtein distance, substring, case-insensitive matching) and warns the agent if a likely variant already exists.

### Retroactive cleanup

Use `engram projects consolidate` to interactively merge variant project names, or `mem_merge_projects` for agent-driven consolidation.

---

## Features

### Full-Text Search (FTS5)

- Searches across title, content, tool_name, type, and project
- Query sanitization: wraps each word in quotes to avoid FTS5 syntax errors
- Supports type and project filters

### Timeline (Progressive Disclosure)

Three-layer pattern for token-efficient memory retrieval:

1. `mem_search` — Find relevant observations
2. `mem_timeline` — Drill into chronological neighborhood of a result
3. `mem_get_observation` — Get full untruncated content

### Privacy Tags

`<private>...</private>` content is stripped at TWO levels:

1. **Plugin layer** (TypeScript) — Strips before data leaves the process
2. **Store layer** (Go) — `stripPrivateTags()` runs inside `AddObservation()` and `AddPrompt()`

Example: `Set up API with <private>sk-abc123</private>` becomes `Set up API with [REDACTED]`

### User Prompt Storage

Separate table captures what the USER asked (not just tool calls). Gives future sessions the "why" behind the "what". Full FTS5 search support.

### Export / Import

Share memories across machines, backup, or migrate:

- `engram export` — JSON dump of all sessions, observations, prompts
- `engram import <file>` — Load from JSON, sessions use INSERT OR IGNORE (skip duplicates), atomic transaction

### Git Sync (Chunked)

Share memories through git repositories using compressed chunks with a manifest index.

- `engram sync` — Exports new memories as a gzipped JSONL chunk to `.engram/chunks/`
- `engram sync --all` — Exports ALL memories from every project
- `engram sync --import` — Imports chunks listed in the manifest that haven't been imported yet
- `engram sync --status` — Shows how many chunks exist locally vs remotely (filesystem mode)
- `engram sync --cloud --status --project <name>` — Shows local, remote, and pending chunk counts for the specified cloud project
- `engram sync --project NAME` — Filters export to a specific project

```
.engram/
├── manifest.json          <- index of all chunks (small, git-mergeable)
├── chunks/
│   ├── a3f8c1d2.jsonl.gz <- chunk 1 (gzipped JSONL)
│   ├── b7d2e4f1.jsonl.gz <- chunk 2
│   └── ...
└── engram.db              <- local working DB (gitignored)
```

**Why chunks?**

- Each `engram sync` creates a NEW chunk — old chunks are never modified
- No merge conflicts: each dev creates independent chunks, git just adds files
- Chunks are content-hashed (SHA-256 prefix) — each chunk is imported only once
- The manifest is the only file git diffs — it's small and append-only
- Compressed: a chunk with 8 sessions + 10 observations = ~2KB

### Agent-Driven Compression

Instead of a separate LLM service, the agent itself compresses observations. The agent already has the model, context, and API key.

**Two levels:**

- **Per-action** (`mem_save`): Structured summaries (What/Why/Where/Learned)
- **Session summary** (`mem_session_summary`): Comprehensive end-of-session summary (Goal/Instructions/Discoveries/Accomplished/Next Steps/Files)

### No Raw Tool-Call Auto-Capture

Engram does not record a firehose of raw tool calls. Raw tool calls (`edit: {file: "foo.go"}`, `bash: {command: "go build"}`) are noisy and pollute FTS5 search. The agent's curated summaries are higher signal, more searchable, and don't bloat the database. Shell history and git provide the raw audit trail.

Since v1.15.3, `mem_save` can also best-effort attach the current user prompt when prompt context was already provided to the same MCP process for the same project/session (typically by `mem_save_prompt`) and `capture_prompt` is not disabled. That is not raw event capture: it stores user intent tied to a curated save, and the save still succeeds if prompt context is missing.

---

## Terminal UI (TUI)

Interactive Bubbletea-based terminal UI. Launch with `engram tui`.

### Screens

| Screen                  | Description                                                       |
| ----------------------- | ----------------------------------------------------------------- |
| **Dashboard**           | Stats overview (sessions, observations, prompts, projects) + menu |
| **Search**              | FTS5 text search with text input                                  |
| **Search Results**      | Browsable results list from search                                |
| **Recent Observations** | Browse all observations, newest first                             |
| **Observation Detail**  | Full content of a single observation, scrollable                  |
| **Timeline**            | Chronological context around an observation (before/after)        |
| **Sessions**            | Browse all sessions                                               |
| **Session Detail**      | Observations within a specific session                            |

### Navigation

- `j/k` or arrow keys — Navigate lists
- `Enter` — Select / drill into detail
- `c` — Copy observation content to clipboard (OSC 52; works in search results, recent list, detail, and session views)
- `t` — View timeline for selected observation
- `s` or `/` — Quick search from any screen
- `Esc` or `q` — Go back / quit
- `Ctrl+C` — Force quit

### Visual Features

- **Catppuccin Mocha** color palette
- **`(active)` badge** — shown next to sessions and observations from active sessions, sorted to top
- **Scroll indicators** — position in long lists (e.g. "showing 1-20 of 50")
- **2-line items** — each observation shows title + content preview

---

## Running as a Service

Without a service supervisor, `engram serve` dies whenever the binary is replaced (e.g. on `brew upgrade engram`) or the host reboots, and autosync stops silently. The templates below restart it automatically. Use `engram cloud status` afterwards to confirm — the `Local daemon:` line should report `running on port 7437`.

### Using systemd (Linux)

1. Move binary to `~/.local/bin` (ensure it's in your `$PATH`)
2. Create directories: `mkdir -p ~/.engram ~/.config/systemd/user`
3. Create `~/.config/systemd/user/engram.service` (see below)
4. `systemctl --user daemon-reload`
5. `systemctl --user enable engram`
6. `systemctl --user start engram`
7. `journalctl --user -u engram -f`

```ini
[Unit]
Description=Engram Memory Server
After=network.target

[Service]
WorkingDirectory=%h
ExecStart=%h/.local/bin/engram serve
Restart=always
RestartSec=3
Environment=ENGRAM_DATA_DIR=%h/.engram

[Install]
WantedBy=default.target
```

### Using launchd (macOS)

This is the recommended setup for Homebrew users on macOS. With `KeepAlive=true`, launchd relaunches `engram serve` automatically after `brew upgrade engram` replaces the binary, so autosync survives upgrades.

1. Find your binary path: `which engram` (typically `/opt/homebrew/bin/engram` on Apple Silicon or `/usr/local/bin/engram` on Intel)
2. Create the data dir if missing: `mkdir -p ~/.engram`
3. Create `~/Library/LaunchAgents/com.gentleman-programming.engram.plist` with the contents below — replace `<HOME>` with the absolute path of your home directory (`echo $HOME`) and adjust the binary path if `which engram` returned something different
4. Load it: `launchctl load ~/Library/LaunchAgents/com.gentleman-programming.engram.plist`
5. Verify: `launchctl list | grep engram` and `engram cloud status` (the `Local daemon:` line should report `running on port 7437`)

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.gentleman-programming.engram</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/engram</string>
        <string>serve</string>
    </array>
    <key>WorkingDirectory</key>
    <string><HOME></string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>ENGRAM_DATA_DIR</key>
        <string><HOME>/.engram</string>
        <!-- Uncomment and fill these to enable cloud autosync:
        <key>ENGRAM_CLOUD_AUTOSYNC</key>
        <string>1</string>
        <key>ENGRAM_CLOUD_SERVER</key>
        <string>https://your-cloud-host</string>
        <key>ENGRAM_CLOUD_TOKEN</key>
        <string>your-cloud-token</string>
        -->
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string><HOME>/.engram/serve.out.log</string>
    <key>StandardErrorPath</key>
    <string><HOME>/.engram/serve.err.log</string>
</dict>
</plist>
```

To unload (stop and disable): `launchctl unload ~/Library/LaunchAgents/com.gentleman-programming.engram.plist`. To reload after editing the plist: unload, then load again.

> **Note on `brew upgrade`:** launchd does not expand `$HOME` or `~` inside plist values, which is why the template uses literal absolute paths.

### Using Windows Task Scheduler

Windows Task Scheduler is the native service equivalent on Windows. It restarts `engram serve` on login and after reboots, keeping autosync alive without a third-party service manager.

**Setup steps:**

1. Confirm `engram.exe` is in your `PATH`: open PowerShell and run `Get-Command engram`.
2. Set `ENGRAM_CLOUD_TOKEN` (and any other cloud vars) as a **user or system environment variable** in System Properties → Advanced → Environment Variables. Task Scheduler does not inherit session environment variables, so tokens set in your shell profile or in `$env:...` within a PowerShell session will not be visible to the scheduled task.
3. Create the scheduled task by running the PowerShell snippet below in an elevated terminal (Run as Administrator), or import it manually through the Task Scheduler GUI.
4. Verify: after the next login (or trigger manually), run `engram cloud status` — the `Local daemon:` line should report `running on port 7437`.

```powershell
$action  = New-ScheduledTaskAction `
    -Execute  "powershell.exe" `
    -Argument "-ExecutionPolicy Bypass -WindowStyle Hidden -Command `"Start-Process engram -ArgumentList 'serve' -NoNewWindow`""

$trigger = New-ScheduledTaskTrigger -AtLogOn

$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit (New-TimeSpan -Hours 0) `
    -RestartCount 5 `
    -RestartInterval (New-TimeSpan -Minutes 1) `
    -StartWhenAvailable

Register-ScheduledTask `
    -TaskName    "EngramMemoryServer" `
    -Action      $action `
    -Trigger     $trigger `
    -Settings    $settings `
    -RunLevel    Limited `
    -Description "Engram persistent memory server (engram serve)"
```

> **Environment variables:** `ENGRAM_CLOUD_TOKEN`, `ENGRAM_CLOUD_SERVER`, `ENGRAM_CLOUD_AUTOSYNC`, and `ENGRAM_DATA_DIR` must be set as persistent user or system environment variables (Control Panel → System → Advanced → Environment Variables) so Task Scheduler can read them. Variables you `export` or set with `$env:` in a terminal session are not visible to scheduled tasks.

> **Logs:** To capture stdout/stderr, redirect output in the PowerShell command string, for example: `... -Command "Start-Process engram -ArgumentList 'serve' -NoNewWindow -RedirectStandardOutput '$env:USERPROFILE\.engram\serve.out.log' -RedirectStandardError '$env:USERPROFILE\.engram\serve.err.log'"`. Ensure the log files are opened with UTF-8 encoding (`-Encoding UTF8`) if you post-process them.

> **Stopping the task:** `Stop-ScheduledTask -TaskName "EngramMemoryServer"` or `Unregister-ScheduledTask -TaskName "EngramMemoryServer" -Confirm:$false` to remove it entirely.

---

## Design Decisions

1. **Go over TypeScript** — Single binary, cross-platform, no runtime. The initial prototype was TS but was rewritten.
2. **SQLite + FTS5 over vector DB** — FTS5 covers 95% of use cases. No ChromaDB/Pinecone complexity.
3. **Agent-agnostic core** — Go binary is the brain, thin plugins per-agent. Not locked to any agent.
4. **Agent-driven compression** — The agent already has an LLM. No separate compression service.
5. **Privacy at two layers** — Strip in plugin AND store. Defense in depth.
6. **Pure Go SQLite (modernc.org/sqlite)** — No CGO means true cross-platform binary distribution.
7. **No raw tool-call auto-capture** — The agent saves curated summaries; `mem_save` may best-effort capture process-local prompt context tied to that save, but Engram does not ingest raw tool-call firehoses. Shell history and git provide the raw audit trail.
8. **TUI with Bubbletea** — Interactive terminal UI following Gentleman Bubbletea patterns.

---

## Dependencies

### Go

| Package                              | Version | Purpose                        |
| ------------------------------------ | ------- | ------------------------------ |
| `github.com/mark3labs/mcp-go`        | v0.44.0 | MCP protocol implementation    |
| `modernc.org/sqlite`                 | v1.45.0 | Pure Go SQLite driver (no CGO) |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | Terminal UI framework          |
| `github.com/charmbracelet/lipgloss`  | v1.1.0  | Terminal styling               |
| `github.com/charmbracelet/bubbles`   | v1.0.0  | TUI components                 |

### OpenCode Plugin

- `@opencode-ai/plugin` — OpenCode plugin types and helpers
- Runtime: Bun (built into OpenCode)

---

## Dashboard templ regeneration

The cloud dashboard uses [templ](https://templ.guide/) for server-side HTML components. Generated `*_templ.go` files are committed alongside their `.templ` sources. If you modify any `.templ` file in `internal/cloud/dashboard/`, you must regenerate the Go output before committing.

### Prerequisite

Download the pinned templ binary:

```sh
go mod download
```

### Regenerate

```sh
make templ
# or directly:
go tool templ generate ./internal/cloud/dashboard/...
```

The regenerated `components_templ.go`, `layout_templ.go`, and `login_templ.go` must be committed together with the `.templ` source changes. The test `TestTemplGeneratedFilesAreCheckedIn` in `internal/cloud/dashboard/templ_policy_test.go` will fail in CI if generated files are missing or outdated.

**Important**: Always use the pinned version `github.com/a-h/templ v0.3.1001` (already in `go.mod`). Regenerating with a different version produces diff churn in generated output.

---

## Cloud Autosync

Autosync is a background push/pull replication service that keeps your local Engram store in sync with the Engram Cloud server without blocking local writes.

### Enabling Autosync

Autosync is **opt-in**. Set all three environment variables before starting `engram serve` or `engram mcp`:

| Variable                | Required          | Description                                                             |
| ----------------------- | ----------------- | ----------------------------------------------------------------------- |
| `ENGRAM_CLOUD_AUTOSYNC` | Yes (exact `"1"`) | Enables autosync. Any other value disables it.                          |
| `ENGRAM_CLOUD_TOKEN`    | Yes               | Bearer token for the cloud server.                                      |
| `ENGRAM_CLOUD_SERVER`   | Yes               | Base URL of the cloud server (e.g. `https://cloud.engram.example.com`). |

Example:

```sh
ENGRAM_CLOUD_AUTOSYNC=1 \
ENGRAM_CLOUD_TOKEN=your-token \
ENGRAM_CLOUD_SERVER=https://cloud.engram.example.com \
engram serve

# Or, for stdio MCP agents:
ENGRAM_CLOUD_AUTOSYNC=1 \
ENGRAM_CLOUD_TOKEN=your-token \
ENGRAM_CLOUD_SERVER=https://cloud.engram.example.com \
engram mcp
```

Missing `ENGRAM_CLOUD_TOKEN` or `ENGRAM_CLOUD_SERVER` logs an `ERROR` and disables autosync gracefully — `engram serve` or `engram mcp` still starts.

### Autosync Phase Table

| Phase         | Meaning                                | Dashboard Status          |
| ------------- | -------------------------------------- | ------------------------- |
| `idle`        | Loop running, no cycle yet             | running                   |
| `pushing`     | Pushing local mutations to cloud       | running                   |
| `pulling`     | Pulling remote mutations               | running                   |
| `healthy`     | Last cycle succeeded                   | healthy                   |
| `push_failed` | Last push failed                       | degraded                  |
| `pull_failed` | Last pull failed                       | degraded                  |
| `backoff`     | Too many consecutive failures; waiting | degraded                  |
| `disabled`    | Paused by `StopForUpgrade`             | degraded (upgrade_paused) |

### Reason Code Table

`reason_code` appears in `Manager.Status().ReasonCode` and is surfaced via `/sync/status`:

| `reason_code`      | Cause                                                   | Resolution                                                                   |
| ------------------ | ------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `transport_failed` | Network error, server 5xx, or 404 on mutation endpoints | Check server health and network; if 404, see `server_unsupported` note below |
| `auth_required`    | Bearer token rejected (401)                             | Rotate `ENGRAM_CLOUD_TOKEN`                                                  |
| `policy_forbidden` | Project access denied (403)                             | Check `ENGRAM_CLOUD_ALLOWED_PROJECTS` on the server                          |
| `internal_error`   | Panic inside the sync cycle                             | Check logs for stack trace                                                   |
| `upgrade_paused`   | Autosync paused during cloud upgrade (`PhaseDisabled`)  | Call `ResumeAfterUpgrade` or restart                                         |

Note: when the cloud server returns 404 on mutation endpoints, the transport logs `[autosync] cloud mutation endpoint returned 404 (server_unsupported)` and the transport-level `ErrorCode` is `"server_unsupported"`, but the manager surfaces this as `reason_code: transport_failed`.

### Troubleshooting

For a step-by-step recovery guide covering `chunk_id does not match payload content hash`, `session payload directory is required`, and the temporary missing-directory repair helper, see [Engram Cloud Troubleshooting](docs/engram-cloud/troubleshooting.md).

**`transport_failed` with `server_unsupported` in logs**: Older pre-mutation cloud server deployments may not implement `POST /sync/mutations/push` or `GET /sync/mutations/pull`, causing 404 responses from those endpoints. Deploy a server version that includes these routes before enabling `ENGRAM_CLOUD_AUTOSYNC=1`. Check logs for the line containing `server_unsupported`.

**Autosync not starting**: Check that `ENGRAM_CLOUD_AUTOSYNC` is exactly `"1"` (not `"true"` or `"yes"`), and that both `ENGRAM_CLOUD_TOKEN` and `ENGRAM_CLOUD_SERVER` are non-empty. The process logs an `[autosync] ERROR` line explaining which variable is missing.

**Local writes still blocked**: Autosync runs in its own goroutine and never holds locks shared with the local write path. If local writes appear blocked, investigate the SQLite store layer, not the autosync manager.

---

---

## Cloud Sync Audit Log

When project sync is paused and a push is rejected, Engram records an audit entry in `cloud_sync_audit_log`. This gives operators a persistent trail of every rejection event, visible in the admin dashboard under **Admin > Audit Log**.

### Schema

| Column        | Type                      | Description                                                                 |
| ------------- | ------------------------- | --------------------------------------------------------------------------- |
| `id`          | SERIAL PK                 | Auto-incrementing row identifier                                            |
| `occurred_at` | TIMESTAMPTZ DEFAULT NOW() | Timestamp of the rejection event                                            |
| `contributor` | TEXT NOT NULL             | Identity of the caller (from `created_by` field in request, or `"unknown"`) |
| `project`     | TEXT NOT NULL             | Project name that was paused and rejected                                   |
| `action`      | TEXT NOT NULL             | Push type discriminator: `mutation_push` or `chunk_push`                    |
| `outcome`     | TEXT NOT NULL             | Rejection outcome: always `rejected_project_paused` in v1                   |
| `entry_count` | INT DEFAULT 0             | Number of entries in the rejected batch                                     |
| `reason_code` | TEXT                      | Short machine-readable reason code (e.g. `sync-paused`)                     |
| `metadata`    | JSONB                     | Reserved for future structured context; not populated in v1                 |

### Outcome Vocabulary

| Outcome                   | Meaning                                                                           |
| ------------------------- | --------------------------------------------------------------------------------- |
| `rejected_project_paused` | Push was rejected because the project's sync is paused via the admin sync control |

### Action Discriminator

| Action          | Meaning                                                     |
| --------------- | ----------------------------------------------------------- |
| `mutation_push` | Rejection occurred on `POST /sync/mutations/push`           |
| `chunk_push`    | Rejection occurred on `POST /sync/push` (legacy chunk push) |

Pull requests (`GET /sync/mutations/pull`) are never gated on pause status and never emit audit entries. Paused projects continue to serve reads to enrolled contributors without restriction.

### Retention and Pruning

There is no automatic retention policy in v1. Audit rows accumulate indefinitely. To prune entries older than 90 days, connect to Postgres and run:

```sql
DELETE FROM cloud_sync_audit_log
WHERE occurred_at < NOW() - INTERVAL '90 days';
```

Wrap in a transaction and add a `LIMIT` clause if the table is large:

```sql
BEGIN;
DELETE FROM cloud_sync_audit_log
WHERE id IN (
  SELECT id FROM cloud_sync_audit_log
  WHERE occurred_at < NOW() - INTERVAL '90 days'
  LIMIT 10000
);
COMMIT;
```

---

## MemoryLake Backend

Engram's default and source-of-truth storage is local SQLite (`internal/store`). **MemoryLake** is an optional, per-project alternate backend: memories for an *enabled* project are stored as MemoryLake V3 facts (under the `engram` workspace at your configured MemoryLake API) instead of the local database. Projects that are not explicitly enabled are completely unaffected — they keep today's SQLite behavior (FTS5/BM25 search, exact 1:1 int ids, verbatim content) with zero changes.

Both backends can be active at once on the same machine: project A can stay on SQLite while project B is enabled for MemoryLake. Routing is decided per `mem_*` call by resolving the calling project and checking the enablement list below.

### Environment Variables

Connection settings resolve in precedence order **env var > saved `engram memorylake config` > built-in default** (see `engram memorylake config` above). The env vars here are the highest-precedence layer; the "Default" column is the value used when neither the env var nor the saved config supplies one.

| Variable                                | Required to enable | Description                                                                                          | Default            |
| ---------------------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------ | ------------------ |
| `ENGRAM_MEMORYLAKE_BASE_URL`              | no                   | MemoryLake V3 API base URL. Overrides the saved config; falls back to the built-in default when unset everywhere.   | `https://app.memorylake.ai/openapi/memorylake` |
| `ENGRAM_MEMORYLAKE_API_KEY`               | yes (env or config)  | MemoryLake API key, sent as `Authorization: Bearer <key>`. Set it here or via `engram memorylake config --api-key`. | (unset)             |
| `ENGRAM_MEMORYLAKE_WORKSPACE`             | no                   | Workspace custom_id or `ws-...` id memories are stored under.                                                       | `engram`            |
| `ENGRAM_MEMORYLAKE_ACTOR`                 | no                   | Actor custom_id for writes/reads from this machine; distinguishes contributors when a project is shared.            | (unset — hostname)  |
| `ENGRAM_MEMORYLAKE_TIMEOUT_MS`            | no                   | Per-request HTTP timeout for the MemoryLake client.                                                                 | `30000`             |
| `ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS`       | no                   | **Deprecated, currently unused.** `mem_save` no longer polls for or waits on MemoryLake's fact extraction — it appends and returns immediately (see "Known limitations" below). Parsed but has no effect on any current code path. | `2000`              |
| `ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS`   | no                   | **Deprecated, currently unused.** Same as above.                                                                    | `30000`             |
| `ENGRAM_BACKEND`                          | no                   | Global safety valve. Set to `sqlite` to force **every** project onto local SQLite, even projects present in the enablement list below. Any other value (or unset) defers to per-project enablement. | (unset — per-project) |

### `engram memorylake` CLI

```
engram memorylake config [--url <url>] [--api-key <key>] [--workspace <ws>] [--actor <actor>] [--clear]
engram memorylake enable  --project <name> [--migrate]   # opt this project into the MemoryLake backend
engram memorylake disable --project <name>                # move this project back to local SQLite
engram memorylake status                                  # list every known project and its current backend
```

- `config` persists connection settings into `~/.engram/memorylake.json` (`connection`, file mode `0600` since it may hold the API key) and prints the resulting **effective** config; with no setter flags it just prints the effective config. Resolution precedence at runtime is **env var > this saved config > built-in default**, applied per field by `memorylake.LoadConfig` — so env vars still override the file for CI/one-off use, and the file overrides the defaults. When `--url` is never supplied (neither env nor saved), the base URL defaults to `https://app.memorylake.ai/openapi/memorylake`; workspace defaults to `engram`. The API key is masked in the printed output. `--clear` wipes the saved connection block (env vars/defaults then apply). This is the recommended alternative to exporting the `ENGRAM_MEMORYLAKE_*` env vars in every shell.
- `enable` records the project in `~/.engram/memorylake.json` (`enabled_projects`). The first `mem_*` call for that project afterward resolves (and caches) the MemoryLake workspace/project id, auto-creating the MemoryLake project under the `engram` workspace if it doesn't already exist.
- `disable` removes the project from that list; the project's `mem_*` calls immediately go back to SQLite. Any memories already written to MemoryLake are left in place (not deleted).
- `status` prints every project engram knows about (from local SQLite plus the enablement list) and which backend each currently uses.
- **First-enable auto-migration.** On the first `enable` for a project, engram automatically migrates that project's existing **non-deleted** SQLite observations into MemoryLake. It resolves/creates the MemoryLake project, reads the local observations, and writes each **verbatim** through MemoryLake's direct fact-add endpoint (`POST …/memories/facts`, which stores each text as-is with a real fact id, bypassing the asynchronous mem0 extraction/decision pipeline) — so migrated memories are stored faithfully and are immediately queryable, and the observation **title is preserved** (prepended to the content). SQLite stays the source of truth — **nothing is deleted locally**. The direct endpoint does not deduplicate, so migration makes itself **idempotent** by first listing the project's existing facts and skipping any observation whose text is already present; re-running writes only genuinely new memories. `--no-migrate` skips migration; `--migrate` forces an (idempotent) re-sync even on a re-enable. If MemoryLake is not configured yet (no API key) or a network error occurs, the project **stays enabled** and engram prints a warning to re-run `engram memorylake enable --project <name> --migrate` once it's fixed — enabling never fails just because migration couldn't run. (Note: this direct verbatim write is migration-only; live `mem_save` on an enabled project still goes through the conversation-append + async mem0 extraction path described below.)

### Per-project semantics

- **Default: SQLite, for every project.** MemoryLake is strictly opt-in — a project must be explicitly enabled before any of its `mem_*` calls touch the MemoryLake API.
- **Enabled projects only:** once a project is in the enablement list (and `ENGRAM_BACKEND` is not forced to `sqlite`), that project's `mem_*` calls route to MemoryLake. All other projects on the same machine are unaffected.
- **`ENGRAM_BACKEND=sqlite` is a global override**, not a per-project one — it forces SQLite for all projects regardless of the enablement list, useful for a full local/cloud comparison or an emergency rollback.
- Memories for enabled projects are stored under the `engram` workspace at your MemoryLake tenant (configurable via `ENGRAM_MEMORYLAKE_WORKSPACE`), which is how multiple people can share the same project's memories without each running their own local SQLite copy.

### Known limitations

- **Only `engram mcp` routes to MemoryLake; other interfaces still use SQLite.** Per-project MemoryLake routing is wired only through the MCP server (`engram mcp`, the interface stdio agents like Claude Code / Gemini / Codex use). The CLI (`engram save` / `engram search`), the TUI (`engram tui`), and the local HTTP API (`engram serve`, used by the OpenCode plugin and Pi extension) do **not** consult the enablement list — they always talk to local SQLite, even for a project that has been enabled for MemoryLake. As a result, the same enabled project is split-brained across interfaces: memories saved via MCP land in MemoryLake and are invisible to `engram search`/TUI/HTTP, and vice versa. This is a current limitation; use the MCP path exclusively for an enabled project until the other interfaces are routed too.
- **`mem_save` is a synchronous, verbatim direct write.** MemoryLake-backed saves post the observation straight to the direct fact-add endpoint (`POST …/memories/facts`, shipped 2026-07-23), which stores the text **verbatim** as a new fact and returns its **real fact id** — synchronously, bypassing the conversation-append + asynchronous mem0 extraction pipeline. So `mem_save`'s response `sync_id` **is** the materialized fact id and resolves via `mem_get_observation` immediately, and the observation **title is preserved** (prepended to the content). PassiveCapture saves and the best-effort session-end summary take the same direct-write path (a summary is memory-like, so it is stored as a first-class verbatim fact). Prompts (`mem_save_prompt`) still go through the conversation-append path — a prompt is not a memory, and because the fact-add endpoint carries no metadata, writing prompts as facts would make them indistinguishable from real memories in `mem_search`.
- **No server-side dedup, `topic_key` upsert, or conflict decision on the live write.** The direct fact-add endpoint stores each text as a new fact with **no** similarity check and **no** ADD/UPDATE/FORGET decision — so saving similar content twice creates two facts, and `topic_key` no longer produces an in-place upsert on a MemoryLake-backed project (every `mem_save` is a new fact). Engram's own local topic_key/hash upsert was retired for MemoryLake under the Option A thin adapter and is **not** reintroduced by the direct-write path; `mem_save` on a MemoryLake project does not return `judgment_required`/`candidates` (that loop is SQLite-only). `mem_judge`/`mem_compare` still work by reading MemoryLake's conflict-detection API. (Migration on first `enable` dedups against existing facts before writing, but live saves do not.)
- **Content is preserved verbatim.** Because the write is a direct fact-add (not a conversation message fed through mem0 extraction), `mem_get_observation` / search results return exactly the text Engram stored — no LLM paraphrase/merge, no `engram_raw` shadow copy needed.
- **`mem_search` is pure semantic search — no local keyword/fuzzy augmentation.** `SearchFacts` issues only MemoryLake's vector-search endpoint (`POST /api/v3/workspaces/{ws}/memories/search`); the earlier `topic_key`-slash-triggered keyword-fuzzy lookup (`fact_fuzzy`) and its dedup/pin-merge logic against semantic results are removed. Type/scope filtering still happens client-side, but a fact missing `engram_type`/`engram_scope` metadata (the common case for new saves, since Option A no longer stamps that metadata at save time) now passes the filter rather than being dropped.
- **No hard delete.** MemoryLake only supports "forget" (soft delete, expiring a fact); `mem_delete`'s `hard_delete` flag is accepted but ignored on MemoryLake-backed projects — every delete degrades to a soft delete.
- **`mem_merge_projects` is not supported** on a MemoryLake-backed project. Calling it against an enabled project returns an explicit unsupported error rather than attempting a cross-project migration; SQLite projects are unaffected and merge normally.
- **Several tools are a first cut on MemoryLake**, favoring "returns something reasonable" over full parity with SQLite: sessions (`mem_session_start`/`mem_session_end`/recent-session lookups), `mem_review`, prompt tracking (`mem_save_prompt`), passive capture (`mem_capture_passive`), and relation/conflict tooling (`mem_judge`, `mem_compare`, `FindCandidates`) either have reduced fidelity or are no-ops on the MemoryLake backend today. These gaps are tracked as follow-up work; see `internal/memorylake/backend.go` for the per-method TODOs and `docs/superpowers/specs/2026-07-22-memorylake-sqlite-parity-testing.md` for the full differential test matrix used to evaluate them against SQLite.
- **Object ids are just the MemoryLake fact id.** `sync_id` for a MemoryLake-backed observation *is* the MemoryLake fact id string — there is no local id-mapping layer any more (an earlier build maintained one, `~/.engram/memorylake-idmap.json`; that file is no longer read or written and can be deleted). Because a fact id is already globally unique to its project, a by-id lookup on a multi-project `engram serve` still cannot resolve to a different project's content.
- **`engram serve` by-id routing is in-process and lost on restart.** The HTTP server remembers which project a routed observation/session belongs to in an in-memory cache populated when it was created. After an `engram serve` restart that cache is empty, so a by-id request (`GET/PATCH/DELETE /observations/{id}`, `GET /sessions/{id}`, ...) for a MemoryLake-backed object it never saw this run falls back to local SQLite. Because MemoryLake observation ids are globally unique, this can only ever *miss* (SQLite doesn't have that id → `404`/not-found), never return another project's content. Re-issue the request after any read that re-populates routing (e.g. a project-scoped search/context call), or address the object through an interface that carries its project.
- **Session by-id lookups can still cross projects when a `session_id` string is reused.** Unlike observation int64 ids, session ids are opaque strings supplied by the caller. Within one `engram serve` instance, if two *different* enabled projects are driven with the **same** `session_id` string, a by-id session query (`GET /sessions/{id}`, `POST /sessions/{id}/end`) may resolve to whichever project's backend the in-process cache last recorded for that string. Use distinct session ids per project (the session ids agents generate are already unique in practice) to avoid this residual ambiguity; it does not affect observations, whose ids are globally unique.
- **By-id operations address only the *active* (non-soft-deleted) observation.** All sync_id-addressed operations over MCP and HTTP — `mem_update`, `mem_delete`, `mem_pin`/`mem_unpin`, `mem_review mark_reviewed`, `mem_timeline` — resolve the sync_id against **non-deleted** rows only (the adapter looks the id up via `GetObservationBySyncID`, which filters `deleted_at IS NULL`). Once an observation is soft-deleted it becomes not-found for every by-id call, so `mem_delete hard_delete=true` cannot reach an already-soft-deleted row through MCP or HTTP. To permanently purge a row that is already soft-deleted, use the CLI (`engram` delete by numeric id), which can still address soft-deleted rows directly.
- **A MemoryLake observation's numeric `#id` displays as `0`.** MemoryLake-backed observations have no local int64 primary key, so the decimal `#id` shown in human-readable `mem_*` output (e.g. `Memory marked reviewed: #0 …`) is always `0` — it is a display artifact, not a handle. The structured `sync_id` (the MemoryLake fact id string) is the real, addressable handle; use it for every by-id call.

### Agent tool surface on MemoryLake-backed projects

The `mem_*` MCP tool set is registered once, globally, and shared by both backends — nothing is removed or hidden per project. On a MemoryLake-backed project, though, several tools thin out or become unnecessary because mem0 now owns dedup/update/conflict-merge:

| Tier | Tools | MemoryLake-backed behavior |
| --- | --- | --- |
| Core (what agents mainly use) | `mem_save`, `mem_search`, `mem_context` | `mem_save` feeds the conversation to mem0 for async extraction/dedup/merge; `mem_search` is pure semantic recall; `mem_context` pulls recent + pinned. |
| Explicit control (still needed) | `mem_delete` (forget), `mem_get_observation` | mem0 only forgets on an explicit delete call, so this stays; `mem_get_observation` fetches a fact directly by id. |
| Engram-only capability (mem0 has no equivalent) | `mem_review` (recency decay), `mem_pin`/`mem_unpin`, `mem_session_*`, `mem_save_prompt`, `mem_capture_passive` | Kept — these wrap session/prompt/passive-capture semantics or provide decay-based ranking mem0 doesn't have. |
| Degraded / defer to mem0 | `mem_update`, `mem_judge`, `mem_compare`, `mem_suggest_topic_key` | `mem_update` still applies an explicit PATCH, but the recommended move is to `mem_save` again and let mem0 merge; `mem_judge`/`mem_compare` still work but read mem0's own conflict-detection API since Engram no longer generates local candidates; `mem_suggest_topic_key` is a harmless pure function but `topic_key` upsert semantics are weaker now that mem0 owns merging. |
| Unsupported | `mem_merge_projects`, hard delete, `mem_doctor` (redefined) | Unchanged from the rest of this section. |

Because `mem_save` on a MemoryLake-backed project never returns `judgment_required`/`candidates` (see "Known limitations" above), the `mem_judge`/`mem_compare` conflict loop is naturally silent there — agents never enter it. The agent-facing memory protocol (`plugin/*/skills/memory/SKILL.md`, `plugin/*/scripts/session-start.sh` and `post-compaction.sh`) reflects this with one added note: on a MemoryLake-backed project (check `engram memorylake status`), dedup/update/conflict-merge is automatic and agents only need `mem_save` / `mem_search` / `mem_context`, without needing to call `mem_update` / `mem_judge` / `mem_compare` themselves. Default SQLite projects keep the full protocol unchanged — this is one shared, session-level protocol (a session can span both backends across projects), not a separate protocol per backend.

---

## Next Steps

- [Agent Setup](docs/AGENT-SETUP.md) — connect your agent to Engram
- [Plugins](docs/PLUGINS.md) — what the OpenCode and Claude Code plugins add beyond bare MCP
- [Obsidian Brain](docs/beta/obsidian-brain.md) — visualize memories as a knowledge graph (beta)
- [Contributing](CONTRIBUTING.md) — how to contribute
