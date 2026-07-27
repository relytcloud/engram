# Baseline Report — Memory Eval Foundation (Phase 1)

**Date**: 2026-07-24 (L3 re-judge completed 2026-07-27)
**Baseline commit**: `2adc24c` (engram, branch feat/memory-eval-foundation)
**Spec**: `docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`
**Test project**: phoenix (MemoryLake-backed, proj `proj-52bfd150…`)

## Phase 2 acceptance anchors

| Anchor | Baseline | Phase 2 target |
|---|---|---|
| **L3 memory uplift** (mean e2e score, memory − no-memory) | **2.267** (6.000 − 3.733) | **≥ 4.53** (uplift ×2) |
| **L2 injected tokens / session** | **7824** | **≤ 3912** (÷2) |

## L1 — Retrieval quality (54 QA, live MemoryLake)

recall@1/5/10 = 0.759 / 0.796 / 0.796, MRR 0.778 (LLM-verified: 0.778 / 0.815 / 0.815, MRR 0.796). Avg payload 517 tokens/query, latency p50/p95 = 529/1023 ms.
Scorecards: `2adc24c-2026-07-24-l1.json`, `…-l1-llm.json`.

## L2 — Injected token breakdown

| Slice | Tokens | Share |
|---|---|---|
| SessionStart hook stdout | 3202 | 41% |
| memory SKILL.md | 1587 | 20% |
| MCP server instructions | 787 | 10% |
| `mem_context` payload | 697 | 9% |
| retrieval (517 × 3 assumed calls) | 1551 | 20% |
| **Total** | **7824** | |

Assumptions recorded in scorecard Env: `search-calls=3.0` (placeholder pending real session replay), tokenizer approx-bytes/4.
Scorecard: `2adc24c-2026-07-24-l2.json`.

## L3 — End-to-end (15 tasks × 2 arms × N=2, model sonnet)

Overall: memory 6.000, no-memory 3.733, **uplift +2.267**. All 60 runs judged (0 judge failures after the judge tools fix; one transient JSON error absorbed by retry).

Per category:

| Category | memory | no-memory | uplift |
|---|---|---|---|
| architecture-qa (5) | 8.00 | 8.90 | **−0.90** |
| gotcha (5) | 8.50 | 0.00 | **+8.50** |
| small-fix (5) | 1.50 | 2.30 | **−0.80** |

Cost signal: memory arm produced 337k output tokens vs 228k for no-memory (+48%) across the same tasks.

### Reading the numbers

1. **The entire uplift comes from gotcha tasks.** Where the needed knowledge exists as a memory fact, memory is decisive (8.50 vs 0.00). The no-memory arm scored 0 on every gotcha run — it cannot recover tribal knowledge from the repo alone within the turn budget.
2. **Memory is currently a net drag on architecture-qa (−0.90) and small-fix (−0.80).** The protocol pushes the agent to spend turns on mem_* calls and memory-flavored narration; on tasks the repo itself can answer, that is pure overhead. Example: fix-003 (memory arm) produced a final message that says "the analysis has been saved to memory" instead of the analysis itself — scored 0 despite doing the work.
3. **small-fix is turn-starved for both arms**: 8/60 runs ended with empty result_text (max_turns 30 exhausted before a final answer; scored 0 per the spec's circuit breaker). Task difficulty is high for both arms; treat fix-category scores as noisy.
4. **Interpretation caveats**: N=2; L1 corpus is 5 usable facts mined into 54 cases (see dataset README); judge is single-model (sonnet) with frozen rubric; memory-arm sessions run the full current protocol (7824-token injection).

### What this means for Phase 2 (directional, not committed)

- Biggest effect lever: make memory stop hurting arch/fix (protocol slimming, "answer first, save silently" behavior) while keeping the gotcha advantage — that alone moves uplift toward ~3.5 without touching retrieval.
- Biggest cost lever: the static protocol is 71% of injected tokens (hook + SKILL + MCP instructions = 5576); halving the total requires cutting there, retrieval trimming alone cannot reach ≤ 3912.
- The +48% output-token overhead of the memory arm is a second cost axis not yet in the L2 composite; consider adding it as a metric in Phase 2.

## Incidents & environment notes (recorded for reproducibility)

- L3 attempt 1 died on the first judge call (transient claude exit 1); fixed by judge retry + checkpoint/resume (`6a40a5a`), then a deterministic judge failure mode (tool-use temptation under --max-turns 1) killed 24/60 verdicts; fixed by disabling tools for judge calls (`eab9e3a`). Re-judge used the checkpoint sidecar — no task re-runs.
- Tests intentionally hit live MemoryLake (user decision, canary value); the isolation commits were reverted (`3f3b506`, `36d9870`). The phoenix eval corpus was verified junk-free before baselines (6 facts; 28 junk fixtures were cleaned from the *engram* project on 2026-07-24).
- Sandbox Go env quirks documented in `/workspace/work/goenv.sh`.
