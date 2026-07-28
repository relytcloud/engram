# Wave 1 Acceptance Report — Phase 2 Optimization Loop (CORRECTED)

**Date**: 2026-07-28 (supersedes the 2026-07-28 first version, which was
invalidated by an infrastructure artifact — see "The auth artifact" below)
**Branch**: `feat/phase2-wave1` (final L3 at binary version 0.4.0, slim active)
**Baseline**: `2adc24c` scorecards, corrected in place (same artifact)
**Spec**: `docs/superpowers/specs/2026-07-27-phase2-optimization-loop-design.md`

## The auth artifact (read this first)

The whole-branch review discovered that in BOTH the baseline L3 run and the
Wave-1 final L3 run, the same 12 no-memory tuples (all 10 gotcha runs +
fix-005 ×2) had died in ~1.7s with `OAuth session expired`, produced zero
output, and were scored 0. The baseline's headline "gotcha: memory 8.50 vs
no-memory 0.00" — the story the program's uplift target was anchored on —
was an artifact, contradicted by this branch's own targeted subset runs where
the no-memory arm scored 8–10 on the same tasks. Both sidecars were repaired
via checkpoint-resume (only the 12 broken tuples re-ran, per side; every
valid run was reused). All numbers below are from the corrected scorecards.

## Anchor verdicts (corrected)

| Anchor | Baseline (corrected) | Wave 1 (corrected) | Original target | Verdict |
|---|---|---|---|---|
| L3 memory uplift | **−0.367** (6.000 vs 6.367) | **−0.183** (6.967 vs 7.150) | ≥ 4.53 | **INVALID TARGET** — anchored on the artifact (2.267 was never real) |
| L2 injected tokens/session | 7824 | **6084** harness / **3916** real-session | ≤ 3912 | **NOT MET** on the harness metric (−22%); real-session view lands at the line (3916 vs 3912) |

**What is honestly true about effect:**
- Memory currently provides **no net e2e uplift on this task set** — in either
  version. Wave 1 moved uplift from −0.367 to −0.183 and the memory arm's
  absolute mean from 6.000 to 6.967 (+0.967, the largest defensible
  effect-side claim), while cutting the memory arm's output-token overhead
  from +48% to +9%.
- The ×2 uplift target is void: it was 2× an artifact. Wave 2 must re-anchor
  the effect goal on corrected data (and a redesigned task set — below).

**L2 note (both views):** the harness metric counts `SKILL.md` (2168 tok on
the final card) as a per-session static slice; in real Claude Code sessions
the skill loads on demand. Excluding it: hook 2875 + MCP instr 184 + context
240 + search 205.6×3 ≈ **3916** — within 4 tokens of the 3912 target. The
harness number (6084) remains the official verdict.

## Per-category L3, corrected (memory / no-memory / uplift)

| Category | Baseline | Wave 1 | Reading |
|---|---|---|---|
| architecture-qa | 8.00 / 8.90 / **−0.90** | 10.00 / 9.25 / **+0.75** | R2 answer-first genuinely cured the drag — this movement is real and survives correction |
| gotcha | 8.50 / 7.90 / **+0.60** | 9.20 / 8.90 / **+0.30** | the "+8.50" story was the artifact; the no-memory arm reads the same CLAUDE.md the tasks were mined from |
| small-fix | 1.50 / 2.30 / **−0.80** | 1.70 / 3.30 / **−1.60** | both arms improved; no-memory improved more — memory is still a drag here |

Memory-arm output tokens: 337k → 283k (−16%); overhead vs no-memory +48% → +9%.

## What Wave 1 actually proved

1. **Cost engineering works**: retrieval 517→206 tok/query, context 1636→240,
   instructions 787→184, composite −22% (−50% on the real-session view) — with
   recall@5 flat (0.796) and distractor ratio not worse (0.133).
2. **Answer-first works**: the one category where memory was measurably
   hurting for a behavioral reason (arch, −0.90) flipped to +0.75, and the
   fix-003-style "saved to memory instead of answering" failure disappeared.
3. **The e2e task set cannot demonstrate memory value as designed.** Gotcha
   tasks test knowledge that lives in CLAUDE.md — which both arms read. The
   uplift a memory system can add is bounded by how much task-relevant
   knowledge exists ONLY in memory. On this task set that bound is near zero.
   This is the single most important finding for Wave 2.

## Round ledger

| Round | Change | Gate result | Kept |
|---|---|---|---|
| R1 | slim protocol + trimmed instructions | recall flat; controlled slices −336 tok (−869 real-session) | ✓ |
| R2 | answer-first everywhere | targeted fix/arch memory 2.70→5.00; arch drag cured in final | ✓ |
| R3 | search index mode + layered context | 991→205.6 tok/query; context ≤400; retrieval not starved | ✓ |
| R4 | pinned core memory (1KB cap) | targeted gotcha memory 8.75 ≥ 8.5 | ✓ |

All rounds kept on their own gates (which measured the memory arm and cost —
both real); zero reverts. Corpus grew 6→15 facts organically during the wave.

## Wave 2 mandates (from corrected data)

1. **Re-anchor the effect goal.** Corrected baseline uplift is −0.367. A
   meaningful Wave-2 goal is "uplift > 0 with p<0.05 on a redesigned task
   set", not a multiple of a broken number.
2. **Redesign the e2e task set** so memory can matter: tasks whose critical
   knowledge exists only in memory (cross-session decisions, user
   preferences, investigation state never committed to the repo), with
   repo-knowledge tasks kept as a control group. The knowledge-isolation
   property must be verified at construction (no-memory arm dry run).
3. **Procedural memory for small-fix** remains the research lever (memory
   −1.60 there), but only after the task set can measure it.
4. **Hook chrome slimming** (2875 tok, 47% of composite) + the L2
   instrument decision (SKILL counting) close the token anchor.
5. **Harness guard**: treat CLI-error result_texts (auth failures) as run
   failures with retry, never scoreable zeros — the artifact class that
   invalidated the original numbers.

## Incidents (acceptance runs)

- 25/60 judge failures overnight (transient claude exit-1 storm) — recovered
  via checkpoint re-judge (0 failures on retry).
- The 12-tuple auth artifact above — recovered via checkpoint re-run of
  exactly those tuples on both sides.
- `~/.engram/memorylake.json` clobbered twice by cmd/engram tests
  (user-accepted posture); restored from backup before each eval round.

## Assumptions unchanged

`search-calls=3.0`, tokenizer approx-bytes/4, frozen L1 corpus (54 QA),
frozen judge prompts/rubrics, N=2, L1 payload metric is a floor.

## Recommendation

Ship Wave 1: every kept change is defensible on corrected data (cost −22%
harness / −50% real-session at flat retrieval quality; memory-arm e2e mean
+0.967; answer-first cured a real behavioral failure; verbosity overhead
+48%→+9%). Do NOT carry the old uplift narrative forward — Wave 2 starts
from uplift ≈ −0.2 and a task-set redesign.
