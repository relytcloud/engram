# Phase 2 Optimization Loop — Design

**Date**: 2026-07-27
**Status**: Approved (user-reviewed section by section)
**Program spec**: `docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`
**Baseline**: engram `2adc24c` scorecards (`eval/results/`, v0.3.0)
**Inputs**: research priority matrix (`docs/research/2026-07-24-memory-sota-survey.md` §5), baseline report (`eval/results/baseline-report.md`)

## Goal (program acceptance anchors)

| Anchor | Baseline | Target |
|---|---|---|
| L3 memory uplift (mean e2e score, memory − no-memory) | 2.267 | **≥ 4.53** |
| L2 injected tokens per session | 7824 | **≤ 3912** |

Baseline decomposition that drives this design: all uplift comes from gotcha
tasks (+8.50); memory is a net drag on architecture-qa (−0.90) and small-fix
(−0.80); the memory arm emits +48% output tokens; the static protocol is 71%
of injected cost (hook 3202 + SKILL 1587 + MCP instructions 787).

## Mode & scope

- **Fully autonomous loop** (user decision): including L3 milestones; escalate
  only on unresolvable bottlenecks.
- **Change surface** (user decision): protocol/plugin text, engram Go client,
  MemoryLake server-side *proposals* (documents only, non-blocking).
- Composition: **Wave 1 = R1–R5** (two tracks: cost R1/R3, effect R2/R4,
  guardrails R5), each independently implementable, measurable, revertable.

## Loop protocol

```
for Rn in R1..R5:
  implement (TDD, SDD subagent flow, opus/fable models)
  → go test ./... green (+ internal/paritytest)
  → run L1 + L2 → scorecard vs previous round
  → behavioral changes (R2/R4): targeted L3 subset (6 tasks × 2 arms × N=1)
  → effective: keep + record (ledger + engram memory)
  → ineffective/regressed: fix once; if still failing, git revert, record why
after all: full L3 (N=2) → acceptance vs both anchors → final report
```

Quantified keep/revert gates:
- R1/R3 (cost track): L2 injected tokens drop AND L1 recall@5 regression ≤ 0.03.
- R2/R4 (effect track): targeted L3 subset mean does not drop (R2 must raise
  the fix/arch subset; R4 must not drop gotcha).
- The targeted subset always includes fix-003 and arch-005 (known victims).

## R1 — Static protocol slimming & layering (cost lead, ~−4000 tok)

- Rewrite the hook-injected protocol to a **compact core** (~800 tok): when to
  save/search/context, MemoryLake-project simplifications. Details (conflict
  loop, topic_key format rules, examples) live only in
  `plugin/*/skills/memory/SKILL.md` — the on-demand skill the hook text
  currently duplicates.
- MCP server instructions trimmed to ~300 tok (one-line tool roles + pointer
  to the skill).
- Reuse the existing `engram protocol-mode` mechanism: `compact` (default) /
  `full` (rollback path).
- Version header + stable section ordering for prompt-cache friendliness
  (bonus; not counted toward the token anchor).

## R2 — Answer-first behavior correction (uplift lead)

Replace the current protocol's save-eager behavioral bias with three rules:
1. **The final reply must contain the complete answer** — memory serves future
   sessions, never substitutes for answering now.
2. **Silent saves**: mem_save is never narrated and never replaces the answer;
   batch saves at task end.
3. **Bounded retrieval**: one mem_search attempt at task start; on miss,
   proceed normally — no repeated searching.

Validation: targeted L3 subset must include fix-003 (agent answered "saved to
memory" instead of the analysis → scored 0) and arch-005 (6,4 vs 10,10).
Monitor mem_save call counts from the L3 sidecar to confirm "silent ≠ absent".

## R3 — Progressive retrieval (retrieval-side cost)

- **mem_search index mode**: hits return `{id, title, 1–2 sentence summary,
  token_count, score}`; full body via `mem_get_observation`. Tool description
  steers "scan index first, expand deliberately".
- **mem_context layering**: pinned block (R4) + one-line recent-session index
  + summaries of the 3 most recent observations; total budget ≤ 400 tok
  (currently 697).
- Optional `budget` parameter on both tools; over-budget results truncated by
  relevance with an explicit "N more omitted" marker.
- Implementation: `internal/mcp` response assembly + `internal/memorylake`
  (FormatContext/Search); SQLite backend changed in lockstep (paritytest
  gates).
- Summaries must preserve key entities (deterministic rule, not free
  paraphrase), so gotcha-critical identifiers survive indexing.

## R4 — `mem_pin` core-memory block (gotcha reinforcement + cache stability)

- Pinned facts render as a "core memory" section at the top of mem_context,
  **hard cap 1KB** (~10 one-line facts); when over cap, oldest pin demotes
  with a notice.
- Stable ordering (by pin time) → stable prompt prefix.
- MemoryLake backend already has PinObservation/isPinned; work is
  FormatContext rendering + cap logic (+ SQLite parity).
- Protocol text gains a pin criterion: "pin only facts whose loss causes
  irreversible damage or repeated pitfalls".
- Eval corpus: pin 2–3 real phoenix gotcha facts through the normal usage
  path — disclosed in the round report (legitimate usage, not test-teaching;
  the L1 dataset stays frozen and untouched).

## R5 — Eval guardrails (anti-gaming, anti-drift)

- **distractor-ratio** in L1: share of non-hits ranked above the first hit,
  per query and aggregated — guards R3's truncation from promoting noise.
- **Mandatory no-memory comparison row** in the report generator (Zep audit
  lesson: "memory looks helpful but isn't" is the worst silent failure).
- **`evalrun -suite compare -base <card> -cur <card>`** wiring for
  `scorecard.CompareMarkdown` (also resolves the final review's dead-code
  note).

## MemoryLake server proposals (parallel, non-blocking)

One proposal document per topic, produced alongside the loop: temporal
validity fields (`valid_from`/`valid_until` de-ranking), relevance-threshold
control on search, batch endpoints. Delivered to the server team; no client
work depends on them in Wave 1.

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| R1 slimming degrades protocol adherence (save rate drops) | count mem_save calls in targeted L3 sidecar; L1 corpus frozen so retrieval metrics unaffected; if degraded, re-add text incrementally |
| R2 overcorrects (agent stops saving) | same save-count monitor; rules say silent, not absent |
| R3 index mode starves the agent (too lazy to expand) | targeted L3 gotcha subset must not drop; summaries keep key entities by rule |
| SQLite/MemoryLake behavior divergence | every round passes `go test ./...` + `internal/paritytest` |
| Protocol changes affect all users | behavioral changes ship via PR + release cadence with scorecards as PR evidence |
| Optimization pollutes frozen datasets | datasets remain read-only; R4 pinning goes through the real usage path and is disclosed in round reports |

## Testing strategy

- TDD per item; protocol-text changes get golden-file tests against accidental
  drift; R3/R4 assembly logic unit-tested; paritytest for backend parity.
- Round-level: R5 guardrail metrics + scorecard comparison every round.
- Behavior-level: targeted L3 subsets; final full L3 (N=2) for acceptance.

## Out of scope (Wave 1)

- Remaining matrix items (consolidation pass, temporal schema client work,
  relations graph, path-glob injection, LongMemEval/LoCoMo/BEAM fixtures,
  scale sweeps) — candidates for Wave 2 based on Wave 1 residual gap.
- Write-time compression of stored facts (explicit non-goal per matrix
  guardrail row).
- Accurate tokenizer (count-tokens API) — unless a round's decision hinges on
  approx-vs-real divergence, in which case it becomes part of that round.
