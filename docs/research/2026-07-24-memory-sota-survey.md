# Memory SOTA Survey (Phase 0)

**Date**: 2026-07-24
**Purpose**: Phase 0 deliverable of
`docs/superpowers/specs/2026-07-24-memory-eval-optimization-design.md`. Four
parallel research agents swept general agent memory, coding-agent memory,
evaluation methodology, and cost-side techniques, mapping each finding onto
engram's Go implementation (`internal/mcp`, `internal/store`,
`internal/memorylake`) and the MemoryLake cloud backend. This document
synthesizes those four reports into one prioritized reading, and its §5
priority matrix feeds the Phase 2 optimization loop directly.

**Program goals** (from the design spec):

1. **Effect ×2** — double the *memory uplift* (`mean e2e task score with
   memory − mean e2e task score without memory`) on phoenix e2e tasks.
2. **Cost ÷2** — cut the token overhead injected by the memory system per
   session (static protocol text + `mem_context` payload + `mem_search`
   payloads × avg calls/session) to ≤ 50% of the current baseline, with no
   effect regression.

Every technique below is presented as *Source / What it does / Mapping to
engram+MemoryLake / Expected impact / Difficulty*. Where two directions
covered the same underlying technique, the fuller write-up carries both
mappings and the other location cross-references it; §5 collapses these into
a single priority-matrix row.

---

## 1 General agent memory

Direction 1: MemGPT/Letta, Mem0, Zep/Graphiti, A-Mem, LangMem — memory
organization, retrieval, forgetting/merging strategies.

### §1 MemGPT/Letta tiered memory (core / recall / archival)

**Source**: https://arxiv.org/abs/2310.08560 (MemGPT paper);
https://docs.letta.com/letta-agent/memory; also covered from the cost angle
at https://github.com/letta-ai/letta (see merged note below).

**What it does**: Letta (formerly MemGPT) manages memory across three tiers
modeled on OS virtual memory: Core Memory is a small, always-in-context block
the agent reads/writes directly; Recall Memory is full conversation history
searchable outside the context window; Archival Memory is a Postgres+pgvector
store queried via explicit tool calls for unbounded long-term storage. The
LLM itself decides when to page information between tiers using function
calls (`core_memory_append`, `archival_memory_insert/search`), acting as its
own memory controller rather than relying on a fixed retrieval policy.

**Mapping to engram+MemoryLake**: Engram already has an analogous two-tier
split (`mem_context` = recent + pinned "hot" memory vs. `mem_search` =
full-text/semantic "cold" recall), but there is no explicit **core memory
block** — a small, always-injected, agent-editable scratchpad distinct from
the ~10.3KB SessionStart protocol injection. The cheapest path to this is not
a new table: `mem_pin`/`mem_unpin` already exist for marking importance.
Render pinned observations as a small, always-present "core memory" section
in the static SessionStart protocol block (ahead of recent observations)
rather than requiring a search hit to surface them, capped at a hard token
budget (e.g. 500 tokens–2KB) enforced at pin time so this section can't
silently grow into a second full protocol block. This gives agents an
OS-style "RAM" tier for volatile facts (current task, must-not-forget
gotchas) that doesn't need a full `mem_save` round trip and doesn't compete
with FTS5/vector relevance scoring.

**Expected impact**: Injected-token cost rises by the core-block size
(bounded, ~500B–2KB) but falls elsewhere because trivial "what am I doing
right now" / "never use --no-verify"-style facts stop being written as full
observations that later pollute `mem_search` results. Task uplift is
strongest for the gotcha-reproduction task category — persistent
must-not-forget facts get 100% recall instead of depending on search
relevance ranking, addressing "agent forgot the immediate goal/constraint
mid-session" failures that `mem_context`'s recency window doesn't reliably
catch. A capped, rarely-changing core block also reinforces prompt-cache
stability (§4 prompt-cache-friendly separation), since pinned content changes
far less often than the full `mem_context` payload.

**Difficulty**: M (`mem_pin` already exists; needs SessionStart rendering + a
size cap + docs update)

---

### §1 Letta memory blocks (labeled, shared, agent-editable)

**Source**: https://www.letta.com/blog/memory-blocks/

**What it does**: Memory is decomposed into discrete labeled blocks (e.g.
`human`, `persona`, `knowledge`), each a plain string with a character-limit
budget, editable in place via tools like `rethink_memory()` rather than
reconstructing a flat context window. Blocks are first-class objects with
their own IDs, so multiple agents (e.g. a primary agent and a background
sleep-time agent) can share and concurrently edit the same block.

**Mapping to engram+MemoryLake**: Engram's `mem_save` treats every write as
an append-only observation row (with `topic_key` giving upsert-in-place for
one row at a time via `internal/store/store.go`'s dedupe/topic-key logic).
There's no notion of a *labeled, budget-capped* section of context that
different tools update in a coordinated way. Extend the SessionStart
protocol payload (`internal/setup/protocol.go`) to be composed of named,
size-budgeted sections (`architecture`, `active-work`, `preferences`) sourced
from `topic_key` prefixes already in use (e.g. `architecture/*`), each
truncated independently instead of the current single flat top-N-by-relevance
dump. This turns the informal `topic_key` convention documented in CLAUDE.md
into an enforced, budgeted schema. Coding-agent memory (§2) independently
converges on the same shape from two other directions — Claude Code's Auto
Memory index budget and Windsurf's per-scope character caps — see §5 for the
merged row.

**Expected impact**: Primarily an injected-token-cost control: today a single
noisy high-relevance-score observation in one topic area can crowd out an
entire other topic category from the ~10.3KB budget; per-section budgets
guarantee proportional representation. Modest task-uplift from more
predictable context composition across sessions.

**Difficulty**: M

---

### §1 Letta sleep-time compute (background memory reflection)

**Source**: https://www.letta.com/blog/sleep-time-compute/;
https://docs.letta.com/guides/agents/architectures/sleeptime/

**What it does**: A background "sleep-time" agent shares memory blocks with
the primary agent and asynchronously reviews recent conversation history
during idle periods, rewriting and consolidating memory state without
blocking the foreground conversation. This turns memory maintenance into a
non-blocking, proactively-scheduled process rather than something the
foreground agent must pause to do inline.

**Mapping to engram+MemoryLake**: Engram already has the raw material for
this (`mem_session_summary`, `internal/cloud/autosync`'s background push/pull
orchestration) but no background *memory-quality* pass — nothing currently
re-reviews old observations for staleness/redundancy outside of `mem_review`'s
manual recency-decay listing. Add a background worker in
`internal/cloud/autosync` (or a new `internal/memorylake` job, since
MemoryLake already does async fact extraction) that periodically runs
`mem_review`-eligible observations through an LLM merge/prune pass and writes
results back via the existing `topic_key` upsert path — reusing the
`judgment_required`/`mem_judge` machinery already built for conflict
surfacing (`internal/mcp/mcp.go`) instead of inventing a new consolidation
protocol. Two other directions independently propose the same category of
change: Anthropic's Managed Agents "Dreaming" (§2) and RAPTOR-style periodic
topic rollups (§4) — see §5 for the merged row spanning all three.

**Expected impact**: Reduces injected-token cost over long project lifetimes
by shrinking the pool of near-duplicate observations that
`mem_search`/`mem_context` must rank against; task uplift is indirect (better
signal-to-noise in retrieved memories) rather than immediate. Cost: needs an
LLM call per background pass, and careful scoping to avoid silently editing
memories a user is mid-review on.

**Difficulty**: L

---

### §1 Letta Context Repositories (git-based memory for coding agents)

**Source**: https://www.letta.com/blog/context-repositories/

**What it does**: Memory is organized as a versioned filesystem of files with
frontmatter metadata rather than a database of opaque blobs; a `system/`
directory holds files always loaded into context, and every memory change is
an automatic git commit with an informative message. This is aimed
specifically at coding agents and multi-agent setups: subagents can edit
memory concurrently in isolated git worktrees and merge, and agents get
progressive disclosure by reorganizing/pinning files using ordinary bash/git
tooling they already know.

**Mapping to engram+MemoryLake**: This is the closest primary-source analog
to engram's own identity as "memory for coding agents," and engram's design
explicitly favors SQLite as source-of-truth with git-friendly compressed
chunks for sync (`internal/sync`) — conceptually adjacent but
observation-row-based, not file-based. A concrete, scoped change: extend
`internal/sync` (already git-friendly chunk format) to optionally
materialize each project's `architecture/*`-scoped `topic_key` observations
as human-readable Markdown files under `.engram/memory/<topic_key>.md` in the
project repo itself (opt-in, mirroring the existing sync chunks), so a coding
agent (or a human reviewing a PR) can `git diff`/`git blame` memory changes
the same way it reviews code changes — without abandoning SQLite as the
operational source of truth `mem_save`/`mem_search` hit.

**Expected impact**: No injected-token change (this is a review/audit
surface, not a retrieval path) but meaningful task-uplift for a specific
failure mode: silent memory corruption or drift is currently invisible
without querying the DB directly; a git-diffable mirror makes bad `mem_save`s
visible in normal code review, catching them before they poison future
context.

**Difficulty**: M

---

### §1 Mem0's ADD/UPDATE/DELETE/NOOP extraction pipeline

**Source**: https://arxiv.org/abs/2504.19413 ("Mem0: Building
Production-Ready AI Agents with Scalable Long-Term Memory")

**What it does**: Mem0 runs conversations through a two-stage pipeline: an
LLM extracts candidate facts, then retrieves semantically similar existing
memories and asks the LLM to choose one of four operations per candidate —
ADD (novel fact), UPDATE (merge with an existing memory), DELETE (existing
memory is contradicted), or NOOP (redundant, ignore) — replacing hand-written
similarity thresholds with an LLM judgment call. The paper reports ~91% lower
p95 latency and >90% token savings versus feeding raw history back into
context every turn.

**Mapping to engram+MemoryLake**: This is almost exactly what engram's SQLite
path already does with `topic_key` upsert + dedupe-by-hash, and what
MemoryLake's async mem0 extraction path does for enabled projects — except
engram's own local `judgment_required`/`mem_judge`/`mem_compare` conflict
loop only offers relation verbs (e.g. SUPERSEDES) for the agent to apply, not
a full ADD/UPDATE/DELETE/NOOP decision in one LLM turn. Concretely: collapse
the current two-round-trip flow (`mem_save` → `judgment_required` →
`mem_judge` per candidate) into a single `mem_save` response that includes
Mem0-style suggested operations per candidate (`add`/`update`/`delete`/`noop`)
computed server-side from embedding similarity, so the agent only needs to
call `mem_judge` to *override* a suggestion rather than blindly classify from
scratch every time.

**Expected impact**: Reduces round-trips (fewer tool calls = fewer injected
tokens per save) and reduces the chance an agent skips the judgment loop
entirely (a known failure mode implied by the protocol's explicit "do NOT use
the top-level judgment_id for multiple candidates" warning in
`internal/mcp/mcp.go`). Task uplift is moderate — mainly fewer accidental
duplicate observations on the SQLite backend, which today has no equivalent
to Mem0's automatic DELETE.

**Difficulty**: M

---

### §1 Mem0g graph memory (entity-relationship extraction)

**Source**: https://arxiv.org/abs/2504.19413 (Mem0 paper, Mem0g variant);
https://docs.mem0.ai

**What it does**: Mem0g augments the base vector/relational Mem0 store with a
graph layer that extracts entities and relationships from conversation and
stores them as graph edges alongside the flat memory store, enabling
relationship-aware queries ("who reports to whom") that pure vector
similarity search misses.

**Mapping to engram+MemoryLake**: Engram has a `mem_judge` relation mechanism
(SUPERSEDES-style verbs between two observations) but no general entity
graph — `mem_search`/`mem_context` are pure FTS5/BM25 (SQLite) or pure vector
search (MemoryLake — "pure semantic search, no local keyword/fuzzy
augmentation" per DOCS.md). A scoped MemoryLake-server-side proposal: since
MemoryLake already runs an async mem0 extraction pipeline for enabled
projects, expose Mem0g's entity/relationship layer (if the underlying
MemoryLake tenant supports it) through a new read-only `mem_related` MCP tool
that, given an observation's sync_id, returns graph-neighbor facts — reusing
MemoryLake's existing extraction rather than building entity extraction into
engram's Go code.

**Expected impact**: Task uplift for a narrow but real class of coding-agent
questions ("what else touches this config value," "who/what depends on this
function") that keyword/vector search alone answers poorly since they
require multi-hop reasoning, not textual similarity. No injected-token cost
by default (opt-in tool call), but risks a large payload if the neighbor
fan-out isn't capped.

**Difficulty**: L (depends entirely on MemoryLake's server-side graph
capability existing — must be verified with MemoryLake, not assumed)

---

### §1 Zep/Graphiti bi-temporal knowledge graph with edge invalidation

**Source**: https://arxiv.org/abs/2501.13956 ("Zep: A Temporal Knowledge
Graph Architecture for Agent Memory"); https://blog.getzep.com/beyond-static-knowledge-graphs/;
also covered from the read-time-cost angle at
https://neo4j.com/blog/developer/graphiti-knowledge-graph-memory/ (see
merged note below).

**What it does**: Graphiti (Zep's open-source engine) represents memory as
node-edge-node triplets where every edge carries four timestamps —
`created_at`/`expired_at` (when the system learned/un-learned it) and
`valid_at`/`invalid_at` (when it was true in the world) — so both "when did
we find out" and "when was it actually true" are tracked separately. When a
new fact contradicts an existing edge, Graphiti uses an LLM to detect the
conflict and marks the old edge's `invalid_at` rather than deleting it,
regenerating its text (e.g. "works as" → "used to work as, until..."),
preserving full history non-destructively. Zep reports 94.8% vs. 93.4% on the
Deep Memory Retrieval benchmark and up to 18.5% accuracy gains with 90% lower
latency on LongMemEval versus naive full-context baselines. A second framing
of the same system emphasizes that Zep builds this graph at *write* time so
that read time serves token-efficient, prompt-ready context directly from
the graph rather than running an LLM summarization pass per query.

**Mapping to engram+MemoryLake**: Engram's soft-delete (`mem_delete`,
`deleted_at IS NULL` filtering) is a coarser, single-timestamp analog — a
memory is either live or soft-deleted, with no notion of "this was true from
date X to date Y" independent of "when we learned/unlearned it." The closest
existing hook is `mem_judge`'s SUPERSEDES relation, which records *that* one
observation replaces another but not a validity interval. Concretely: add two
nullable columns to the `store.go` schema, `valid_from`/`valid_until`
(world-time, agent-settable at `mem_save`/`mem_update` time, separate from
the existing `created_at` row timestamp), and have `mem_search`/`mem_context`
default to filtering `valid_until IS NULL OR valid_until > now()`,
de-ranking or omitting superseded facts instead of returning both old and
new. On the MemoryLake side this is mostly a server-side proposal: request
that MemoryLake tag facts with valid-until/superseded-by when a topic_key or
clear contradiction resolves, with the engram-side de-rank/omit rendering
being a small, separate change once the field exists. This is a genuinely new
capability, not a repackaging of soft-delete — it lets "config X used to be
Y, now it's Z" be represented as one auditable timeline instead of two
disconnected observations plus a manual SUPERSEDES judgment call.

**Expected impact**: Meaningful task-uplift for exactly the failure mode
Zep's benchmarks target — stale facts silently outranking their replacements
in retrieval because nothing marks the old one invalid — and a real cost
reduction proportional to how often topics get revised, since a superseded
fact currently wastes injected tokens on stale content the agent then has to
reason its way past. Injected-token cost is otherwise roughly flat for
never-revised facts (same number of rows surfaced, just correctly filtered),
though building the full bi-temporal query surface is nontrivial; the
MemoryLake-side tagging is the harder L piece, engram-side filtering is S
once the field exists.

**Difficulty**: L

---

### §1 A-Mem Zettelkasten-style dynamic note linking

**Source**: https://arxiv.org/abs/2502.12110 ("A-Mem: Agentic Memory for LLM
Agents", NeurIPS 2025); https://github.com/agiresearch/A-mem

**What it does**: Each new memory becomes a "note" with a contextual
description, keywords, tags, and an embedding; A-Mem retrieves the k nearest
existing notes by embedding similarity and then uses an LLM (not just
similarity score) to decide whether to create an explicit link between the
new note and each candidate, capturing causal/conceptual relationships that
pure vector distance misses. Retrieval automatically pulls in linked
neighbors alongside the direct hit ("memories in the same box"), and the
system supports arbitrary many-to-many links rather than a fixed taxonomy.

**Mapping to engram+MemoryLake**: This is structurally close to a
generalization of engram's `mem_judge` relation mechanism (today narrowly
scoped to conflict-candidates surfaced right after a `mem_save`) into a
standing, queryable link graph. Concretely: persist every `mem_judge`
relation verb (not just SUPERSEDES but also new verbs like
RELATES_TO/EXTENDS) as rows in a `relations` table keyed by both observation
sync_ids, and have `mem_search` optionally expand each hit with its 1-hop
linked neighbors (bounded, e.g. top 2) before returning results — i.e. make
the currently one-shot, save-time-only linking mechanism persistent and
retrieval-time-active instead of write-time-only.

**Expected impact**: Task uplift from surfacing related-but-not-textually-
similar memories (e.g. a bugfix note and the architecture decision that
caused the bug) that keyword/BM25 or plain embedding search under-ranks;
injected-token cost rises modestly per search call (bounded neighbor
expansion) but is controllable via the expansion cap.

**Difficulty**: M

---

### §1 LangMem's semantic/episodic/procedural memory typing

**Source**: https://langchain-ai.github.io/langmem/concepts/conceptual_guide/;
https://github.com/langchain-ai/langmem

**What it does**: LangMem explicitly separates three memory kinds: semantic
memory (facts/preferences, stored either as an evolving "profile" object or
an append-only "collection"), episodic memory (full successful-interaction
traces kept as worked examples, not just facts), and procedural memory (the
agent's own operating instructions/behavior, refined over time rather than
fixed at prompt-authoring time). Each type gets different consolidation
logic: profiles are updated in place, collections grow without loss, episodes
are stored as complete narratives for few-shot guidance.

**Mapping to engram+MemoryLake**: Engram already has an informal type system
(`engram_type`/`engram_scope` metadata, plus the CLAUDE.md-documented
categories "decisions, bugs, discoveries, conventions") but no distinct
handling for *procedural* memory — nothing lets an agent's own successful
workflow ("ran X then Y to fix Z-class bugs") get retrieved specifically as a
worked episode rather than as one more fact competing in the same relevance
ranking as everything else. Add a `type=episode` value to the existing
`mem_save` type enum, and have `mem_search` optionally boost/segregate
episode-type results into their own labeled section of the response (distinct
from fact-type results), mirroring how LangMem's episodic memory is retrieved
and injected as a "worked example" rather than blended into general context.

**Expected impact**: Task uplift for repeated procedural tasks (recurring
build/test/debug workflows specific to this Phoenix-style monorepo, e.g. the
CLAUDE.md-documented submodule-sync-then-rebuild sequence) where a full
worked trace is more useful than a one-line fact. Injected-token cost is
roughly neutral if episodes replace rather than add to what's already being
saved as unstructured "discovery" observations today.

**Difficulty**: S — it's a metadata/query-shape change, not a new storage
engine

---

### §1 LangMem hot-path vs. background-consolidation memory formation

**Source**: https://langchain-ai.github.io/langmem/hot_path_quickstart/;
https://www.langchain.com/blog/langmem-sdk-launch

**What it does**: LangMem gives agents two distinct memory-write paths:
hot-path tools the agent calls inline during a live conversation to
create/update/delete memories immediately (higher latency cost, immediate
effect), and a background Memory Manager that runs after a conversation ends
or during idle time, extracting/consolidating/deleting memories in batches
from full transcripts — recommended as an add-on only once hot-path alone
isn't enough for latency or memory-quality reasons.

**Mapping to engram+MemoryLake**: Engram's current model is hot-path-only —
every save is an explicit, synchronous `mem_save` call the agent makes
mid-session (MemoryLake's async mem0 extraction is a backend-internal
detail, not an agent-visible background-consolidation stage engram itself
orchestrates). The `memory-protocol` skill's proactive-save-rule assumes the
agent remembers to call `mem_save` at the right moments, which is a real,
documented failure surface (agents forget or defer saves). Concretely: add a
session-end batch pass — trigger it from the existing
`mem_session_end`/`mem_session_summary` hook — that re-scans the just-ended
session's tool-call transcript (already available to the harness) for
save-worthy content the agent didn't explicitly `mem_save`, and writes those
as lower-confidence observations tagged distinctly (e.g.
`source=background_extraction`) so they're visibly different from
agent-asserted memories. This is the same category of change as Letta's
sleep-time compute above and Anthropic Managed Agents' "Dreaming" (§2) — see
§5 for the merged row.

**Expected impact**: Directly targets the biggest practical gap between
engram's current protocol (proactive, agent-discretion saves) and what
LangMem's data suggests: background extraction catches memories a distracted
or compaction-truncated agent would otherwise drop entirely. Injected-token
cost is flat at write time (batch happens post-session) and only appears
later, gated by however these lower-confidence rows are ranked in
`mem_search`/`mem_context` — they should rank below explicit saves by default
to avoid drowning high-signal, agent-curated memories in noise.

**Difficulty**: M

---

## 2 Coding-agent memory

Direction 2: Claude Code native memory & CLAUDE.md practice, Cursor Memories,
Devin Knowledge, Windsurf Cascade Memories/Rules.

### §2 Hierarchical CLAUDE.md scoping (managed → user → project → local) with directory-tree walk

**Source**: https://code.claude.com/docs/en/memory

**What it does**: Claude Code resolves persistent instructions from four
fixed scopes — org-managed policy file, user `~/.claude/CLAUDE.md`, project
`./CLAUDE.md`/`./.claude/CLAUDE.md`, and a gitignored `CLAUDE.local.md` —
concatenating them in that precedence order at every session start. It walks
the directory tree from the working directory upward, loading ancestor
CLAUDE.md/CLAUDE.local.md files in full at launch, while CLAUDE.md files in
*subdirectories* below cwd load lazily, only when Claude reads a file in that
subdirectory. `claudeMdExcludes` lets a user skip specific ancestor files
(e.g. another team's rules in a monorepo).

**Mapping to engram+MemoryLake**: engram's SessionStart injection is one flat
~10.3KB blob regardless of scope. Introduce a `local` scope value in
`internal/store`/`mem_save` alongside existing `project`/`scope`/`topic_key`,
and have the protocol builder in `internal/mcp` concatenate org-tenant
defaults (MemoryLake) → user (`~/.engram/protocol.md`) → project (existing
DB) → local (gitignored file) in fixed order, deferring subdirectory-scoped
facts to `mem_context`/`mem_search` on demand instead of always injecting
them.

**Expected impact**: Lowers fixed injected-token cost on large monorepos
(only root-level facts always load; subtree facts load on demand), likely
uplift-neutral-to-positive since precision goes up without losing coverage.

**Difficulty**: M

---

### §2 Auto Memory: MEMORY.md index + on-demand topic files, hard 200-line/25KB budget

**Source**: https://code.claude.com/docs/en/memory

**What it does**: Claude Code writes its own learnings (build commands,
debugging insights, preferences) to
`~/.claude/projects/<project>/memory/MEMORY.md` plus optional topic files
(`debugging.md`, `api-conventions.md`, ...). Only the first 200 lines or 25KB
of `MEMORY.md` load at session start — topic files load lazily via normal
file reads when relevant — and Claude Code actively measures the index after
every write, warning (and, past the limit, erroring) Claude to compress it
into one line per entry and push detail into topic files.

**Mapping to engram+MemoryLake**: engram already separates raw facts
(SQLite/MemoryLake) from the injected protocol, but the protocol itself isn't
self-monitoring. Add a token/line budget check inside
`mem_session_summary`/`mem_save` write paths (`internal/store`) that measures
the would-be `mem_context` injection size and returns a soft-warning (and
eventually a write-refusal) telling the agent to move detail into a
linked topic-keyed fact rather than growing one giant entry — effectively
porting Claude Code's self-enforced index budget onto engram's SessionStart
injection. This is the same underlying "cap the always-loaded section, defer
detail on demand" idea as Letta's labeled memory blocks (§1) and Windsurf's
tiered character budgets below — see §5 for the merged row.

**Expected impact**: Bounds injected-token growth over a project's lifetime
(currently engram's ~10.3KB protocol can only grow); uplift preserved because
detail is still retrievable via `mem_search`/`mem_get_observation`, just not
force-loaded.

**Difficulty**: S–M

---

### §2 `.claude/rules/` path-scoped conditional loading (glob-triggered rules)

**Source**: https://code.claude.com/docs/en/memory

**What it does**: Rule files under `.claude/rules/` carry YAML frontmatter
with a `paths` glob list; a rule only enters context when Claude reads a file
matching that glob, rather than being injected at every session start. This
lets large repos keep hundreds of lines of frontend/backend/security-specific
guidance out of the fixed startup cost, paying the token cost only when the
matching subsystem is actually touched.

**Mapping to engram+MemoryLake**: engram's `topic_key` already namespaces
facts (`architecture/auth-model`) but nothing ties a fact to a file glob.
Extend the fact schema with an optional `path_glob` column and have the MCP
server watch tool-call file paths (already visible to `internal/mcp`) to
trigger a targeted `mem_search`/injection only when a matching path is
opened, instead of relying solely on the agent to proactively call
`mem_search`.

**Expected impact**: Meaningful injected-token reduction for path-irrelevant
facts (a Rust-only project never sees Python conventions); uplift improves
for on-topic facts because retrieval becomes push-based instead of depending
on the agent remembering to search.

**Difficulty**: M–L

---

### §2 `@path` imports with an external-import trust/approval dialog

**Source**: https://code.claude.com/docs/en/memory

**What it does**: CLAUDE.md can `@path/to/file` import other files
(recursively, max 4 hops), which get expanded and loaded at launch. If a
project-level CLAUDE.md imports a path that resolves *outside* the working
directory (e.g. `@~/.claude/my-project-instructions.md`), Claude Code shows a
one-time approval dialog listing the external files before trusting them —
protecting against a teammate's committed CLAUDE.md silently pulling in
arbitrary files from your machine.

**Mapping to engram+MemoryLake**: MemoryLake-backed projects auto-merge/
auto-resolve facts server-side with no local approval step (per the plugin's
own instructions, `mem_judge`/`mem_compare` are skipped there). Borrow the
trust-boundary idea narrowly: when a shared/team-authored fact (topic_key
owned by someone else, or synced from a different project) would be injected
into *your* SessionStart protocol for the first time, surface a one-time
confirmation via `mem_review` rather than silently trusting it — closing the
same "committed file silently changes my agent's behavior" gap Claude Code
addresses for imports.

**Expected impact**: Small negative on convenience/friction, but reduces risk
of a teammate's bad or malicious topic-keyed fact silently steering the
agent; no direct token-cost effect.

**Difficulty**: S

---

### §2 Managed Agents "Dreaming" — scheduled background session review, pattern extraction, memory curation

**Source**: https://claude.com/blog/new-in-claude-managed-agents (announced
at Code with Claude, May 2026; also
https://thenewstack.io/anthropic-managed-agents-dreaming-outcomes/)

**What it does**: Dreaming is a scheduled background process (research
preview) that reviews *past agent sessions and the existing memory store*,
extracts patterns a single session can't see on its own (recurring mistakes,
workflows agents converge on, team-shared preferences), and rewrites/curates
memory so it "stays high-signal as it evolves" — either applying updates
automatically or queuing them for human review. Anthropic frames it as a
second stage on top of ordinary in-session memory writes: memory captures
learnings as the agent works, dreaming consolidates across sessions and
across agents afterward.

**Mapping to engram+MemoryLake**: This is the most direct hit for MemoryLake
specifically — MemoryLake already does server-side extraction into facts;
add a scheduled consolidation job (cron-style, in `internal/cloud/autosync`
or a new `internal/cloud/dream` package) that periodically re-reads a
project's accumulated facts, dedupes/merges near-duplicate topic_keys,
promotes recurring corrections into higher-confidence facts, and
demotes/archives stale ones — surfaced through a new `mem_review`-style diff
that the user (or an auto-apply GUC) approves, mirroring Anthropic's
"auto-update vs. review" toggle. This is the same category of change as
Letta's sleep-time compute and LangMem's hot-path/background split (both
§1) and RAPTOR's topic rollups (§4) — see §5 for the merged row spanning all
four.

**Expected impact**: Primarily a quality/precision lift on long-lived
projects (stale or contradictory facts get pruned instead of accumulating
forever, which should raise task uplift from `mem_context`/`mem_search`)
with a secondary token-cost benefit since consolidated facts are fewer and
denser than raw accumulated ones.

**Difficulty**: L

---

### §2 Managed Agents "memory" (public beta) paired with "outcomes" (reinforcement-style feedback signal)

**Source**: https://claude.com/blog/new-in-claude-managed-agents ;
https://9to5mac.com/2026/05/07/anthropic-updates-claude-managed-agents-with-three-new-features/

**What it does**: Alongside dreaming, Anthropic shipped "memory" (in-session
learning capture) and "outcomes" as public-beta primitives for the Managed
Agents platform — outcomes let a caller attach a success/failure signal to a
completed agent run, which then feeds back into what dreaming considers worth
promoting into durable memory. Cognition/Harvey reported large
task-completion gains attributable to this feedback loop.

**Mapping to engram+MemoryLake**: engram has no concept of "did this saved
fact actually help." Add an optional `outcome` field settable via a new
lightweight tool (e.g. `mem_feedback session_id=... outcome=success|failure`)
that MemoryLake's fact-extraction pipeline (and the future
dreaming-style consolidation job above) can weight by — facts cited in
sessions marked `success` get reinforced/kept verbatim; facts present in
`failure` sessions get flagged for review rather than silently persisting.

**Expected impact**: Positive on uplift over time (the system learns which
facts are actually load-bearing vs. noise); negligible token cost since
outcome is a single small write, not something injected into context.

**Difficulty**: M

---

### §2 Subagent-scoped auto memory vs. fork-inherited memory

**Source**: https://code.claude.com/docs/en/memory (see "Subagent memory" /
`#enable-persistent-memory`)

**What it does**: A subagent's main-conversation auto memory is *not*
inherited by default — subagents start clean unless launched via "fork"
(which inherits the parent conversation and system prompt wholesale), or
unless the subagent definition explicitly sets a `memory` field, which then
gives it its own separate memory directory rather than sharing the parent's.

**Mapping to engram+MemoryLake**: engram has no notion of sub-agent
isolation today — all `mem_*` calls from any agent instance in a project
write/read the same project-scoped store. When engram is used from within a
fanned-out Task/Agent-tool workflow, add an opt-in "ephemeral scope" to
`mem_save`/`mem_context` (e.g. `scope=subagent:<task-id>`) so
exploratory/throwaway subagent findings don't pollute the shared project
memory, while an explicit `mem_save --promote` call can lift a subagent's
finding into the shared project scope.

**Expected impact**: Reduces noise/token cost in the shared project's
`mem_context` (fewer one-off, task-specific facts leaking in) with no uplift
downside, since genuinely useful findings still get promoted explicitly.

**Difficulty**: M

---

### §2 Cursor Memories — background-model-proposed, user-approved, project-scoped facts (shipped then deprecated)

**Source**: https://cursor.com/changelog/1-0 ; forum thread confirming
removal: https://forum.cursor.com/t/custom-modes-and-memories-gone-in-2-1/143744

**What it does**: Shipped in Cursor 1.0 (June 2025): a background model
watches conversations, proposes a candidate memory, and the user must
approve it via a UI prompt before it's persisted; approved memories are
stored per-project and manageable from Settings → Rules ("Generate Memories"
toggle). By late 2025 (v2.1.x) the feature was removed; Cursor told users to
export existing memories and convert them into (version-controlled) Rules
instead.

**Mapping to engram+MemoryLake**: This is a cautionary data point rather than
a feature to copy outright: Cursor's own team apparently concluded that
*ungated, auto-proposed* memory was a weaker abstraction than curated,
version-controlled Rules for durable knowledge. engram's default SQLite-
backend flow already requires no per-fact approval (dedupe + `mem_judge`
conflict surfacing is the closest analog); the actionable change is to keep
pushing recurring/high-confidence facts *out* of ephemeral auto-capture
(`mem_capture_passive`) and into topic-keyed, upsertable facts (`mem_save`
with `topic_key`) that behave like Cursor's "Rules" — durable and explicitly
curated — rather than trying to build a full approve/reject UI loop that
Cursor itself walked back. Windsurf's docs (below) independently reach the
same conclusion — see §5 for the merged row.

**Expected impact**: Avoiding the ungated-memory failure mode should protect
precision/uplift as project age grows; no direct token-cost change, this is
a design-lesson, not a new mechanism.

**Difficulty**: S (process/doc change only — reinforce topic_key usage in the
memory-protocol text, no new code)

---

### §2 Devin Knowledge — trigger-description-gated retrieval instead of bulk loading

**Source**: https://docs.devin.ai/product-guides/knowledge (via Cognition's
Sept 2024 product update, still current per docs.devin.ai as of 2026)

**What it does**: Each Knowledge item pairs a short natural-language "trigger
description" with its content; Devin retrieves an item only when the current
task context matches its trigger, not all-at-once or all-at-session-start —
this is explicit, documented context-aware retrieval rather than eager
injection. Items also carry an optional macro identifier (e.g.
`!deploy-checklist`) so a user can force-invoke one directly in a prompt.

**Mapping to engram+MemoryLake**: engram's `mem_context`/protocol injection
is still largely eager (the ~10.3KB SessionStart blob). Add a `trigger`
field to facts (short natural-language description, distinct from the
free-text content) and have `mem_session_start` inject only trigger
descriptions for lower-confidence/long-tail facts (cheap, one line each)
while reserving full-content injection for high-confidence/pinned facts —
with `mem_search`/an agent-side "this trigger matched" step pulling full
content on demand. The macro-identifier idea maps directly onto engram's
existing `topic_key`, which could double as an explicit `@topic_key` recall
shorthand in prompts. This is the same "index/description first, body on
demand" idea as Windsurf's `model_decision` activation mode below and as
progressive disclosure / JIT retrieval (§4) — see §5 for the merged row
spanning all of these.

**Expected impact**: Large potential injected-token reduction for projects
with many long-tail facts (only descriptions, not full bodies, load by
default) with uplift preserved or improved since relevance-matching happens
per-task instead of dumping everything upfront.

**Difficulty**: M

---

### §2 Devin Knowledge — feedback-driven auto-suggestion with scoping tiers (user/repo/org/enterprise)

**Source**: https://docs.devin.ai/product-guides/knowledge ;
https://x.com/cognition_labs/status/1836866705521529330

**What it does**: Devin automatically suggests new Knowledge entries based on
user feedback given mid-chat (correcting Devin's behavior), which the user
can edit, dismiss, or regenerate before it's saved — a lighter-weight version
of Cursor's approve-then-store loop, but keyed off explicit correction rather
than passive inference. Knowledge also has four scopes (user, repository,
organization-default, enterprise), with a path to promote proven org-level
knowledge up to enterprise scope.

**Mapping to engram+MemoryLake**: engram already has per-project scoping but
no explicit "correction → suggested fact" loop, and no promotion path across
scope tiers (MemoryLake tenants are already org/workspace-level, but there's
no "promote a project fact to org default" flow). Concretely: (1) have the
memory-protocol text instruct the agent that when a user issues a correction
("no, don't do X, do Y"), it should propose a `mem_save` with a short trigger
+ content pair rather than only appending to session notes; (2) add a
`mem_promote` MCP tool that copies a fact from project scope into a new
"org-default" MemoryLake scope other projects on the same tenant inherit
from.

**Expected impact**: Improves uplift by capturing corrections systematically
instead of relying on the agent remembering to call `mem_save` unprompted;
promotion tier adds cross-project leverage without extra per-project token
cost.

**Difficulty**: M

---

### §2 Windsurf Memories vs. Rules — explicit "ephemeral vs. durable" split with a documented recommendation

**Source**: https://docs.windsurf.com/windsurf/cascade/memories (redirects
to docs.devin.ai/desktop/cascade/memories post-acquisition)

**What it does**: Cascade auto-generates unapproved, credit-free Memories
stored locally (`~/.codeium/windsurf/memories/`), workspace-scoped and never
committed to source control or shared with teammates; the docs explicitly
recommend that "for knowledge you want Cascade to reliably reuse, write it as
a Rule or add it to AGENTS.md... Rules are version-controlled, shareable with
your team" — i.e. the vendor itself frames auto-memory as a convenience
layer, not the durable-knowledge mechanism.

**Mapping to engram+MemoryLake**: This validates engram's existing
architecture (SQLite/MemoryLake as the durable, shareable store vs. any
transient per-session notes) but suggests the memory-protocol text should
say so more explicitly to the agent: instruct it that "facts worth another
session or another teammate seeing must go through `mem_save` with a
`topic_key`" and that anything not saved that way is Cascade-Memory-
equivalent (i.e., gone). Also worth an explicit doc callout in `DOCS.md`/
`memory-protocol` skill contrasting engram's durable store against ephemeral
tool/session state so agents don't under-persist. Reaches the same
conclusion as Cursor's deprecation of ungated Memories above — see §5 for
the merged row.

**Expected impact**: Documentation-level change; expected effect is fewer
"lost" facts (agent assumed something persisted when it didn't), improving
downstream uplift; no token-cost change.

**Difficulty**: S

---

### §2 Windsurf/Cursor rule activation modes: always_on / model_decision / glob / manual

**Source**: https://docs.windsurf.com/windsurf/cascade/memories (redirected)
— four-mode table; compare Cursor's `.mdc` frontmatter modes at
https://docs.cursor.com (Project Rules)

**What it does**: Windsurf Rules declare one of four activation modes:
`always_on` (always in every system prompt), `model_decision` (only a short
description is shown up front; the model can request the full rule body when
it judges it relevant), `glob` (auto-applied when matching files are
touched, same idea as Claude Code's `paths` frontmatter), or `manual` (only
loads when explicitly `@rule-name`-mentioned in a prompt). `model_decision`
is the most distinctive — it puts the *relevance judgment* in the model's
hands rather than a static glob or a human toggle.

**Mapping to engram+MemoryLake**: engram facts today are essentially always
"manual" (retrieved only if the agent calls `mem_search`) or bulk-injected
via `mem_context`/SessionStart. Add a `model_decision`-style mode:
`mem_context` returns short one-line descriptions for a broader tail of
medium-confidence facts (cheap), and the memory-protocol text instructs the
agent to call `mem_get_observation`/`mem_search` for the full body only when
a description looks relevant to the current turn — functionally a two-stage
retrieval (description-first, body-on-demand) analogous to how tool
descriptions work in the MCP/tool-use spec itself. Same underlying pattern as
Devin Knowledge's trigger-gated retrieval above and progressive disclosure /
JIT retrieval (§4) — see §5 for the merged row.

**Expected impact**: Meaningfully lowers injected-token cost for the "maybe
relevant" tail of facts (only ~1 line each vs. full content) while preserving
uplift, since the model can still pull full content when a description
matches.

**Difficulty**: M

---

### §2 Windsurf tiered character budgets per rule scope (global 6,000 / workspace 12,000 chars, enterprise read-only)

**Source**: https://docs.windsurf.com/windsurf/cascade/memories (redirected,
table of scopes/limits)

**What it does**: Windsurf hard-caps how much can live in each rule tier —
6,000 characters for the always-loaded global rules file, 12,000 characters
per workspace rule file, with an enterprise tier that's read-only and
IT-managed — enforcing a fixed, predictable ceiling on injected-token cost
per tier rather than leaving it unbounded like a single growing file.

**Mapping to engram+MemoryLake**: engram's ~10.3KB SessionStart protocol is a
single number with no per-scope sub-budget. Split the injection budget
explicitly (e.g. org/tenant defaults capped at ~1-2KB, project facts capped
at ~6-8KB, with anything beyond routed to description-only per the
`model_decision` idea above) and have `mem_session_start`/the protocol
builder enforce and report these sub-budgets (similar to how Claude Code's
`/doctor` now proposes trims when `MEMORY.md` nears its limit) so growth in
one tier can't silently crowd out another. Same underlying pattern as Letta's
labeled memory blocks (§1) and Claude Code's Auto Memory index budget above —
see §5 for the merged row.

**Expected impact**: Predictable, capped injected-token cost regardless of
how much a given project accumulates over time; uplift preserved because
per-tier caps push overflow to on-demand retrieval rather than dropping it
silently.

**Difficulty**: S–M

---

## 3 Evaluation methodology

Direction 3: LongMemEval, LoCoMo, the Mem0 paper's eval protocol, plus
adjacent benchmarks/harnesses (BEAM, SWE-ContextBench, SWE-Bench-CL, the
Zep/Mem0 methodology dispute, Mem0's open-source benchmark harness) — these
directly inform L1/L3 dataset construction in Phase 1.

### §3 LongMemEval

**Source**: https://arxiv.org/abs/2410.10813 (Wu, Wang, Yu, Zhang, Chang, Yu
— "LongMemEval: Benchmarking Chat Assistants on Long-Term Interactive
Memory," 2024); critique/positioning context via
https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/

**What it does**: 500 curated questions embedded in freely-scalable chat
histories (~115k tokens each) that probe five abilities — information
extraction, multi-session reasoning, temporal reasoning, knowledge updates,
and abstention (recognizing when information isn't in memory). It frames
memory systems as a 3-stage pipeline — indexing, retrieval, reading — and
shows commercial assistants and long-context LLMs losing ~30% accuracy on
sustained multi-session recall versus single-session recall.

**Mapping to engram+MemoryLake**: Engram's eval plan already does recall@k/
MRR retrieval QA plus LLM-judged e2e tasks; LongMemEval's contribution is the
*question taxonomy*, not just a score. Build a fixed eval fixture in
`internal/paritytest` (or a new `internal/evalsuite`) with question
categories mirrored 1:1: (1) info-extraction — "what did we decide about X",
(2) multi-session — facts split across 3+ `mem_save` calls that must be
joined, (3) temporal — "what was true as of last Tuesday" exercising
`updated_at`/`revision_count`, (4) knowledge-update — save topic_key A, then
update it, assert `mem_search` returns the latest revision not both, (5)
abstention — assert `mem_context`/`mem_search` return empty/low-confidence
rather than hallucinating when no matching memory exists. Track abstention as
its own metric (currently absent from the recall@k/MRR/LLM-judge trio).

**Expected impact**: Abstention and knowledge-update are the two axes
engram's current plan doesn't explicitly score, yet they map directly onto
MemoryLake's upsert-on-topic_key behavior and the SQLite soft-delete
semantics — a regression in either would be invisible to a plain recall@k
number (recall@k can stay flat while the system silently returns a stale
revision alongside the current one). Adding these as scored categories turns
a latent correctness bug into a CI-visible number.

**Difficulty**: M

---

### §3 LoCoMo

**Source**: https://arxiv.org/abs/2402.17753 (Maharana et al., "Evaluating
Very Long-Term Conversational Memory of LLM Agents," 2024)

**What it does**: 10–50 synthetic-but-human-verified conversations (up to 35
sessions, ~300 turns, built by grounding LLM agent dialogue in personas +
temporal event graphs) with ~200 QA pairs across single-hop, multi-hop,
temporal, open-domain, and adversarial (unanswerable) categories, plus an
event-summarization task over a temporal event graph and a multimodal
image-sharing dialogue task.

**Mapping to engram+MemoryLake**: The multi-hop and event-graph angle is the
useful part for engram, which currently has `internal/store` relations but
no first-class "event graph" answer key. Add a small synthetic fixture
generator (Go, under `internal/evalsuite/fixtures/`) that seeds a project
with N sessions of `mem_save` calls threaded by shared `topic_key` prefixes
(e.g. `bugfix/parser-crash`, then a follow-up `bugfix/parser-crash-2`
referencing it), and write multi-hop questions whose answer requires joining
two or more saved observations — exactly the case `mem_search`'s FTS5
keyword match is weakest at versus true semantic multi-hop reasoning. Also
add LoCoMo's "adversarial" category (questions with no true answer in
memory) as a sibling of LongMemEval's abstention test — same mechanism,
different name in the literature, worth keeping distinct because LoCoMo's
version is deliberately deceptive phrasing rather than plain absence.

**Expected impact**: Multi-hop failures are exactly what recall@k on single
facts hides — you can retrieve both source rows individually (recall@k=1.0)
yet the LLM-judged e2e task still fails because nothing forced the join.
This argues for scoring multi-hop QA separately from single-hop recall@k in
engram's eval report, not folding both into one aggregate number.

**Difficulty**: M

---

### §3 Mem0 paper's evaluation protocol (J-score / LLM-as-judge + accuracy-cost-latency triad)

**Source**: https://arxiv.org/abs/2504.19413 (Chhikara et al., "Mem0:
Building Production-Ready AI Agents with Scalable Long-Term Memory," ECAI
2025); metric usage detail via
https://docs.mem0.ai/core-concepts/memory-evaluation and
https://mem0.ai/blog/understanding-memory-benchmark-for-production-ai-agents

**What it does**: Evaluates on LOCOMO's four QA categories using an
"LLM-as-judge" correctness score (J score) rather than string-match F1,
alongside p50/p95 latency and mean tokens-per-retrieval-call, explicitly
reporting all three together rather than optimizing accuracy in isolation.
Later work (`docs.mem0.ai`) adds a fixed "top-200 retrieval budget" so
systems can't win by brute-forcing the whole context window into the prompt.

**Mapping to engram+MemoryLake**: Engram's plan already pairs recall@k/MRR
with an LLM judge for e2e tasks — the missing piece is reporting them as a
*triad* the way Mem0 does, not as separate uncorrelated numbers. Concretely:
extend whatever harness computes recall@k/MRR to also emit p50/p95
wall-clock latency for `mem_search` and `mem_context` (both SQLite-backend
and MemoryLake-backend paths, since MemoryLake adds a network hop) and mean
tokens returned per call. Cap retrieval at engram's actual production limit
(`mem_search`'s default result count) rather than an unbounded top-N, so the
benchmark can't inflate recall by dumping the whole memory store into the
LLM-judge context — mirroring Mem0's "top-200 budget" discipline.

**Expected impact**: Without a token/latency ceiling, any recall number is
gameable (retrieve everything, let the LLM judge sort it out) and doesn't
predict production behavior, where `mem_context`'s ~10.3KB SessionStart
injection budget and MemoryLake's network latency are real constraints.
Reporting the triad prevents engram's own eval from quietly rewarding
"return more, filter less."

**Difficulty**: S

---

### §3 BEAM (Beyond a Million Tokens)

**Source**: https://github.com/mohammadtavakoli78/BEAM (ICLR 2026, "Beyond a
Million Tokens: Benchmarking and Enhancing Long-Term Memory in LLMs");
results summary via
https://mem0.ai/blog/what-is-beam-memory-benchmark-the-paper-that-shows-1m-context-window-isnt-enough

**What it does**: Auto-generates coherent conversations up to 10M tokens with
2,000+ validated probing questions across ten memory-ability categories
(preference-following, instruction-following, extraction, knowledge-update,
multi-session reasoning, summarization, temporal reasoning, event ordering,
abstention, contradiction resolution), explicitly to test scale ranges where
long-context alone stops working (reported Mem0 scores drop from 92.5 on
LoCoMo to 64.1/48.6 at 1M/10M tokens).

**Mapping to engram+MemoryLake**: Engram's realistic ceiling is nowhere near
10M tokens per project, but the *shape* of the test — does quality degrade
gracefully as memory volume grows — is directly applicable to long-lived
projects with hundreds of sessions. Add a scale sweep to the eval harness:
run the same recall@k/MRR/LLM-judge suite at project sizes of ~50, ~500, and
~5,000 saved observations (synthetically seeded), and plot the degradation
curve per backend (SQLite FTS5 vs. MemoryLake semantic search). This is
cheap to build since `internal/store` already has bulk-insert paths for
migration/testing.

**Expected impact**: This is the single highest-value addition for catching
a slow regression: FTS5 keyword relevance and MemoryLake's semantic ranking
may both look fine at demo scale (tens of memories) and silently degrade at
real project scale (a project that's been running for a year). A one-time
recall@k score at small N would never surface that; a scale sweep would.

**Difficulty**: M

---

### §3 SWE-ContextBench (SWE-bench-style e2e evaluation of memory/context frameworks)

**Source**: https://arxiv.org/abs/2602.08316 ("SWE Context Bench: A
Benchmark for Context Learning in Coding," 2026); results discussion via
https://medium.com/@mrsandelin/the-first-controlled-benchmark-of-ai-memory-in-coding-agents-8e0bb776d39e

**What it does**: Evaluates memory/context frameworks (Mem0, OpenViking,
LangMem, Supermemory) as drop-in components inside a real coding-agent loop
solving SWE-bench-style tasks, scoring on the actual SWE-bench axes —
FAIL_TO_PASS rate, PASS_TO_PASS (no-regression) rate, and overall resolution
rate — plus wall-clock runtime and dollar cost per task, rather than a
memory-specific QA score.

**Mapping to engram+MemoryLake**: This is the template for engram's
"LLM-judged e2e coding tasks" leg. Concretely: build a small internal harness
that runs the same coding task (e.g., "fix bug X in repo Y") three ways — no
memory, engram/SQLite memory, engram/MemoryLake memory — using an actual
coding agent loop (Claude Code or the opencode plugin engram already ships),
and score task success (does the patch pass tests) plus regression rate
(does anything break), not just "did the agent recall the right fact." Reuse
SWE-bench-style repos/issues if licensing allows, or a small in-house set of
10–20 regression-prone bugs the team has already fixed once (so the "right
answer" for memory recall is the actual prior fix committed to git history).

**Expected impact**: This is the only technique that measures memory's
effect on the outcome that matters — did the agent actually resolve the task
faster/better because it remembered something — as opposed to whether it
retrieved the "correct" text fragment. It directly operationalizes the
"LLM-judged end-to-end coding tasks" leg of engram's own eval plan and gives
it a concrete scoring scheme borrowed from a published benchmark instead of
inventing one from scratch.

**Difficulty**: L

---

### §3 SWE-Bench-CL (continual-learning metrics: forgetting, forward/backward transfer)

**Source**: https://arxiv.org/abs/2507.00014 ("SWE-Bench-CL: Continual
Learning for Coding Agents")

**What it does**: Chronologically orders GitHub issues per repository
(mirroring real repo evolution) and evaluates agents with memory on/off
across the sequence, scoring average accuracy, "forgetting" (does
performance on earlier-learned tasks degrade as later tasks are learned),
forward/backward transfer (does memory from task N help task N+k, and does
learning task N+k retroactively improve recall of task N's context), plus a
composite continual-learning score.

**Mapping to engram+MemoryLake**: Engram's memory protocol explicitly
supports `topic_key`-based upserts across sessions on the same project —
this benchmark's "forward transfer" metric is a direct probe of that upsert
path. Concretely: seed a project with a chronological sequence of related
`mem_save` calls (e.g., a bug diagnosed in session 1, a partial fix in
session 2, a regression discovered in session 5), then measure whether an
agent answering a session-10 question about that bug uses the session-5
revision (not a stale session-1 fact) — a direct test of MemoryLake/SQLite
`revision_count` and soft-delete semantics doing their job across a long
timeline, not just within one retrieval call.

**Expected impact**: This is the sharpest tool for validating the specific
claim in engram's CLAUDE.md that MemoryLake handles "dedup, updating
existing memories, and merging contradictions... automatically" — a
forgetting/transfer metric would catch the case where an old, superseded
fact still surfaces in `mem_search` results alongside (or instead of) the
current one, which recall@k treats as a false positive but a raw "did it
retrieve something related" score would not.

**Difficulty**: M

---

### §3 Adversarial/methodology-audit evaluation (Zep's critique of Mem0's LoCoMo results)

**Source**: https://blog.getzep.com/lies-damn-lies-statistics-is-mem0-really-sota-in-agent-memory/

**What it does**: Not a benchmark but an evaluation-of-the-evaluation: it
re-runs Mem0's published LoCoMo numbers and finds a full-context baseline
(dump the whole 16–26k-token conversation into the prompt, no memory system
at all) beats Mem0's own memory pipeline on the same LLM-judge metric (~73%
vs. Mem0's ~68%), and separately flags dataset defects (missing ground truth
in one category, incorrect speaker attribution, ambiguous questions) and
implementation bugs in both sides' harnesses (sequential vs. parallel search
skewing latency, a broken user-model assumption).

**Mapping to engram+MemoryLake**: Adopt "full-context / no-memory" as a
mandatory baseline row in every engram eval report, not an optional
ablation. Concretely: for any project small enough to fit its full history
in context, the eval harness should also run the LLM-judged e2e task with
*zero* `mem_search`/`mem_context` calls (raw prompt + task only) and report
that score alongside the memory-enabled scores. If engram's memory-enabled
score doesn't beat the no-memory baseline by a clear margin, that's a
harness or product bug, not noise — surface it as a hard CI gate.

**Expected impact**: This is the cheapest possible sanity check and the one
most likely to catch an eval-methodology bug (e.g., an LLM judge that's
biased toward longer answers, or a fixture where the answer is guessable
from the question itself) before publishing a "memory helps" claim. Given
how easy it is to accidentally build a benchmark that a no-op beats, this
should be a gate on the whole eval plan, not a nice-to-have.

**Difficulty**: S

---

### §3 Mem0's open-source `memory-benchmarks` harness (ingestion → search → evaluation pipeline)

**Source**: https://github.com/mem0ai/memory-benchmarks

**What it does**: A reusable three-stage evaluation pipeline (ingest
conversation data into the memory system under test → run search/retrieval
queries against it → score answers with an LLM judge and aggregate
pass-rate/average-score/per-question-type breakdowns), packaged with
Docker-based local setup and a defined client interface so third-party
memory systems can be plugged in and compared against Mem0's own numbers
under the same LOCOMO/LongMemEval/BEAM datasets.

**Mapping to engram+MemoryLake**: Structure engram's own eval harness
(wherever it lands — likely `internal/paritytest` or a sibling
`internal/evalsuite`) along the same three-stage seam: an `Ingest(fixture)
error` step that drives `mem_save` calls against either backend, a
`Query(question) []Candidate` step that drives `mem_search`/`mem_context`,
and a separate `Score(candidates, goldAnswer) Result` step using an LLM
judge — kept as three composable interfaces rather than one monolithic eval
function. That seam is what lets the *same* fixture set run against both the
SQLite backend and the MemoryLake backend (via `internal/mcp.BackendSelector`)
for the parity comparison `internal/paritytest` already exists to do, and is
also what would let engram publish its own numbers in a format directly
comparable to Mem0/LoCoMo/LongMemEval leaderboards if that's ever a goal.

**Expected impact**: Structuring for reuse now is cheap and pays off the
moment engram wants a second fixture set (BEAM-style scale sweep,
SWE-ContextBench-style e2e coding tasks) — without the three-stage seam,
each new benchmark idea becomes a bespoke script instead of a new fixture
plugged into the same pipeline.

**Difficulty**: S

---

## 4 Cost-side techniques

Direction 4: prompt-cache-friendly injection layout, context compression,
progressive/layered retrieval (index first, expand on demand), structured
summaries. Grounding facts verified in the current codebase before mapping:
`internal/mcp/mcp.go`'s `handleSearch` already truncates each result body to
a 300-char preview (`truncate(r.Content, 300)`) with `[preview]` appended and
`mem_get_observation` fetching the untruncated body — a two-layer
progressive-disclosure pattern already exists, just without token-count
metadata on the index layer. `handleContext` truncates entry previews to 150
chars and the "focus" block to 500 chars. `plugin/claude-code/scripts/session-start.sh`
emits a static heredoc protocol block followed by a dynamic `mem_context`
payload — static-then-dynamic ordering is already correct for caching, but
the block has no `cache_control` semantics and a "slim"/"full" mode toggle
(`engram protocol-mode`) that can flip per project, which risks moving the
cache breakpoint. Recent commits (`8abd3dd`, `629f775`, `ea27644`) already
moved live `mem_save` and session-end summaries to *verbatim* fact writes
rather than LLM-extracted/summarized text.

### §4 Prompt-cache-friendly static/dynamic separation

**Source**: https://platform.claude.com/docs/en/build-with-claude/prompt-caching

**What it does**: Anthropic's prompt caching hashes a prefix of `tools →
system → messages` up to a `cache_control` breakpoint; cache reads cost
~10% of base input tokens vs. 1.25x (5-min TTL) or 2x (1-hour TTL) for cache
writes, but any change to content *before* the breakpoint (including tool
defs) invalidates everything after it. Breakpoints must sit on the last
byte-identical block, with a 20-block lookback window, so dynamic content
must always be appended after — never interleaved with — the cached static
block.

**Mapping to engram+MemoryLake**: `session-start.sh` already emits static
protocol text before the dynamic `mem_context` payload, which is the right
shape, but two things break cache stability today: (1) `engram
protocol-mode` can return "slim" vs "full" per project, changing the static
block's byte content across sessions of the same project and busting the
cache; (2) the protocol text has no explicit version marker, so any future
wording tweak silently changes the prefix without a way to detect it.
Concrete change: pin protocol-mode per-project (persist the resolved mode
once per project rather than re-deriving it every SessionStart) and add an
explicit `## Engram Protocol vN` header so the static block is provably
identical across sessions until a deliberate version bump; document in
`DOCS.md`/`skills/memory-protocol/SKILL.md` that this text must never
contain interpolated values (timestamps, session IDs, project names) — those
belong only in the dynamic suffix.

**Expected impact**: No effect on task uplift (purely a cost lever). Cost
impact is high-leverage because the harness (Claude Code) applies its own
`cache_control` to system/additionalContext — if engram's static block is
unstable, every session pays full-price input tokens for ~10.3 KB (~2.6K
tokens) instead of ~0.1x that after the first hit in a session; stabilizing
it is a prerequisite for *any* caching benefit, at effectively zero downside.

**Difficulty**: S

---

### §4 Structured note-taking, compaction, and just-in-time retrieval

**Source**: https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents

**What it does**: Anthropic's applied-AI team recommends three linked
patterns: (1) persistent structured notes written outside the context window
and pulled back in later, (2) "compaction" — summarizing a near-full context
and reinitializing with the summary, prioritizing decisions/bugs/
implementation details over redundant tool output, and (3) just-in-time
retrieval — keep only lightweight references (file paths, IDs, queries) in
context and dynamically load full content via tool calls instead of
pre-loading everything upfront.

**Mapping to engram+MemoryLake**: Engram already implements (1) as its
entire premise. (3) is partially implemented (`mem_search` returns 300-char
previews + IDs, `mem_get_observation` expands) but not enforced as the
*default contract* — nothing in the tool description or protocol text tells
the agent "prefer the reference, only expand when needed." Concrete change:
rewrite the `mem_search` tool description and the SessionStart protocol
block to explicitly frame results as references-first ("each result is a
pointer; call `mem_get_observation` only for entries you will act on"), and
apply Anthropic's compaction pattern to `mem_session_summary`: today it
stores whatever the agent writes verbatim (correctly, post commit `ea27644`)
but the *protocol instructions telling the agent what to include* should
explicitly bias toward decisions/bugs/next-steps over transcript-like
detail, mirroring "prioritize architectural decisions ... discarding
redundant tool outputs." This is the same "reference-first, expand-on-demand"
family as Devin Knowledge's trigger-gated retrieval, Windsurf's
`model_decision` mode (both §2), and claude-mem's progressive disclosure
below — see §5 for the merged row.

**Expected impact**: Reduces `mem_search` follow-up expansion calls (each
`mem_get_observation` currently returns full content unconditionally) since
agents will more often act on the preview alone — cuts per-session dynamic
overhead. Uplift-neutral-to-positive: compaction bias toward decisions over
noise should slightly *improve* the gotcha-reproduction task category by
keeping session summaries information-dense rather than verbose.

**Difficulty**: S

---

### §4 Progressive disclosure (index → details → deep dive)

**Source**: https://docs.claude-mem.ai/progressive-disclosure

**What it does**: A three-layer memory access pattern — Layer 1 is a compact
index (ID, timestamp, type icon, title, token count) covering many
observations for very few tokens; Layer 2 is the full observation body
fetched only for entries the agent judges relevant; Layer 3 is the original
source file/commit, read only if the observation text itself is
insufficient. The documented example: pre-loading everything costs 35,000
tokens for 6% relevance, while progressive fetching costs 920 tokens at 100%
relevance.

**Mapping to engram+MemoryLake**: Engram's `mem_search`/`mem_get_observation`
pair is Layer 1+2 already; Layer 3 (source file/commit) is unimplemented —
observations that reference file paths or commit SHAs have no follow-up
mechanism to fetch the referenced file region. Concrete change, two parts:
(a) add a per-result **token-count field** to `mem_search`/`mem_context`
output (cheap: `len(content)/4` or a real tokenizer call) next to the
existing 300-char preview, so the agent can see "this observation is 1,800
tokens" before deciding to expand — directly copying claude-mem's index
annotation; (b) when an observation's structured content includes a `files`
field (already tracked per the session-summary schema), surface it as a
suggested Layer-3 action in the tool description rather than requiring the
agent to separately grep for the file. Same "reference-first, expand-on-
demand" family as §4's structured note-taking/JIT retrieval and §2's Devin
Knowledge/Windsurf `model_decision` — see §5 for the merged row.

**Expected impact**: Token-count-aware previews should reduce unnecessary
`mem_get_observation` calls (skip expanding large, marginally-relevant hits),
directly lowering per-session retrieval overhead. Uplift impact is likely
near-neutral-to-positive since the agent is not deprived of any information
it currently uses, only given better cost visibility before choosing to
spend tokens.

**Difficulty**: S (token-count field) / M (Layer-3 file-region fetch tool)

---

### §4 LLMLingua-2 / LongLLMLingua prompt compression

**Source**: https://github.com/microsoft/LLMLingua ;
https://llmlingua.com/longllmlingua.html ;
https://www.microsoft.com/en-us/research/publication/llmlingua-2-data-distillation-for-efficient-and-faithful-task-agnostic-prompt-compression/

**What it does**: LLMLingua-2 is a small BERT-level token-classifier
(distilled from GPT-4 labels) that drops low-information tokens from a
prompt, compressing to as little as 20% of original length at 3–6x lower
compression latency than LLMLingua-1, with claimed minimal task-performance
loss. LongLLMLingua extends this with a question-aware, coarse-to-fine
compression plus document reordering, mitigating "lost-in-the-middle" and
reporting up to 21.4% performance improvement at 4x compression on
long-context QA/RAG benchmarks.

**Mapping to engram+MemoryLake**: Applicable to the highest-volume dynamic
slice: `mem_search` result bodies and `mem_context` payloads, especially
once Layer-3 file content is added. Concrete change: add an optional
compression step in `internal/mcp` (or a small Go-side heuristic port, since
LLMLingua's reference implementation is Python/PyTorch and pulling it into
the Go binary is heavy) that applies question-aware compression to the
top-k search results before they're written into the tool response — using
the *user's current query* as the "question" signal, exactly as
LongLLMLingua does for RAG. Given engram is pure-Go/CGO_ENABLED=0, the
pragmatic path is not to embed the transformer model but to call it as a
sidecar service, or to substitute a much cheaper Go-native proxy
(stopword/redundancy stripping + extractive sentence ranking against query
TF-IDF) and treat true LLMLingua-2 as a stretch goal.

**Expected impact**: Meaningful cost reduction on the dynamic/search slice
specifically (the "retrieval dynamic overhead × avg calls/session" leg of
the cost budget) — LongLLMLingua's own numbers suggest 2–4x compression with
*positive* rather than negative uplift on RAG-style QA, because irrelevant
filler is what gets cut. Risk: engram's `mem_search` results are dense
structured facts (title/type/scope/content), not prose passages — LLMLingua's
token-classifier was trained on natural-language prompts, so the compression
ratio and safety of dropping tokens on structured fact text is unverified
and would need eval validation before trusting it in the loop. Should be
applied only to *read-time, per-query* filtering of retrieved results, never
to compressing the stored fact itself (see the verbatim-vs-extracted
guardrail below).

**Difficulty**: L (true LLMLingua-2 integration, new sidecar dependency) / M
(cheap Go-native extractive approximation)

---

### §4 RAPTOR — recursive tree-organized summarization for retrieval

**Source**: https://arxiv.org/pdf/2401.18059 (ICLR 2024; Sarthi, Abdullah,
Tuli, Khanna, Goldie, Manning — Stanford)

**What it does**: RAPTOR recursively embeds, clusters, and summarizes chunks
bottom-up into a tree of progressively more abstract summaries, then
retrieves at whichever level of abstraction best answers the query —
letting a single retrieval traverse from fine detail up to document-level
synthesis instead of only ever pulling flat, equally-sized chunks. It shows
consistent gains over flat-chunk RAG baselines on multi-hop and long-document
QA (NarrativeQA, QASPER, QuALITY).

**Mapping to engram+MemoryLake**: Directly targets a gap in
`mem_context`/`mem_search`: today "recent context" and "search hits" are
flat lists of individual observations with no cross-observation synthesis,
so a question like "what's our overall ZDB vacuum strategy" requires the
agent to read many separate observations and synthesize itself, in-context,
every session. Concrete change (mostly MemoryLake-server-side proposal,
since fact clustering/synthesis is exactly the kind of extraction pipeline
MemoryLake already runs): propose an interface where MemoryLake periodically
clusters related facts (by topic_key prefix or embedding similarity) into a
rolled-up "topic summary" fact, and `mem_context`/`mem_search` surface the
rolled-up summary as a top hit with the individual facts available one level
down (mirrors RAPTOR's tree levels). On the SQLite-backend side (no
clustering infra), a cheaper partial version is: when many observations
share a `topic_key` prefix, `mem_search` already collapses via topic_key
upsert (revision_count++) — the gap is only for *related-but-distinct*
topic_keys, which would need either an engram-side periodic batch job or
remain a MemoryLake-only capability. This is the same category of periodic-
consolidation infrastructure as Letta's sleep-time compute and LangMem's
hot-path/background split (both §1) and Anthropic Managed Agents' "Dreaming"
(§2) — see §5 for the merged row spanning all four.

**Expected impact**: Uplift-positive for architecture-QA-style tasks — this
is precisely the failure mode RAPTOR was built to fix. Cost-negative in
isolation (an extra summarization pass costs tokens/compute at write time)
but cost-positive at read time since one rolled-up fact replaces N raw facts
in the injected context.

**Difficulty**: L (requires new clustering/synthesis pipeline; primarily a
MemoryLake server-side proposal, not an engram Go client change)

---

### §4 Context Rot — precision over volume as a design constraint

**Source**: https://www.trychroma.com/research/context-rot (Chroma Research,
Hong/Troynikov/Huber, July 2025)

**What it does**: Testing 18 frontier models, Chroma found LLM output
quality degrades measurably as input length grows even well below the
context limit, via lost-in-the-middle attention, attention dilution, and
distractor interference from semantically-similar-but-irrelevant content —
and, counterintuitively, models did *better* on shuffled/incoherent long
contexts than on well-structured ones, implying structural coherence isn't
what saves long contexts from degradation, only *not including the
distractors in the first place* does.

**Mapping to engram+MemoryLake**: This is not a technique to implement but a
constraint that should reframe how engram's other cost levers are
evaluated: it directly argues against "just inject more retrieved facts to
be safe" as a mitigation for retrieval-quality gaps, and in favor of the
progressive-disclosure/JIT techniques above. Concrete change: the eval
harness's retrieval metrics (recall@k, tokens returned per query — see §3)
should be read jointly — a change that raises recall@10 by returning more,
lower-precision hits is not free even when MRR looks fine, because each
additional low-relevance hit is a Chroma-style distractor. Recommend adding
a per-round "distractor ratio" check (fraction of injected `mem_search` hits
judged irrelevant by the LLM-judge rubric) alongside recall@k, so precision
regressions aren't masked by a token-cost win.

**Expected impact**: Not a standalone cost or uplift lever; it's evidence
that engram's cost-reduction program (Phase 2) must optimize
`mem_search`/`mem_context` for precision, not just shrink token count — a
naive compression pass that keeps recall@k constant but doesn't reduce
distractor count could cut tokens without recovering the uplift the program
is chasing.

**Difficulty**: S (add a distractor-ratio metric to the existing eval
harness)

---

### §4 Verbatim chunks vs. extracted/summarized artifacts

**Source**: https://arxiv.org/pdf/2601.00821 ("Verbatim Chunks Beat
Extracted Artifacts: A Controlled Ablation of Memory Representations for
Long LLM Conversations")

**What it does**: A controlled ablation across LoCoMo and related
long-conversation benchmarks finding verbatim conversation chunks
substantially outperform lossy LLM-extracted/summarized artifacts on
retrieval accuracy and multi-hop reasoning fidelity — the paper's stated
trade-off is that verbatim storage costs more tokens per stored unit, but
the accuracy gain justifies it, i.e. "fidelity before structure" beats
aggressive compression at write time.

**Mapping to engram+MemoryLake**: This is direct external validation of
engram's own recent direction — commits `8abd3dd`/`629f775`/`ea27644`
already moved live `mem_save` and session-end summaries to verbatim
direct fact-add rather than LLM-extracted text, specifically to avoid this
failure mode. The actionable follow-up is the flip side: this paper is a
caution *against* over-applying the LLMLingua/RAPTOR compression techniques
above at write time — any compression introduced into the pipeline should
be scoped to *read-time, per-query* filtering of retrieved results (where
the question-aware LongLLMLingua approach applies), never to compressing
the stored fact itself, since that would reproduce exactly the "extracted
artifact" failure mode this paper measures against verbatim storage.

**Expected impact**: No new change needed — this is a design-boundary
confirmation. Its practical value is as a guardrail for the Phase 2
optimization loop: any proposed change to *how facts are stored* (as opposed
to how they're rendered/filtered at injection time) should be run through
eval before adoption, because the literature shows compression-at-storage is
the one place in this survey where the intuitive cost win is likely to
actively hurt uplift.

**Difficulty**: S (documentation/guardrail — record this as an explicit
non-goal in the Phase 2 optimization scope, no code change)

---

## 5 Priority matrix

Rows that collapse techniques covered by more than one direction are marked
"(merged)" and cite every contributing section. Priority is gain÷difficulty
judgment: **P0** = do first (cheap, low risk, clear win), **P1** = valuable
but larger/riskier or dependent on external (MemoryLake) coordination, **P2**
= worth doing but low marginal leverage or narrow applicability.

| Candidate change | Direction source | Expected gain (effect / cost) | Difficulty | Priority |
|---|---|---|---|---|
| Stabilize the static SessionStart protocol block (pin protocol-mode, add a version header) so prompt caching actually holds | §4 Prompt-cache-friendly static/dynamic separation | Cost: unlocks ~0.1x input-token pricing on ~2.6K tokens/session that's currently paid in full every session; effect: none (pure cost lever), but is a prerequisite for every other cache-dependent win | S | P0 |
| Reframe `mem_search`/`mem_context` results as reference-first, expand-on-demand (tool descriptions + protocol text bias, token-count index field) (merged) | §2 Devin Knowledge trigger-gated retrieval; §2 Windsurf `model_decision` activation mode; §4 Structured note-taking/JIT retrieval; §4 Progressive disclosure (claude-mem) | Cost: large reduction in unnecessary `mem_get_observation` expansions and long-tail full-body injection; effect: neutral-to-positive (agent loses no information, only spends tokens more deliberately) | S–M | P0 |
| Add a capped, budget-enforced tiering of the injection surface: labeled/budgeted sections + self-monitored size check + per-tier char caps (merged) | §1 Letta labeled memory blocks; §2 Auto Memory MEMORY.md budget; §2 Windsurf tiered character budgets | Cost: bounds injected-token growth over a project's lifetime, prevents one noisy topic crowding out another; effect: modest positive from more predictable, proportional context composition | S–M | P0 |
| Add a "distractor ratio" metric to the eval harness alongside recall@k/MRR | §4 Context Rot — precision over volume | Prevents Phase 2 from shipping a token-cost win that quietly increases irrelevant-hit rate; methodology fix, not a product change | S | P0 |
| Mandatory no-memory/full-context baseline row in every eval report | §3 Zep's Mem0 critique (adversarial audit) | Cheapest possible sanity gate against a broken benchmark; catches the single worst failure mode (memory looks like it helps but doesn't) before any other number is trusted | S | P0 |
| Report recall@k/MRR + p50/p95 latency + mean tokens/call as one triad, capped at production retrieval limits | §3 Mem0 eval protocol (J-score triad) | Prevents recall from being gamed by unbounded retrieval; makes eval numbers predictive of production cost | S | P0 |
| Structure the eval harness as Ingest → Query → Score composable interfaces | §3 Mem0 `memory-benchmarks` harness shape | Enables every other eval technique below (LongMemEval, LoCoMo, BEAM fixtures) to plug into one pipeline instead of bespoke scripts | S | P0 |
| Record "no compression at write time" as an explicit Phase 2 non-goal | §4 Verbatim chunks vs. extracted artifacts | Guardrail only; prevents a plausible but literature-contradicted mistake (compressing stored facts) | S | P0 |
| Document "durable facts go through `mem_save`+`topic_key`, everything else is ephemeral and will be lost" more explicitly in protocol/skill text (merged) | §2 Cursor Memories cautionary tale (deprecated); §2 Windsurf Memories-vs-Rules | Protects precision/uplift as project age grows by discouraging ungated auto-capture from becoming the durable-knowledge mechanism; doc-only | S | P1 |
| Add `mem_pin`-rendered "core memory" block: small, always-injected, capped section ahead of recent observations (merged) | §1 MemGPT/Letta tiered memory; §4 Tiered memory via `mem_pin` | Effect: high for gotcha-reproduction tasks (100% recall of must-not-forget facts vs. relevance-ranked chance); cost: small, bounded rise (~0.5–2KB), also reinforces cache stability | M | P0 |
| Add `valid_from`/`valid_until` to the fact schema; de-rank/omit superseded facts in `mem_search`/`mem_context` (merged) | §1 Zep/Graphiti bi-temporal knowledge graph; §4 Zep/Graphiti temporal validity | Effect: high — removes the exact failure mode where a stale fact outranks or coexists with its replacement; cost: reduces wasted tokens on stale content proportional to revision frequency; requires MemoryLake-side tagging support for the full version | L | P1 |
| Add a background/session-end/scheduled consolidation pass: merge near-duplicate topic_keys, promote recurring corrections, prune stale facts, optionally roll up related-but-distinct topics into a summary fact (merged) | §1 Letta sleep-time compute; §1 LangMem hot-path vs. background consolidation; §2 Managed Agents "Dreaming"; §4 RAPTOR topic rollups | Effect: indirect but meaningful — better signal-to-noise in every subsequent retrieval, catches memories a distracted agent never explicitly saved; cost: fewer, denser facts reduce injected tokens over long project lifetimes; needs an LLM call per pass and careful scoping | L | P1 |
| Collapse `mem_save` → `judgment_required` → `mem_judge` into a single response carrying Mem0-style ADD/UPDATE/DELETE/NOOP suggestions | §1 Mem0 ADD/UPDATE/DELETE/NOOP extraction pipeline | Fewer round-trips (fewer injected tokens per save), reduces risk the agent skips the judgment loop entirely; moderate uplift from fewer duplicate observations | M | P1 |
| Persist `mem_judge` relation verbs as a standing `relations` table; expand `mem_search` hits with bounded 1-hop neighbors | §1 A-Mem Zettelkasten dynamic note linking | Uplift from surfacing related-but-not-textually-similar memories (e.g. a bugfix and the decision that caused it); modest, controllable per-call token cost | M | P1 |
| Add `type=episode` to the `mem_save` type enum; segregate/boost episode results as worked examples | §1 LangMem semantic/episodic/procedural typing | Uplift for repeated procedural workflows (build/test/debug sequences); token-neutral if episodes replace unstructured discovery notes | S | P1 |
| Extend `internal/sync` to optionally materialize `architecture/*` topic_keys as git-diffable Markdown under `.engram/memory/` | §1 Letta Context Repositories | No token-cost effect; catches silent memory corruption/drift by making bad `mem_save`s visible in normal code review | M | P2 |
| Expose MemoryLake's Mem0g entity/relationship graph via a new read-only `mem_related` tool | §1 Mem0g graph memory | Narrow but real uplift for multi-hop "what depends on this" questions; contingent on unverified MemoryLake server capability | L | P2 |
| Add hierarchical scope resolution (org → user → project → local) with lazy subtree loading | §2 Hierarchical CLAUDE.md scoping | Lowers fixed injected-token cost on large monorepos; uplift-neutral-to-positive from higher precision | M | P1 |
| Add `path_glob`-triggered fact injection tied to files the agent actually opens | §2 `.claude/rules/` path-scoped conditional loading | Meaningful token reduction for path-irrelevant facts; uplift improves since retrieval becomes push- rather than agent-recall-dependent | M–L | P1 |
| One-time trust/approval surfacing (via `mem_review`) for facts synced in from another owner/project on first injection | §2 `@path` imports trust/approval dialog | Small friction cost; reduces risk of a teammate's bad/malicious fact silently steering the agent; no token-cost effect | S | P2 |
| Add an `outcome` field / `mem_feedback` tool so success/failure of a session can weight which facts get reinforced vs. flagged | §2 Managed Agents memory+outcomes feedback signal | Positive uplift over time as the system learns which facts are load-bearing; negligible token cost | M | P1 |
| Add an opt-in ephemeral `scope=subagent:<task-id>` plus explicit promotion, so subagent exploration doesn't pollute shared project memory | §2 Subagent-scoped auto memory vs. fork-inherited | Reduces noise/token cost in shared `mem_context` with no uplift downside | M | P2 |
| Add a correction→suggested-fact prompt-text rule plus a `mem_promote` tool for project→org-default scope tiers | §2 Devin Knowledge feedback-driven auto-suggestion + scope tiers | Improves uplift by systematically capturing corrections; promotion adds cross-project leverage at no added per-project cost | M | P1 |
| Add LongMemEval-style scored categories (info-extraction, multi-session, temporal, knowledge-update, abstention) to the eval fixture set | §3 LongMemEval | Turns two currently-unscored axes (abstention, knowledge-update) into CI-visible regressions instead of silent correctness bugs | M | P0 |
| Add LoCoMo-style multi-hop and adversarial (unanswerable) question fixtures, scored separately from single-hop recall@k | §3 LoCoMo | Surfaces multi-hop join failures that recall@k=1.0 on individual facts hides entirely | M | P1 |
| Add a BEAM-style scale sweep (50/500/5,000 seeded observations) per backend to the eval harness | §3 BEAM | Only technique that catches gradual quality degradation at real project scale rather than demo scale; cheap given existing bulk-insert paths | M | P1 |
| Build a real e2e coding-agent harness (no-memory vs. SQLite vs. MemoryLake) scored on FAIL_TO_PASS/resolution rate, not memory-QA score | §3 SWE-ContextBench | The only technique measuring memory's effect on the outcome that matters for the program's ×2 uplift goal; directly operationalizes the acceptance metric | L | P0 |
| Seed chronological `topic_key` sequences and measure whether later sessions use the latest revision, not a stale one (forgetting/forward-transfer) | §3 SWE-Bench-CL | Sharpest validator of the "MemoryLake handles dedup/updates/merging automatically" claim in CLAUDE.md; catches stale-fact false positives recall@k misses | M | P1 |
| Question-aware (LongLLMLingua-style) compression of retrieved search-result bodies at read time, gated by fact-text validation | §4 LLMLingua-2 / LongLLMLingua prompt compression | Real cost win on the dynamic/search slice if validated safe on structured fact text (unverified — LLMLingua was trained on prose, not structured facts) | M–L | P2 |

**29 rows.**
