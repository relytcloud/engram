# MemoryLake server-side asks — evidence from Engram Phase-2 Wave 1

**Author:** Engram (client team)
**Date:** 2026-07-27
**Audience:** MemoryLake server team
**Status:** proposal — no server work committed, no client work blocked on it

Engram is a MemoryLake client: for projects enabled via `engram memorylake
enable`, every memory read and write goes through the V3 API (see
`internal/memorylake/writequeue.go`, `search.go`, `backend.go`). Over
2026-07-24…2026-07-27 we built a three-level memory eval harness and ran a
baseline plus four optimization rounds against the **live** MemoryLake project
`phoenix`. This document turns what that measurement exposed into three
concrete API asks, plus one section of client-side findings the server team
should know about.

Every number below is quoted from a committed scorecard in
`eval/results/`; the file is named at each citation so it can be re-checked.

## Program anchors these asks are measured against

| Anchor | Baseline (`2adc24c`, 2026-07-24) | Phase-2 target |
|---|---|---|
| **L3 memory uplift** (mean end-to-end score, memory arm − no-memory arm) | **2.267** (6.000 − 3.733) | **≥ 4.53** |
| **L2 injected tokens / session** | **7824** | **≤ 3912** |

Source: `eval/results/baseline-report.md`, `2adc24c-2026-07-24-l2.json`,
`2adc24c-2026-07-24-l3.json`.

Baseline L2 decomposition (tokenizer `approx-bytes/4`, `search-calls=3.0`):
SessionStart hook stdout 3202 (41%), memory SKILL.md 1587 (20%), MCP server
instructions 787 (10%), `mem_context` payload 697 (9%), retrieval 517 × 3 =
1551 (20%).

Where Wave 1 has taken those numbers so far, entirely with client-side work:
L2 composite `7824 → 6083.78` (`2289c68-2026-07-27-l2.json`), retrieval
`517.06 → 205.59` tokens/query (`2adc24c-2026-07-24-l1.json` →
`b4db321-2026-07-27-l1.json`). The three asks below target the parts of the
remaining gap that the client structurally **cannot** close, because the cost
is paid before our code sees the bytes.

---

## 1. Temporal validity on facts (`valid_from` / `valid_until`)

### 1a. Current client-side pain

The V3 fact model has no notion of *when a fact was true*. A fact is either
present (and fully ranked) or forgotten. There is no way to say "this was
correct until 2026-07-24" and have search reflect that.

The consequence is visible in our own corpus. When constructing the frozen L1
dataset we dumped the live `phoenix` project and found that of its 6 facts,
one was **a tombstone written as prose**:

> `[此条记忆已按用户要求于 2026-07-24 删除/作废...]`

— cited verbatim in `eval/datasets/README.md` (Sources §1), which records that
this fact "was excluded entirely as source material — it was never used to
write a case, and no case's keywords match it."

That is the ask in one artifact. A user's invalidation of a memory had to be
expressed as **fact text**, because the API offers no validity field. The
invalidation therefore:

- **still occupies a search slot** — it is embedded, scored, and eligible to
  rank ahead of a live fact;
- **still costs injected tokens** every time it is returned;
- **is invisible to the client as a signal** — Engram cannot filter or de-rank
  it, because "this fact is dead" is only legible to a human reading Chinese
  prose, not to `SearchFacts`.

The distractor metric quantifies the general cost of ranked-but-useless facts.
From `2289c68-2026-07-27-l1.json` / `b4db321-2026-07-27-l1.json`
(`avg_distractor_ratio` = 0.13271604938271603) over 54 cases:

| First-hit rank | Cases |
|---|---|
| 1 (clean top hit) | 37 |
| 2 | 5 |
| 3 | 1 |
| miss | 11 |

Of the 11 misses, **6 returned zero results** (the current `threshold` 0.1 in
`semanticSearchFacts` filtered everything) and **5 returned a payload that was
100% distractors** (`distractor_ratio` = 1.0). A further 6 cases shipped
partial distractors above the first hit (2 × 0.25, 2 × 0.333, 2 × 0.5). The
earlier round measured the same shape at `avg_distractor_ratio`
0.13888888888888887 (`0e9020d-2026-07-27-l1.json`). So roughly one query in
five is paying tokens for facts ranked above the answer — and today a
superseded fact is indistinguishable from a live one in that ranking.

This also gets worse with time, not better: the `phoenix` corpus grew 6 → 15
facts organically during Wave 1 (recorded in `.superpowers/sdd/progress.md`,
W1-T5), which by itself moved retrieval from 517.06 to 890.78 and then
991.46 tokens/query (`0e9020d-2026-07-27-l1.json`,
`9332c39-2026-07-27-l1.json`) before client-side index-mode rendering pulled it
back to 205.59. Superseded facts accumulate; without validity they accumulate
*in the ranking*.

### 1b. Proposed API change

**(i) `valid_from` / `valid_until` as first-class fact fields**, settable at
add time and by PATCH.

```http
POST /api/v3/workspaces/engram/projects/proj-52bfd150…/memories/facts
Content-Type: application/json

{
  "facts": [
    {
      "fact": "ZDB regression tests must scope FDB introspection UDFs by relfilenode.",
      "valid_from": "2026-07-24T03:38:15Z"
    }
  ]
}
```

Note this requires the batch element to accept an **object** as well as the
bare string it accepts today (`{"facts": ["text", …]}` —
`writequeue.go:AddFacts`). We ask that the string form keep working unchanged
(see §4 on why verbatim string add is load-bearing for us).

```http
PATCH /api/v3/workspaces/engram/projects/proj-52bfd150…/memories/facts/fact-4c762f9b5c83…

{ "valid_until": "2026-07-24T00:00:00Z" }
```

Response (the shape `patchFactMetadata` already consumes — the echoed fact,
now carrying validity):

```json
{
  "id": "fact-4c762f9b5c83…",
  "fact": "…",
  "metadata": { "pinned": true },
  "valid_from": "2026-07-20T11:02:00Z",
  "valid_until": "2026-07-24T00:00:00Z",
  "created_at": "2026-07-20T11:02:00Z",
  "updated_at": "2026-07-27T16:25:00Z"
}
```

**(ii) Search honors validity by default, with explicit opt-in for history.**

```http
POST /api/v3/workspaces/engram/memories/search

{
  "query": "how do I avoid flaky FDB introspection counts",
  "project_ids": ["proj-52bfd150…"],
  "memory_types": ["fact"],
  "top_k": 10,
  "threshold": 0.1,
  "validity": "current"          // "current" (default) | "all" | "as_of"
  // "as_of": "2026-07-01T00:00:00Z"  — with validity:"as_of"
}
```

Semantics we would rely on:

- `validity: "current"` — omit facts whose `valid_until` is in the past. This
  is the behavior we want as the default for every agent-facing read
  (`mem_search`, `mem_context`).
- `validity: "all"` — return them, each annotated (`"superseded": true`), so a
  human-facing surface (Engram's TUI/dashboard timeline) can still show
  history.
- `validity: "as_of"` — point-in-time reads. We do not need this for Wave 2;
  we list it so the field is not designed as a boolean that later has to grow.

A **de-ranking** variant (superseded facts returned but score-penalized) is
acceptable to us as a first increment and is strictly better than today, but
omission is what actually buys the token win.

### 1c. Expected effect on the anchors

- **Injected tokens (≤ 3912).** Direct and bounded: superseded facts stop
  consuming search slots and stop being rendered. On the current 54-case L1
  set the addressable slice is the distractor mass — `avg_distractor_ratio`
  0.133, with 5 cases shipping a 100%-distractor payload. At 205.59
  tokens/query and `search-calls=3.0` (≈617 tokens of the 6083.78 composite),
  eliminating the fully-wasted payloads is a small absolute win *today* on a
  15-fact corpus. Its real value is derivative-of-time: it removes the term
  that made the composite drift from 7824 to 9699.39 as the corpus grew
  (`9332c39-2026-07-27-l2.json`). Without validity, every optimization we ship
  is re-eaten by accumulation.
- **Uplift (≥ 4.53).** This is the more important axis. Baseline per-category
  uplift is gotcha **+8.50**, architecture-qa **−0.90**, small-fix **−0.80**
  (`baseline-report.md`): memory only wins where the retrieved fact is *right*,
  and actively hurts where the agent spends turns on material the repo already
  answers better. A stale-but-confident fact is the worst case for that
  arithmetic — it is the mechanism by which memory produces a *negative*
  category score. Validity filtering is the only way to make "the memory
  system will not hand the agent a fact the user already retired" a property
  of the system rather than a hope. We are not claiming a specific point
  gain: our current corpus has exactly one tombstone, so the effect is not
  yet separately measurable. We are claiming it removes an unbounded downside
  risk on the axis where our uplift target lives.

---

## 2. `min_score` (relevance threshold) on search

### 2a. Current client-side pain

`semanticSearchFacts` (`search.go:79`) sends `top_k` and `threshold`
(`defaultSearchTopK` 10, `defaultSearchThreshold` 0.1). What we observe is
that `top_k` governs the wire payload and the effective floor is low enough
that weak hits ship. The client then filters and re-renders — **after** paying
for the full payload on the wire and in our own process.

Evidence:

- Baseline: **517.0555555555555 tokens/query** average over 54 cases on a
  6-fact corpus (`2adc24c-2026-07-24-l1.json`) — a payload larger than the
  usable corpus, for a single question.
- As the corpus grew to 15 facts: **890.7777777777778** then
  **991.4629629629629** tokens/query (`0e9020d-2026-07-27-l1.json`,
  `9332c39-2026-07-27-l1.json`).
- After we shipped client-side index-mode rendering (W1-T8): **205.59259259259258**
  tokens/query (`b4db321-2026-07-27-l1.json`, `2289c68-2026-07-27-l1.json`).
  Round-over-round against `9332c39` that is **991.46 → 205.59, a 79%
  reduction, with retrieval quality bit-identical**: `recall@1/5/10`
  0.6851851851851852 / 0.7962962962962963 / 0.7962962962962963 and MRR
  0.7376543209876544 in both scorecards, `avg_distractor_ratio`
  0.13271604938271603 in both. (Measurement caveat, recorded in
  `.superpowers/sdd/progress.md` W1-T8: the token *basis* changed with that
  commit — the pre-index number was a JSON-marshal payload, the post-index
  number is index text plus the structured entries of shown hits. The recall
  and distractor figures are unaffected by the basis change.)

That last line is the argument. We cut what the *agent* sees by 79% and lost
nothing measurable — which means the bulk of the pre-index payload was never
load-bearing. But we could only cut the rendering. The bytes still cross the
network, are still
embedded-and-scored server-side, and still land in our heap: L1's own comment
records that it now measures the shown payload as a **floor**, with envelope
framing uncounted. A server-side `min_score` would cut the same waste one
layer earlier, where it is actually free.

The distractor distribution confirms the sub-threshold hits are real and
shippable: 5 of 54 cases (`2289c68-2026-07-27-l1.json`) return a payload in
which **every** result is a distractor (`distractor_ratio` 1.0). Those queries
have no answer in the corpus. The correct response is an empty result set —
which the server already produces for 6 other misses, so the mechanism exists;
it is the floor that is too low to be useful.

### 2b. Proposed API change

Add `min_score` to the search body: a hard post-ranking floor, applied
server-side, independent of `top_k`.

```http
POST /api/v3/workspaces/engram/memories/search

{
  "query": "why is the memory arm slower on small-fix tasks",
  "project_ids": ["proj-52bfd150…"],
  "memory_types": ["fact"],
  "top_k": 10,
  "min_score": 0.45,
  "actor_ids": ["actor-engram-cli"]
}
```

Response — same shape as today, plus the two fields that let a client tune
without guessing:

```json
{
  "facts": [
    { "id": "fact-ae290150a41d…", "fact": "…", "score": 0.71, "metadata": {} }
  ],
  "returned_count": 1,
  "filtered_by_min_score": 7
}
```

`filtered_by_min_score` matters: without it a client cannot distinguish "no
matching facts exist" from "your floor is too high", and will hedge by setting
the floor low — reproducing today's behavior. With it, Engram can calibrate
per project and report the calibration in its own scorecards.

Semantics we would rely on:

1. `min_score` is applied **after** ranking and **before** truncation to
   `top_k`, so raising the floor never silently drops a strong hit.
2. Returning fewer than `top_k` (including zero) is a normal, non-error
   response.
3. `min_score` and the existing `threshold` must have a documented
   relationship. Our reading of the current contract is that `threshold` is a
   *retrieval-stage* similarity cutoff; if `min_score` is simply its
   post-ranking counterpart, say so, and we will drop `threshold` from our
   request body.

### 2c. Expected effect on the anchors

- **Injected tokens (≤ 3912).** Retrieval is ≈617 of the current 6083.78
  composite (205.59 × 3 assumed search calls). A `min_score` that suppresses
  the 5 all-distractor payloads and trims the 6 partial-distractor cases would
  reduce that slice further, but the honest accounting is that **the client
  already captured most of the agent-visible retrieval win** (517.06 → 205.59).
  The remaining gap to 3912 is dominated by `static_hook_tokens` **2875**
  (`2289c68-2026-07-27-l2.json`), which is entirely ours to fix. So we are
  **not** asking for `min_score` primarily on token grounds; we are asking
  because it moves the cut server-side, which (a) removes the wire and
  server-embedding cost that our floor-metric does not even measure, and
  (b) keeps the win stable as corpora grow, which client-side rendering caps
  do not (see the 6 → 15 fact drift in §1a).
- **Uplift (≥ 4.53).** A hard floor converts "the agent got 10 weak facts" into
  "the agent got nothing and knows it." That is the behavior the baseline says
  we need: architecture-qa uplift is **−0.90** and small-fix **−0.80**
  precisely because the agent burns turns on memory material when the repo is
  the better source. An empty, fast, unambiguous "no relevant memory" is the
  cheapest possible correct answer on those categories, and it is the one
  result shape the client cannot manufacture without first paying for the
  wrong one.

---

## 3. Batch endpoints: batch forget, batch metadata PATCH

### 3a. Current client-side pain

Batch **add** exists — `POST .../memories/facts` takes `{"facts": [...]}` and
`AddFacts` uses it. Nothing else does. Every other mutation is
one-fact-per-round-trip:

- **Forget.** `MemoryLakeBackend.forgetFact` (`backend.go:944`) issues
  `POST .../memories/facts/{id}/forget` per fact. On 2026-07-24 we cleaned **28
  junk facts** that test runs had written into the live `engram` project
  (incident recorded in `eval/results/baseline-report.md` "Incidents &
  environment notes" and `.superpowers/sdd/progress.md`). That was **28
  sequential per-fact round-trips** — 28 chances to fail partway, no
  atomicity, and no way to express "forget this set" as one intent. There is no
  hard delete at all: `DeleteObservation` documents that `hard_delete=true`
  degrades to forget, so cleanup is *always* this loop.
- **Pin / metadata.** `setPinned` (`backend.go:300`) is a read-modify-write:
  `GET .../facts/{id}` then `PATCH .../facts/{id}` with the merged metadata —
  **two** round-trips per fact, because PATCH replaces `metadata` wholesale and
  the client must not clobber keys it does not own. W1-T11 pinned 2 phoenix
  gotcha facts (`fact-4c762f9b5c83…`, `fact-ae290150a41d…`) and that cost 4
  round-trips for 2 pins. Pinning a curated set of 20 facts for a project would
  cost 40.

Related, and cheap to fix in the same change: MemoryLake records **no pin
timestamp**. `factPinTime` documents the workaround — Engram orders the pinned
block by `updated_at` as a proxy, so any unrelated PATCH reshuffles the pinned
section. See the metadata note in §3b.

### 3b. Proposed API change

**(i) Batch forget.**

```http
POST /api/v3/workspaces/engram/projects/proj-2ae06986…/memories/facts/forget

{ "fact_ids": ["fact-a1…", "fact-b2…", "fact-c3…"] }
```

```json
{
  "forgotten": ["fact-a1…", "fact-b2…"],
  "failed": [ { "id": "fact-c3…", "code": "NOT_FOUND" } ]
}
```

Per-item outcomes rather than all-or-nothing: our cleanup loop is inherently
best-effort over ids that may have already been forgotten, and a single 404
must not abort the batch. (If the server prefers `DELETE` with a body, or a
`?ids=` query form, either is fine — the shape of the *response* is what we
depend on.)

**(ii) Batch metadata PATCH, with merge semantics.**

```http
PATCH /api/v3/workspaces/engram/projects/proj-52bfd150…/memories/facts

{
  "merge": true,
  "updates": [
    { "id": "fact-4c762f9b5c83…", "metadata": { "pinned": true } },
    { "id": "fact-ae290150a41d…", "metadata": { "pinned": true } }
  ]
}
```

```json
{
  "updated": [
    { "id": "fact-4c762f9b5c83…", "metadata": { "pinned": true, "pinned_at": "2026-07-27T16:25:00Z" } },
    { "id": "fact-ae290150a41d…", "metadata": { "pinned": true, "pinned_at": "2026-07-27T16:25:01Z" } }
  ],
  "failed": []
}
```

`merge: true` is the load-bearing half. It removes the GET from every
read-modify-write, taking pin from 2 round-trips to (amortized) a fraction of
one, and it eliminates a real lost-update race: today two concurrent metadata
writers each read-then-replace, and the loser's key vanishes. We would like
`merge: false` (today's replace) to remain available and to stay the default
for the existing single-fact PATCH, so nothing we already ship changes meaning.

**(iii) Optional, small:** stamp a server-side `pinned_at` (or expose a
generic `metadata_updated_at`) so pinned-order does not have to be inferred
from `updated_at`. This is a one-field change that retires a documented
client-side approximation.

### 3c. Expected effect on the anchors

We are explicit here: **batch endpoints move neither anchor directly.** They
are latency, correctness, and operability asks, and we would rather say so than
attach a fabricated token number to them.

- **Injected tokens (≤ 3912):** no direct effect. Indirect and real: the 28
  junk facts were *in a live project's ranking* until they were cleaned one at
  a time. Corpus hygiene is a token lever (a 6 → 15 fact growth cost us
  517.06 → 991.46 tokens/query), and hygiene that costs O(n) round-trips does
  not get done promptly. Cheap bulk forget is what makes "keep the corpus
  clean" an operation rather than a project.
- **Uplift (≥ 4.53):** no direct effect. Indirect: pinning is our main
  mechanism for guaranteeing that decisive gotcha facts are present, and the
  gotcha category is where the *entire* baseline uplift lives (**+8.50** vs
  0.00 for the no-memory arm; the R4 targeted gotcha round measured memory-arm
  mean **8.75** vs no-memory **7.75**, `w1-r4-l3subset.json`, N=1). Making
  curation cheap makes that mechanism usable at more than 2 facts at a time.
- **Latency, the honest headline:** L1 p50/p95 is 584/1165 ms
  (`2289c68-2026-07-27-l1.json`). Multiply a round-trip in that range by 28
  (cleanup) or by 2-per-pin (curation) and the cost of operating a MemoryLake
  project is dominated by round-trip count, not by work done.

---

## 4. Client-side findings for the server team's awareness

Not asks. Two properties of the current system that shaped our work and would
shape anyone else's.

**(a) mem0 async extraction lag constrains eval-dataset construction.**
`AppendObservation` writes a conversation *message*; MemoryLake's extraction
pipeline turns it into a fact asynchronously, out of band. Engram deliberately
does not poll or backfill — `mem_save` returns immediately after the append,
and the resulting fact is only observable later through normal read paths (see
`writequeue.go:AppendObservation`'s doc comment, and `backend.go:153`: "A
caller that needs the materialized fact … must re-find it later via Search
once mem0 has processed the message").

The eval consequence: **there is no point at which a test can assert "the thing
I just saved is now retrievable."** We could not *seed* a retrieval corpus and
then measure against it. The L1 dataset was instead mined from a frozen
read-only dump of whatever the live project already held — 6 facts, 5 usable —
and `eval/datasets/README.md` records the resulting caveat verbatim: "all 54
cases are mined from only 5 usable facts … Recall figures from this dataset
measure retrieval quality over a small, dense corpus, not a broad one." Every
recall number in this document (0.7592592592592593 / 0.7962962962962963 /
0.7962962962962963 at baseline) inherits that limitation, and the limitation is
downstream of extraction being unobservable rather than of anything we chose.

If the server ever exposes an extraction-completion signal — a
`GET .../messages/{id}/extraction` status, or a synchronous
`?wait_for_extraction=true` on message append — it would let clients build
seeded, reproducible retrieval fixtures. We are not requesting it in this
document; we are recording that its absence is why our corpus is 15 facts and
not 500.

**(b) The direct fact-add endpoint's verbatim semantics are load-bearing for
migration idempotency.** `POST .../memories/facts` (shipped 2026-07-23) is the
one write path that bypasses conversation-plus-extraction: each string becomes
a fact **as-is, synchronously**, and the response carries the real fact ids.
Engram depends on all four of those properties:

- **Verbatim** — an Engram observation is a `title` + `content` pair rendered
  to text (`factText`). If the server rewrote, summarized, split, or merged it,
  the migrated memory would no longer be the memory the user saved, and
  `mem_get_observation` would return something the user never wrote.
- **Synchronous, with real ids** — the returned fact id becomes the
  observation's `sync_id`. Every subsequent operation (get, update, pin,
  forget) addresses the fact by it.
- **Not server-deduplicated** — `AddFacts` documents that posting the same text
  twice creates two facts, and that callers needing idempotency must dedupe
  first, which `MigrateObservations` does. This is the right split: the client
  knows which of its local observations it has already migrated.

The ask embedded in this finding is a *stability* ask, not a change: if
extraction, normalization, or server-side dedupe were ever added to this
endpoint, migration would stop being idempotent and existing `sync_id`s could
be invalidated. Please treat the verbatim/synchronous/non-deduplicating
contract as a compatibility surface. In particular, the object-form batch
element proposed in §1b must be **additive** — the bare-string form
(`{"facts": ["text", …]}`) has to keep meaning exactly what it means today.

---

## Summary

| Ask | Anchor it serves | Directness |
|---|---|---|
| 1. `valid_from` / `valid_until` + validity-aware search | uplift (primary), tokens | Removes an unbounded correctness risk; the one tombstone in our corpus is a prose workaround for a missing field |
| 2. `min_score` on search | tokens (primary), uplift | Moves a cut the client already proved is safe (991.46 → 205.59 tokens/query, recall and MRR identical) to the layer where it is free and stays free as corpora grow |
| 3. Batch forget + batch merge-PATCH | neither directly | Round-trip count, atomicity, operability: 28 sequential forgets, 2 round-trips per pin |
| 4. (findings, not asks) extraction-lag observability; verbatim fact-add stability | — | Explains our 15-fact corpus ceiling; flags a compatibility surface |

Reproduce any figure here from `eval/results/` — `baseline-report.md` for the
anchors, `<sha>-<date>-l{1,2,3}.json` for per-round metrics,
`w1-r{2,3,4}-l3subset.json` for the targeted end-to-end rounds, and
`.superpowers/sdd/progress.md` for round-by-round decisions.
