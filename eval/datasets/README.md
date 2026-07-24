# eval/datasets

Frozen ground-truth datasets for the eval suites (spec:
`docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`).

## phoenix-retrieval-v1.jsonl

- **Constructed:** 2026-07-24.
- **Target:** L1 retrieval evaluation (`eval/cmd/evalrun -suite l1`) against the
  **live MemoryLake backend**, project `phoenix` (proj_id resolved from
  `~/.engram/memorylake.json`, workspace/API key from `memorylake.LoadConfig()`).
- **Case count:** 54, ids `r-001`…`r-054`.
- **Categories:** `architecture` (20), `bugfix` (16), `decision` (10),
  `gotcha` (8).

### Sources

Built per spec §L1 from three raw-material sources, gathered read-only:

1. **Dumped MemoryLake facts** — `go run ./eval/cmd/evalrun -suite dump-facts
   -project phoenix`. The live `phoenix` project held 6 facts at construction
   time; 1 was an explicitly user-invalidated/deleted memory ("此条记忆已按
   用户要求于 2026-07-24 删除/作废...") and was excluded entirely as source
   material — it was never used to write a case, and no case's keywords match
   it. The remaining 5 usable facts (user memory-workflow preference, project
   identity/architecture, one detailed SP2 bug fix, one "patent point" design
   writeup, and one ZDB regression-test convention) are dense, multi-fact
   records, so most cases reverse-construct a *different sub-fact* out of the
   same record (e.g., the SP2 bug-fix fact yields separate cases for symptom,
   root cause, fix location, verification, and the open ORCA caveat).
2. **`git -C /workspace/phoenix log --oneline -100`** — used to phrase
   commit-hash/issue-number-precise questions (e.g. `7a39c8494b8`,
   `4f2b84033c5`, `90ca2fd462b`, `8b3ccf6a929`). Every commit referenced by a
   case also appears verbatim inside one of the 5 usable dumped facts above —
   git log supplied phrasing and cross-checking, not new unverified content.
3. **`/workspace/phoenix/CLAUDE.md`** — used to phrase architecture/gotcha
   questions (data-warehouse-vs-OLTP default workload assumption, ZDB
   regression-test flakiness conventions) that align with the same 5 dumped
   facts.

Approximate split by primary framing lens: ~50% straight fact
reverse-construction, ~25% git-history/commit-anchored, ~25%
CLAUDE.md-aligned architecture/gotcha framing. All three lenses converge on
the same 5 usable facts — there was no material in the live project beyond
those 5 records at construction time.

### Verification method

Every case's `expected_keywords` groups were checked programmatically against
the raw dumped fact text (`phoenix-facts.jsonl`, same AND-of-groups/OR-within-
group substring semantics as `eval/metrics.HitsKeywordGroups`) before being
included: a case is only added if at least one of the 5 usable facts contains
every keyword group. No case was included on the strength of git log or
CLAUDE.md content alone. A case whose keywords never appear in any usable
fact was excluded as a guaranteed miss.

### Freeze rule

This file is **frozen**: once merged, it is never edited to make scores look
better. If a case turns out to be wrong, ambiguous, or the underlying data
changes, fix forward — do not patch `phoenix-retrieval-v1.jsonl` in place.
Create a new versioned file (`phoenix-retrieval-v2.jsonl`) with the fix and
switch the runner default over deliberately, so historical scorecards
(`eval/results/*.json`) that reference `dataset: phoenix-retrieval-v1.jsonl`
stay reproducible against the exact file they were scored on.

### Read-only rule

Dataset construction only ever *reads* from MemoryLake (`dump-facts` calls
`ListAllFacts`, a pass-through of the existing read-only `listAllFacts`
listing). Nothing in this process writes, saves, updates, or deletes
MemoryLake facts for the `phoenix` project.
