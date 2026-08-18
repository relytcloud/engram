# Memory Eval & Optimization Program — Design

**Date**: 2026-07-24
**Status**: Approved (user-reviewed section by section)
**Baseline commit**: `01b1e9b` (engram main)
**Test project**: phoenix (`/workspace/phoenix`), already MemoryLake-enabled (`proj_id=proj-52bfd150b13041389ebe2e2c66f96294`)

## Goal

Improve the engram + MemoryLake memory system so that, measured on the phoenix
project as a realistic testbed:

1. **Effect ×2** — the *memory uplift* doubles.
   `uplift = mean e2e task score across the task set (with memory) − mean
   e2e task score (no memory)`.
   Acceptance: `uplift(optimized) ≥ 2 × uplift(current)`.
   Rationale: absolute-score doubling is mathematically unreachable when the
   baseline score is already high; uplift isolates the contribution that is
   attributable to the memory system.
2. **Cost ÷2** — the token overhead injected by the memory system per session
   drops to ≤ 50% of the current version, with no effect regression.
   "Injected tokens" = static protocol text + `mem_context` payload +
   `mem_search` payloads × average calls per session.

Retrieval quality (L1 below) is a **process metric** used to steer iteration;
it is not the acceptance metric.

## Scope of change

All three layers are in scope:

- **engram client, all layers** (this repo): `mem_*` tool response formats,
  truncation strategies, retrieval pipeline, hook-injected protocol text,
  MemoryLake call patterns, caching.
- **Memory protocol / prompt layer**: SessionStart hook output (currently
  ~10.3 KB), MCP server instructions, tool descriptions.
- **MemoryLake server side**: the server code is NOT local (external service
  at `app.memorylake.ai`); server-side improvements are delivered as
  requirements/interface-proposal documents, not code.

## Architecture: three phases

```
Phase 0 research ──→ Phase 1 eval foundation ──→ Phase 2 optimization loop
 (SOTA → engram map)   (L1/L2/L3 + baseline)      (change → L1/L2 each round,
                                                   L3 at milestones)
```

Each phase has its own deliverable and its own spec → plan → implementation
cycle. This spec covers the overall architecture plus the detailed design of
Phase 0 and Phase 1. Phase 2's concrete optimization items are deliberately
deferred until Phase 0/1 data exists — choosing them now would be guessing.

## Phase 0 — Research

Parallel research agents sweep four directions. Every item is reported in a
uniform format: *technique description → concrete candidate change mapped to
engram/memorylake → expected effect/cost impact → implementation difficulty*.

1. **General agent memory systems**: MemGPT/Letta, Mem0, Zep/Graphiti, A-Mem,
   LangMem — memory organization, retrieval, forgetting/merging strategies.
2. **Coding-agent-specific memory**: Claude Code native memory & CLAUDE.md
   practice, Cursor Memories, Devin Knowledge, Windsurf.
3. **Memory evaluation methodology**: LongMemEval, LoCoMo, the Mem0 paper's
   eval protocol — directly informs L1/L3 dataset construction.
4. **Cost-side techniques**: prompt-cache-friendly injection layout, context
   compression, progressive/layered retrieval (index first, expand on
   demand), structured summaries.

**Deliverable**: `docs/research/2026-07-24-memory-sota-survey.md`, ending
with a priority matrix (candidate change × expected gain × difficulty) that
feeds Phase 2 directly.

## Phase 1 — Evaluation foundation

New top-level `eval/` directory in this repo (Go package + datasets + runner
scripts), isolated from `internal/*`, never shipped in the release binary.
Every run emits a versioned JSON scorecard (`eval/results/<git-sha>-<date>.json`);
a report generator renders markdown comparison tables.

Budget model: **layered pyramid** — cheap metrics (L1+L2) run every
iteration; expensive e2e (L3) runs only at milestones.

### L1 — Retrieval quality (cheap, every iteration)

**Dataset**: `eval/datasets/phoenix-retrieval-v1.jsonl`, 50–100 QA pairs from
three sources:

- phoenix memories already migrated into MemoryLake (real content → reverse
  question construction);
- key decisions/fixes in phoenix git history (answers verifiable against
  commits);
- gotcha knowledge in phoenix `CLAUDE.md` (e.g. "must regenerate the
  delta-kernel FFI header after rebase").

Entry format: `{question, expected_keywords[], expected_fact_hint, category}`.

**Hit judgment**: keyword-group matching first, LLM verification only for
borderline cases (pure keywords misjudge paraphrases; pure LLM is too
expensive per round).

**Execution**: real path — `engram search` CLI / MCP `mem_search` against the
live MemoryLake backend, no mocks. Metrics: `recall@k` (k=1,5,10), MRR,
tokens returned per query (feeds L2), latency P50/P95.

**Async-extraction hazard**: live `mem_save` goes through conversation-append
plus the asynchronous mem0 extraction pipeline, so writes are not immediately
searchable. Dataset construction must poll for extraction completion before
freezing; the eval phase itself is read-only.

### L2 — Token accounting (cheap, every iteration)

| Slice | Content | Measurement |
|---|---|---|
| Static protocol overhead | SessionStart hook output (~10.3 KB today) + MCP server instructions + `mem_*` tool schemas | one-off text extraction, tokenizer count |
| Session-start overhead | `mem_context` payload | real call against the phoenix project, count |
| Retrieval dynamic overhead | `mem_search` payloads × typical calls/session | counted during L1 batch runs + replay of real session call sequences |

Tokenizer: Anthropic count-tokens API as the accurate path, local `chars/4`
approximation as the fast path. Output is a composite "injected tokens per
session" metric (static + start + retrieval × average call count).

### L3 — End-to-end (expensive, milestones only)

**Task set**: `eval/datasets/phoenix-e2e-v1/`, 12–16 tasks from real phoenix
work, three categories:

- **Architecture QA** (4–5): e.g. "where does ZDB store visimap metadata and
  how is it GC'd" — graded against an answer-point checklist.
- **Gotcha reproduction** (4–5): e.g. "build fails after rebase with
  `PrimitiveType undeclared`, fix it" — the correct path lives in
  CLAUDE.md/history; tests whether memory prevents wasted exploration.
- **Small modification tasks** (4–6): small bugs picked from phoenix git
  history that have real fix commits; agent output is compared against the
  actual commit.

**Execution**: `claude -p` headless, cwd = phoenix, three arms:

- **no-memory**: engram fully isolated (exact isolation mechanism decided in
  the plan phase — must be clean: no MCP tools, no hooks, no protocol text);
- **current**: engram binary at the baseline commit;
- **optimized**: the iterated binary.

Per-task token budget and timeout; milestone runs use N=2 per task, averaged,
to control variance.

**Grading**: LLM judge with a pre-written per-task rubric (answer-point hits,
gotcha avoidance, equivalence to the real fix commit), 0–10, plus objective
signals (correct file paths cited, CLAUDE.md-required steps executed). Judge
prompts and rubrics are committed to git so rounds stay comparable.

**Deliverable**: baseline scorecard — the current-vs-no-memory uplift is the
number Phase 2 must double.

## Phase 2 — Optimization loop

Fixed per-round protocol:

```
pick optimization item (priority from research matrix + baseline data)
  → implement (TDD, normal repo contribution flow)
  → run L1 + L2, compare against previous scorecard
  → keep & record if effective; revert & record reason if not
  → after several rounds, run an L3 milestone
```

Each round produces one scorecard record and one engram memory (decision +
data), forming a traceable optimization log — this is the "self-optimize,
self-reflect" vehicle.

**Preliminary candidate directions** (direction-level only; priorities and
trade-offs are decided in Phase 2's own plan after Phase 0/1 data exists):

1. **Static protocol slimming** — the 10.3 KB SessionStart injection is the
   most visible fat; compress protocol text, load details on demand.
2. **Layered `mem_context`** — return a lightweight index/summary first, let
   the agent expand via `mem_get_observation`; progressive retrieval is the
   industry-standard token saver.
3. **`mem_search` payload governance** — dedup, relevance-threshold cutoff,
   token-budget-based trimming.
4. **Prompt-cache-friendly layout** — keep static protocol content as a
   stable prefix; avoid busting the cache every session.
5. **MemoryLake server proposals** — retrieval relevance, fact
   merge/compression, batch endpoints; delivered as requirement docs +
   interface proposals (server code not local).

**Acceptance**: L3 uplift ×2 AND L2 injected tokens ≤ 50% of baseline →
final acceptance report. If after multiple rounds the goals are approached
but not met, the report states the achieved ratios and bottleneck analysis
honestly — no dressing up.

## Risks & error handling

| Risk | Mitigation |
|---|---|
| MemoryLake async extraction causes eval to read incomplete data | poll extraction status before dataset freeze; eval phase is read-only |
| MemoryLake is an external service; behavior/latency may drift | scorecards record eval time + service response characteristics; L1 every round detects server-side drift |
| L3 variance causes misjudgment | milestone N=2 averaged; rubric + judge prompt frozen in git; manual spot-check when between-arm differences are anomalous |
| QA dataset "teaching to the test" during optimization | dataset frozen after construction — never modified because of scores; new memory content must not plant answers |
| Runaway token burn in phoenix headless sessions | per-task token cap + timeout circuit breaker; over-limit scores 0 and is flagged |
| Optimization breaks the default SQLite path | every round must pass `go test ./...` + `internal/paritytest`; SQLite behavior regression is a blocker |

## Testing strategy

- **Eval framework itself**: metric computation (recall/MRR/token stats),
  dataset parsing, and scorecard comparison logic all unit-tested, running
  under `go test ./...`.
- **Product changes**: existing repo rules — TDD, conventional commits,
  issue-first, e2e tag separation; `internal/paritytest` remains the
  MemoryLake↔SQLite behavioral-consistency gatekeeper.
- **Judge stability**: calibrate the LLM judge with 3–5 fixed fixture answers
  of known quality (good/medium/bad); only use it for real evals once scoring
  is monotonic and sensible.

## Out of scope

- Rewriting MemoryLake server internals (external service; proposals only).
- Fully unattended self-optimization loop automation (possible later upgrade
  once Phase 2 is stable; not part of this program's acceptance).
- Changes to phoenix itself — phoenix is a read-mostly testbed; e2e tasks
  run in throwaway worktrees/branches.
