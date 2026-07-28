# Wave 1 Acceptance Report — Phase 2 Optimization Loop

**Date**: 2026-07-28
**Branch**: `feat/phase2-wave1` (final L3 at binary version 0.4.0, slim protocol active)
**Baseline**: `2adc24c` scorecards (v0.3.0)
**Spec**: `docs/superpowers/specs/2026-07-27-phase2-optimization-loop-design.md`

## Anchor verdicts

| Anchor | Baseline | Wave 1 | Target | Verdict |
|---|---|---|---|---|
| L3 memory uplift | 2.267 | **2.983** (+31.6%) | ≥ 4.53 | **NOT MET** (66% of target) |
| L2 injected tokens/session | 7824 | **6084** (−22.2%) | ≤ 3912 | **NOT MET** (harness metric; see both-views note) |

Both anchors improved materially; neither reached its ×2/÷2 target. Per the
spec, this report states achieved ratios and bottlenecks without dressing up.

**Both-views note (L2)**: the harness metric counts `SKILL.md` (1982 tok) as a
static per-session slice, but the skill is loaded on demand in real Claude Code
sessions (Task 4 measurement caveat, ledger W1-T4). Excluding it, the
real-session injected cost is **3885 tok** (hook 2844 + MCP instr 184 +
context 240 + search 205.6×3), which is *below* the 3912 target. We report the
harness metric as the official verdict because it is the instrument the spec
anchored; the real-session view says the remaining gap is concentrated in one
place (the hook, see bottlenecks).

## Mandatory no-memory comparison (R5 guardrail)

| Arm | Final mean (N=2, 15 tasks) | Baseline mean |
|---|---|---|
| memory | 6.967 | 6.000 |
| no-memory | 3.983 | 3.733 |

All 60 verdicts judged (0 failures after checkpoint re-judge; see incidents).

## Per-category L3 (memory / no-memory / uplift)

| Category | Baseline | Wave 1 | Movement |
|---|---|---|---|
| architecture-qa | 8.00 / 8.90 / **−0.90** | 10.00 / 9.25 / **+0.75** | memory drag CURED; memory now a small net positive |
| gotcha | 8.50 / 0.00 / **+8.50** | 9.20 / 0.00 / **+9.20** | advantage widened (pinned core memory) |
| small-fix | 1.50 / 2.30 / **−0.80** | 1.70 / 2.70 / **−1.00** | still a drag; near-floor scores for both arms |

Output-token overhead of the memory arm: baseline +48% (337k vs 228k) →
Wave 1 **+9%** (283k vs 260k) — the answer-first rules removed most of the
memory-driven verbosity. Empty-result (turn-starved) runs: 8/60 → 6/60.

## Round ledger (what shipped, what each round measured)

| Round | Change | Gate result | Kept |
|---|---|---|---|
| R1 (T2–T5) | slim protocol 1265B + MCP instructions 736B, protocol-mode revived | recall@5 flat 0.796; R1-controlled slices −336 tok measured (−869 real-session) | ✓ |
| R2 (T6–T7) | answer-first rules in all 12 agent-facing surfaces; slim survives compaction | targeted fix/arch memory mean 2.70→5.00; gotcha held; saves still occur (audit) | ✓ |
| R3 (T8–T10) | mem_search index mode + budget; layered mem_context ≤400 tok | tokens/query 991→205.6; context 1636→88; recall@5 flat; distractor 0.133; gotcha not starved | ✓ |
| R4 (T11) | pinned core-memory block (1KB cap) + pin criterion; 2 real phoenix gotcha facts pinned | targeted gotcha memory mean 8.75 ≥ 8.5 | ✓ |

All five optimization items kept; zero reverts. Corpus note: the live phoenix
corpus grew 6→15 facts during the wave (our own L3 memory-arm saves + real
user sessions) — raw L2 comparisons across rounds carry that organic-growth
term; like-for-like slice deltas are recorded per round in the ledger.

## Bottleneck analysis (what blocks each anchor)

**Uplift (2.983 vs 4.53).** The gap decomposes cleanly:
- gotcha is nearly saturated (9.20 with a 10 ceiling) and arch is saturated
  (10.00) — together they cannot contribute more than ~0.5 additional uplift.
- **small-fix is the entire remaining lever**: memory is still −1.00 there,
  and both arms score near the floor (1.70/2.70). The tasks require sustained
  multi-file investigation; current memories (facts, conventions) do not carry
  the *procedural* knowledge these tasks need. Wave 2 candidates from the
  research matrix: episodic memory type (worked examples of past
  investigations), relations expansion (bugfix ↔ causing decision), and
  turn-budget/tooling improvements orthogonal to memory.
- Arithmetic reality check: with gotcha+arch saturated, reaching uplift 4.53
  requires small-fix uplift ≈ +3.9 (from −1.00) — i.e. memory must make fix
  tasks *succeed* (score ~6) where both arms now fail. That is a Wave 2/3
  research problem (procedural memory), not a tuning problem.

**Tokens (6084 vs 3912, harness metric).**
- `static_hook_tokens` 2844 is now 47% of the composite and is dominated by
  the session-start hook's non-protocol output (status block, session
  bootstrap, import notices) — the protocol text itself is only ~316 tok of
  it. The hook's operational chrome is the next frontier.
- `static_skill_tokens` 1982 is a measurement artifact (on-demand in real
  sessions) — resolving the instrument-vs-reality question (measure a real
  session transcript, or move the metric to the real-session basis) should be
  a Wave 2 decision before more optimization is spent chasing it.
- Search/context slices are now capped by construction (index mode + budget +
  layered context); further cuts there risk starving retrieval.

## Incidents (this acceptance run)

- The overnight full L3 hit 25/60 judge failures (`claude` exit 1, empty
  stderr, all 3 retries; concentrated on gotcha/fix-004/fix-005 — the
  tool-temptation-shaped prompts). Manual reproduction of the same judge
  call succeeded the next morning: transient API-side failure under batch
  load, not the Task-9-era tool-use bug (the `--tools ""` hardening was
  active). Recovered via the checkpoint sidecar: 25 failed verdicts stripped,
  resume re-judged exactly those (35 complete / 25 re-judge / 0 to run) — no
  task re-runs, 0 failures on the second pass. The >20% warning fired as
  designed and the first-pass means were correctly treated as unreliable.
- `~/.engram/memorylake.json` was clobbered twice more by cmd/engram tests
  during the wave (user-accepted live-testing posture); restored from backup
  before each eval round per the standing ledger rule.

## Assumptions unchanged from baseline

`search-calls=3.0` per session (placeholder), tokenizer approx-bytes/4,
L1 corpus frozen at 54 QA cases, judge prompts/rubrics frozen, N=2.
L1 payload metric is a floor (envelope framing uncounted — see
`SearchPayloadTokens` doc comment).

## Recommendation

Ship Wave 1 (all rounds kept; both anchors materially improved; agent-facing
behavior strictly better). Open Wave 2 with two mandates: (a) procedural
memory for small-fix uplift — the only lever with headroom; (b) hook-chrome
slimming + L2 instrument decision — the only lever left for the token anchor.
