#!/bin/bash
# Engram — Post-compaction hook for Claude Code
#
# When compaction happens, inject Memory Protocol + context and instruct
# the agent to persist the compacted summary via mem_session_summary.

ENGRAM_PORT="${ENGRAM_PORT:-7437}"
ENGRAM_URL="http://127.0.0.1:${ENGRAM_PORT}"

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

# Read hook input from stdin
INPUT=$(cat)
SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')
PROJECT=$(detect_project "$CWD")

# Ensure session exists
if [ -n "$SESSION_ID" ] && [ -n "$PROJECT" ]; then
  curl -sf "${ENGRAM_URL}/sessions" \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg id "$SESSION_ID" --arg project "$PROJECT" --arg dir "$CWD" \
      '{id: $id, project: $project, directory: $dir}')" \
    > /dev/null 2>&1
fi

# Fetch context from previous sessions
ENCODED_PROJECT=$(printf '%s' "$PROJECT" | jq -sRr @uri)
CONTEXT=$(curl -sf "${ENGRAM_URL}/context?project=${ENCODED_PROJECT}" --max-time 3 2>/dev/null | jq -r '.context // empty')

# Resolve protocol verbosity mode for this slug. All slim/full branching
# (including the engram-version floor check) lives in Go — see `engram
# protocol-mode`. A missing/old engram binary or an unrecognized subcommand
# never yields "slim" here, so this always defaults safely to full. $mode is
# NEVER echoed/logged to this hook's own stdout.
mode=$(engram protocol-mode claude-code 2>/dev/null)
if [ "$mode" != "slim" ]; then
  mode="full"
fi

# Inject Memory Protocol + compaction instruction + context. Only the static
# protocol prose is gated on $mode — the "CRITICAL INSTRUCTION" header below
# and the numbered recovery steps that follow it stay unconditional (they are
# the compaction-recovery contract itself, not the duplicated protocol text).
if [ "$mode" != "slim" ]; then
cat <<'PROTOCOL'
## Engram Persistent Memory — ACTIVE PROTOCOL

You have engram memory tools. This protocol is MANDATORY and ALWAYS ACTIVE.

### CORE TOOLS — always available, no ToolSearch needed
mem_save, mem_search, mem_context, mem_session_summary, mem_get_observation, mem_save_prompt

Use ToolSearch for other tools: mem_update, mem_suggest_topic_key, mem_session_start, mem_session_end, mem_stats, mem_delete, mem_timeline, mem_capture_passive

### SAVING — silent, batched, never instead of answering
Save decisions, bug root causes, conventions, gotchas, and user preferences.
But: your final reply must contain the complete answer itself — memory serves
future sessions, never this reply. Never narrate saves ("I've saved this to
memory") and never let a save replace the answer. Batch saves at task end.

### SEARCHING — once, up front
One mem_search at task start ("have we seen this before?"). On miss, proceed
normally; do not search repeatedly for the same information.

### MemoryLake-backed projects
If the current project uses the MemoryLake backend (check with `engram memorylake status`), dedup, updating existing memories, and merging contradictions are handled automatically by the backend — you only need mem_save / mem_search / mem_context; you do not need to call mem_update, mem_judge, or mem_compare yourself. Default SQLite projects are unaffected — keep following the protocol above.

### SEARCH MEMORY when:
- User asks to recall anything ("remember", "what did we do", or the equivalent in the user's language)
- Starting work on something that might have been done before
- User mentions a topic you have no context on

### SESSION CLOSE — before saying "done":
Call `mem_session_summary` with: Goal, Discoveries, Accomplished, Next Steps, Relevant Files.

---

PROTOCOL
else
# Slim mode must survive compaction: without this branch the full protocol
# would be re-injected here mid-session, undoing the slim budget. Text is
# byte-identical to the session-start hook's slim heredoc (pinned by
# TestSlimProtocolSurvivesCompaction in plugin/assets_test.go).
cat <<'SLIM_PROTOCOL'
## Engram Memory — Active Protocol (compact)

Persistent memory across sessions. Tools: mem_save, mem_search, mem_context,
mem_session_summary, mem_get_observation, mem_save_prompt (others via ToolSearch).

RULES
1. SAVE decisions, bug root causes, conventions, gotchas, and user
   preferences when they happen — silently: never narrate saves in your
   reply, never let a save substitute for answering. Batch saves at task end.
2. Your final reply must contain the complete answer itself; memory serves
   FUTURE sessions, not this reply.
3. SEARCH once at task start for relevant prior work ("have we seen this
   before?"). On miss, proceed normally — do not search repeatedly.
4. mem_context at session start / after compaction for recent history.
5. Durable facts need topic_key (lowercase-kebab, max 2 levels, e.g.
   architecture/auth-model) — same key updates in place.
6. End of session: mem_session_summary before saying done.
7. MemoryLake-backed projects (check: engram memorylake status): dedup and
   conflict-merge are automatic — mem_save/mem_search/mem_context suffice.
   SQLite projects: on judgment_required, follow the conflict loop in the
   memory SKILL.

Details, examples, and edge cases: load the `memory` SKILL on demand.
SLIM_PROTOCOL
printf '\n---\n\n'
fi

# Unconditional lead-in for the numbered recovery steps below — this is the
# compaction-recovery contract itself, not duplicated protocol prose, so it
# stays outside the $mode gate even when the static protocol text is slim.
echo "CRITICAL INSTRUCTION POST-COMPACTION — follow these steps IN ORDER:"

printf "\n1. FIRST: Call mem_session_summary with the content of the compacted summary above. Use project: '%s'.\n" "$PROJECT"
printf "   This preserves what was accomplished before compaction.\n\n"
printf "2. THEN: Call mem_context with project: '%s' to recover recent session history and observations.\n" "$PROJECT"
printf "   Read the returned context carefully — it tells you what was being worked on.\n\n"
cat <<'PROTOCOL'
3. If you need more detail on a specific topic, call mem_search with relevant keywords.

4. Only THEN continue working on what the user asked.

All 4 steps are MANDATORY. Without them, you lose context and start blind.
PROTOCOL

# Inject memory context if available
if [ -n "$CONTEXT" ]; then
  printf "\n%s\n" "$CONTEXT"
fi

exit 0
