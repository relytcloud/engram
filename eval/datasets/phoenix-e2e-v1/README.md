# eval/datasets/phoenix-e2e-v1

Frozen L3 end-to-end task set (spec: `docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`,
§L3). Schema is `eval/e2e.Task` / `eval/e2e.Rubric` (`eval/e2e/task.go`); the
runner that executes these tasks headlessly against the phoenix repo across
memory arms lands in Task 9 — this task only freezes the schema and the task
set.

- **Constructed:** 2026-07-24.
- **Target repo:** `/workspace/phoenix` (read at task-authoring time; the
  runner executes headless agents directly against this shared tree — the
  mitigation is task design: every prompt asks the agent to investigate and
  *propose* a change, never to apply one. Worktree isolation is deferred to
  Phase 2's first L3 change).
- **Task count:** 15, ids `arch-001`…`arch-005`, `gotcha-001`…`gotcha-005`,
  `fix-001`…`fix-005`.
- **Categories:** `architecture-qa` (5), `gotcha` (5), `small-fix` (5).
- **Defaults:** `max_turns: 30`, `timeout_min: 20`, except `architecture-qa`
  which uses `max_turns: 20`, `timeout_min: 15` (lighter — no code changes
  expected, just investigation/read).
- Note: the `architecture-qa` `max_turns: 20` / `timeout_min: 15` sit within
  the brief's bounds — the brief's `15` / `10` figures were a suggestion, not a cap.

## Raw material

Per spec §L3, mined from three sources, all read-only:

1. **`/workspace/phoenix/CLAUDE.md`** — build-mode table, repo layout,
   ZDB module-structure table, the isolation2-test conventions section
   (FDB-introspection UDFs are global, autovacuum/GUC leakage, table-name
   collisions), and the "Investigation Guidelines" section (don't
   hypothesize about gmeta/submodule internals not checked out locally).
2. **`git -C /workspace/phoenix log --oneline -150`** (dumped to
   `phoenix-gitlog-150.txt` in the construction scratchpad) — used to find
   real fix commits for the `small-fix` tasks. Every `small-fix` task's
   `answer_points` were checked against the actual `git show <hash>` diff
   before being written, not just the commit subject line.
3. **The phoenix MemoryLake facts dump** (Task 6's `phoenix-facts.jsonl`,
   5 usable facts) — used for `architecture-qa` tasks `arch-004`/`arch-005`
   (the heap/ZDB mixed-partition direct-dispatch bug fact) reframed as
   general dispatch-architecture questions rather than the specific bug, and
   for `gotcha-002` (the fake-OID convention fact).

## Per-task sources

| Task | Category | Source |
|---|---|---|
| `arch-001` | architecture-qa | CLAUDE.md "ZDB Module Structure" table |
| `arch-002` | architecture-qa | CLAUDE.md "Build Modes" table |
| `arch-003` | architecture-qa | CLAUDE.md "Project Overview" / repository layout |
| `arch-004` | architecture-qa | CLAUDE.md + code (`src/backend/cdb/cdbtargeteddispatch.c`); same underlying mechanism as the `fact-9714b3a87bfe...` patent-point fact, reframed to not leak the bug/fix |
| `arch-005` | architecture-qa | code (`src/backend/gporca/libgpopt/src/translate/CTranslatorExprToDXLUtils.cpp`); same fact as above |
| `gotcha-001` | gotcha | CLAUDE.md "Pre-build (mandatory after rebase / pull / branch switch)" section (delta-kernel-rs FFI header regen) |
| `gotcha-002` | gotcha | fact `fact-4c762f9b5c834594a74f23318df1af67`; fix commit `8b3ccf6a929` (to #1889, MR!2003, 2026-04-03) — verified via `git show 8b3ccf6a929` |
| `gotcha-003` | gotcha | CLAUDE.md "Isolate new regression tests from surrounding tests" section (FDB introspection UDFs are global) |
| `gotcha-004` | gotcha | CLAUDE.md same section (autovacuum interactions / GUC leakage) |
| `gotcha-005` | gotcha | CLAUDE.md "生成 expected .out 文件" note (pg_isolation2_regress vs pg_regress format) |
| `fix-001` | small-fix | fix commit `b45797d6136` "Fix race condition in CTE reader-writer communication (#16431)" — verified via `git show b45797d6136` (`src/backend/executor/nodeShareInputScan.c`) |
| `fix-002` | small-fix | fix commit `cf630f5eaf1` "save errno before ML_CHECK_FOR_INTERRUPTS()" (`src/backend/cdb/motion/ic_udpifc.c`, `ic_tcp.c`) — verified via `git show cf630f5eaf1`. Provenance (verified `git merge-base --is-ancestor cf630f5eaf1 7e714adde36` → ancestor): `cf630f5eaf1` (2025-12-03) first applied the `save_errno` pattern to **both** `ic_tcp.c` and `ic_udpifc.c`; then a later descendant commit `7e714adde36` ("fix #1841: Improper use of errno", 2025-12-04) **regressed** `ic_tcp.c` by removing that fix. Current phoenix HEAD's `ic_tcp.c` `readPacket` therefore still reads raw `errno` (the bug is live today), so `fix-002`'s `answer_points[3]` — flagging the TCP path as still carrying the same bug — remains accurate |
| `fix-003` | small-fix | fix commit `34fbfd18354` "fix #1846: process SIGUSR1 in pgstat after InitPostgres()" (`src/backend/postmaster/pgstat.c`) — verified via `git show 34fbfd18354` |
| `fix-004` | small-fix | fix commit `d3aedae9cfd` "Fix dropped-table BACKUP space key/file leaks" (`contrib/zdb/src/storage/zdb_vacuum.c`) — verified via `git show d3aedae9cfd`. This is also the commit that introduced CLAUDE.md's "Investigation Guidelines" section, so the task deliberately probes whether the agent respects that rule (gmeta-side root cause is out of worktree) rather than inventing gmeta internals |
| `fix-005` | small-fix | fix commit `3e1505ce335` "fix #1853, add `regexp_matches` into non dispatch functions" (`contrib/relyt/relyt--3.22.sql`, carried forward through `relyt--3.43.sql`) — verified via `git show 3e1505ce335` and confirmed the entry persists in the current default-version SQL file |

Fix commit hashes are recorded here, in this README, **only** — never in a
task's `prompt` or `rubric.answer_points` text, so a headless agent cannot
"cheat" by grepping git log for the exact commit.

- Note: `fix-004` intentionally carries one `gotcha_points` entry — it tests the
  "don't hypothesize about gmeta/submodule internals not checked out locally"
  rule (CLAUDE.md "Investigation Guidelines"); it is the only `small-fix` task
  with a non-empty `gotcha_points`.

## Grading model

Each task is graded by an LLM judge using `judge_prompt.md`
(`{{TASK_PROMPT}}`, `{{ANSWER_POINTS}}`, `{{GOTCHA_POINTS}}`,
`{{AGENT_RESULT}}` substituted in) against the agent's final answer text —
not against an applied code diff. This is deliberate: these tasks run
headless on `cwd=/workspace/phoenix` with no repo modifications persisted
(Task 9's runner handles isolation), so `small-fix` tasks are designed to be
gradable from the agent's *analysis and proposed change* (file/function
names, root cause, fix direction) without requiring the fix to actually be
applied and tested.

## Arm-comparison protocol

Per spec §L3, the same 15 tasks run once per memory arm (e.g. no-memory
baseline vs. engram-memory-enabled) with everything else held constant —
same model, same `max_turns`/`timeout_min` per task, same judge prompt. Only
the `answer_points` that a memory-enabled arm can plausibly retrieve (i.e.
material that exists in CLAUDE.md, the phoenix facts dump, or git history —
see "Per-task sources" above) are expected to show a measurable score
delta; tasks whose answer requires only reading code already present in the
checked-out repo (most of `architecture-qa`) serve as a control where memory
should not matter much, since the code is directly discoverable either way.

## Freeze rule

This task set is **frozen**: once merged, `eval/datasets/phoenix-e2e-v1/tasks/*.json`,
`judge_prompt.md`, and this README are never edited to make scores look
better. If a task turns out to be wrong, ambiguous, or the underlying
phoenix code/history changes enough to invalidate it, fix forward — create
`eval/datasets/phoenix-e2e-v2/` with the correction and switch the runner
default over deliberately, so historical L3 results that reference
`phoenix-e2e-v1` stay reproducible against the exact task set they were
scored on.

## Read-only rule

Task authoring only ever *reads* from `/workspace/phoenix` (via `git log`,
`git show`, and file reads) and from the previously-dumped, already-frozen
`phoenix-facts.jsonl` / CLAUDE.md content. Nothing in this process modifies
the phoenix repository or any MemoryLake/engram data.
