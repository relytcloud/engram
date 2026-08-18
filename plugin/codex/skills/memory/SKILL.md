---
name: engram-memory
description: "ALWAYS ACTIVE — Persistent memory protocol. Save decisions, conventions, bugs, and discoveries to engram silently, batched at task end — never narrate saves. Answering comes first: the final reply must contain the complete answer itself."
---

# Engram Persistent Memory — Protocol

You have access to Engram, a persistent memory system that survives across sessions and compactions.
This protocol is MANDATORY and ALWAYS ACTIVE — not something you activate on demand.

## AVAILABLE TOOLS

Core tools are loaded automatically at session start by the UserPromptSubmit hook.
They are available immediately — no manual ToolSearch needed.

- `mem_save`, `mem_search`, `mem_context`, `mem_session_summary`
- `mem_get_observation`, `mem_suggest_topic_key`, `mem_update`
- `mem_session_start`, `mem_session_end`, `mem_save_prompt`

**Fallback**: If tools are unexpectedly unavailable, run `engram setup codex`
again and restart Codex. Setup repairs the durable MCP config and
permissions allowlist for the `mcp__engram__...` server ids.

Admin tools (deferred — use ToolSearch only if needed):
- `mem_stats`, `mem_delete`, `mem_timeline`, `mem_capture_passive`

## SAVING — silent, batched, never instead of answering

Save decisions, bug root causes, conventions, gotchas, and user preferences.
But: your final reply must contain the complete answer itself — memory serves
future sessions, never this reply. Never narrate saves ("I've saved this to
memory") and never let a save replace the answer. Batch saves at task end.

What is worth saving (batched at the end of the task, not mid-answer):

- Architecture, design, workflow, or tool/library decisions — including the ones the user confirms, rejects, or states as a preference
- Bug fixes (with root cause) and features implemented with a non-obvious approach
- Conventions and patterns established (naming, structure, approach)
- Non-obvious discoveries, gotchas, edge cases, and constraints learned

Format for `mem_save`:
- **title**: Verb + what — short, searchable (e.g. "Fixed N+1 query in UserList", "Chose Zustand over Redux")
- **type**: bugfix | decision | architecture | discovery | pattern | config | preference
- **scope**: `project` (default) | `personal`
- **topic_key** (optional but recommended for evolving topics): stable key like `architecture/auth-model`
- **content**:
  **What**: One sentence — what was done
  **Why**: What motivated it (user request, bug, performance, etc.)
  **Where**: Files or paths affected
  **Learned**: Gotchas, edge cases, things that surprised you (omit if none)

### Pinning — the exception, not the habit

Pin (mem_pin) only facts whose loss causes irreversible damage or repeated pitfalls; pinned facts are always in context.
The pinned block is the one part of `mem_context` the byte budget never drops, and it
carries its own 1024-byte ceiling: entries render oldest-pin-first, and once the
ceiling is reached the OLDEST pins stop being rendered behind a
`(pin cap reached: N pinned facts not shown — mem_unpin to prune)` marker. Over-pinning
therefore evicts your own earlier pins from context — use `mem_unpin` to prune.
Concretely: with the 300-rune content cap on each pinned line, only ~2-3 pinned facts
render before the 1KB cap is reached — over-pinning evicts your own earlier pins.

### Topic update rules (mandatory)

- Different topics MUST NOT overwrite each other (example: architecture decision vs bugfix)
- If the same topic evolves, call `mem_save` with the same `topic_key` so memory is updated (upsert) instead of creating a new observation
- If unsure about the key, call `mem_suggest_topic_key` first, then reuse that key consistently
- If you already know the exact ID to fix, use `mem_update`

### MemoryLake-backed projects

If the current project uses the **MemoryLake backend** (check with `engram memorylake status`), dedup, updating existing memories, and merging contradictions are handled **automatically** by the backend — you only need `mem_save` / `mem_search` / `mem_context`; you do **not** need to call `mem_update`, `mem_judge`, or `mem_compare` yourself (`mem_save` never returns `judgment_required` there, so the conflict loop below simply won't trigger). Projects on the default **SQLite backend** are unaffected — keep following the topic-key/upsert and conflict-surfacing rules.

## Conflict loop (SQLite projects)

The MCP server instructions point here instead of carrying this walkthrough in
every session. It applies to projects on the default **SQLite backend** only.

### CONFLICT SURFACING — when mem_save returns candidates

After every mem_save call, check the response envelope for judgment_required.

IF judgment_required IS TRUE:
  Iterate candidates[] and call mem_judge once per candidate using the
  judgment_id from that candidate entry. Do NOT use the top-level judgment_id
  for multiple candidates — each candidate has its own.

  HEURISTIC — when to ask the user vs. resolve autonomously:

  ASK the user (conversationally, as part of your next reply) when:
    - confidence is below 0.7, OR
    - the relation you would choose is supersedes or conflicts_with AND the
      observation type is architecture, policy, or decision

  RESOLVE silently (call mem_judge without asking) when:
    - confidence >= 0.7 AND the relation is not supersedes/conflicts_with, OR
    - the relation is related, compatible, scoped, or not_conflict

  HOW TO ASK (conversationally — never via blocking CLI or dashboard prompt):
    Raise it naturally in your next reply to the user. Example phrasing:
    "I noticed memory #abc123 might conflict with what we just saved.
     Want me to mark the new one as superseding it, or are they about
     different scopes? I can also mark them as compatible if both still apply."

  AFTER RESOLUTION (both paths):
    Call mem_judge with the chosen relation, a reason, and if the user gave
    explicit direction, include their words as the evidence field. This persists
    the verdict and closes the pending conflict row.

### Deferred tools (use ToolSearch when needed; the session-start hook preloads several of these — ToolSearch only if a call fails)

  mem_update, mem_review, mem_pin, mem_unpin, mem_suggest_topic_key, mem_session_start, mem_session_end,
  mem_stats, mem_delete, mem_timeline, mem_capture_passive, mem_merge_projects

## SEARCHING — once, up front

One mem_search at task start ("have we seen this before?"). On miss, proceed
normally; do not search repeatedly for the same information.

## WHEN TO SEARCH MEMORY

When the user asks to recall something — any variation of "remember", "recall", "what did we do",
"how did we solve", or the equivalent in the user's language, or references to past work:
1. First call `mem_context` — checks recent session history (fast, cheap)
2. If not found, call `mem_search` with relevant keywords (FTS5 full-text search)
3. If you find a match, use `mem_get_observation` for full untruncated content

Also search memory PROACTIVELY when:
- Starting work on something that might have been done before
- The user mentions a topic you have no context on — check if past sessions covered it
- The user's FIRST message references the project, a feature, or a problem — call `mem_search` with keywords from their message to check for prior work before responding

## SESSION CLOSE PROTOCOL (mandatory)

Before ending a session or saying "done" / "that's it", you MUST:
1. Call `mem_session_summary` with this structure:

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

This is NOT optional. If you skip this, the next session starts blind.

## AFTER COMPACTION

If you see a message about compaction or context reset:
1. IMMEDIATELY call `mem_session_summary` with the compacted summary content — this persists what was done before compaction
2. Then call `mem_context` to recover any additional context from previous sessions
3. Only THEN continue working

Do not skip step 1. Without it, everything done before compaction is lost from memory.
All core tools are loaded automatically by the hook at session start. If they are unexpectedly missing, rerun `engram setup codex` and restart Codex.
