#!/bin/bash
# Engram — SessionStart hook for Claude Code
#
# 1. Ensures the engram server is running
# 2. Creates a session in engram
# 3. Auto-imports git-synced chunks if .engram/manifest.json exists
# 4. Injects Memory Protocol instructions + memory context

ENGRAM_PORT="${ENGRAM_PORT:-7437}"
ENGRAM_URL="http://127.0.0.1:${ENGRAM_PORT}"
IMPORT_TIMEOUT_SECS=8
LOCK_TTL_SECS=$((IMPORT_TIMEOUT_SECS + 4))
LOCK_METADATA_STALE_SECS=$((LOCK_TTL_SECS * 5))

# Load shared helpers
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "${SCRIPT_DIR}/_helpers.sh"

# Read hook input from stdin
INPUT=$(cat)
SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')
OLD_PROJECT=$(basename "$CWD" | tr '[:upper:]' '[:lower:]')
PROJECT=$(detect_project "$CWD")

# Ensure engram server is running
if ! curl -sf "${ENGRAM_URL}/health" --max-time 1 > /dev/null 2>&1; then
  engram serve &>/dev/null &
  sleep 0.5
fi

# Migrate project name if it changed (one-time, idempotent)
if [ "$OLD_PROJECT" != "$PROJECT" ] && [ -n "$OLD_PROJECT" ] && [ -n "$PROJECT" ]; then
  curl -sf "${ENGRAM_URL}/projects/migrate" \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg old "$OLD_PROJECT" --arg new "$PROJECT" \
      '{old_project: $old, new_project: $new}')" \
    > /dev/null 2>&1
fi

# Create session
if [ -n "$SESSION_ID" ] && [ -n "$PROJECT" ]; then
  curl -sf "${ENGRAM_URL}/sessions" \
    -X POST \
    -H "Content-Type: application/json" \
    -d "$(jq -n --arg id "$SESSION_ID" --arg project "$PROJECT" --arg dir "$CWD" \
      '{id: $id, project: $project, directory: $dir}')" \
    > /dev/null 2>&1
fi

# Auto-import git-synced chunks
if [ -f "${CWD}/.engram/manifest.json" ]; then
  (
    cd "$CWD" 2>/dev/null || exit 0
    IMPORT_LOCK="/tmp/engram-sync-import-$(printf '%s' "$CWD" | cksum | cut -d ' ' -f 1).lock"
    write_import_lock_info() {
      LOCK_INFO_TMP="$IMPORT_LOCK/info.$$"
      printf '%s %s\n' "$LOCK_PID" "$LOCK_NOW" > "$LOCK_INFO_TMP" 2>/dev/null \
        && mv "$LOCK_INFO_TMP" "$IMPORT_LOCK/info" 2>/dev/null
    }
    lock_path_age_secs() {
      LOCK_PATH=$1
      LOCK_MTIME=""
      if LOCK_MTIME=$(stat -f %m "$LOCK_PATH" 2>/dev/null); then
        :
      elif LOCK_MTIME=$(stat -c %Y "$LOCK_PATH" 2>/dev/null); then
        :
      else
        return 1
      fi
      case "$LOCK_MTIME" in
        ''|*[!0-9]*) return 1 ;;
      esac
      printf '%s\n' $(( LOCK_NOW - LOCK_MTIME ))
    }
    lock_metadata_is_stale() {
      LOCK_METADATA_PATH=$1
      LOCK_METADATA_AGE=$(lock_path_age_secs "$LOCK_METADATA_PATH") || return 1
      [ "$LOCK_METADATA_AGE" -gt "$LOCK_METADATA_STALE_SECS" ]
    }
    acquire_import_lock() {
      LOCK_PID="${BASHPID:-$$}"
      LOCK_NOW=$(date +%s)
      if mkdir "$IMPORT_LOCK" 2>/dev/null; then
        write_import_lock_info || true
        return 0
      fi

      LOCK_INFO=""
      if [ -f "$IMPORT_LOCK/info" ]; then
        LOCK_INFO=$(cat "$IMPORT_LOCK/info" 2>/dev/null || true)
      fi
      read -r OLD_PID OLD_EPOCH LOCK_INFO_EXTRA <<< "$LOCK_INFO"
      STALE_LOCK=0
      if [ -z "$LOCK_INFO" ] || [ -z "$OLD_PID" ] || [ -z "$OLD_EPOCH" ] || [ -n "$LOCK_INFO_EXTRA" ]; then
        lock_metadata_is_stale "$IMPORT_LOCK" || return 1
        STALE_LOCK=1
      elif [[ "$OLD_PID" == *[!0-9]* ]] || [[ "$OLD_EPOCH" == *[!0-9]* ]]; then
        lock_metadata_is_stale "$IMPORT_LOCK/info" || return 1
        STALE_LOCK=1
      elif kill -0 "$OLD_PID" 2>/dev/null; then
        return 1
      elif [ $(( LOCK_NOW - OLD_EPOCH )) -gt "$LOCK_TTL_SECS" ]; then
        STALE_LOCK=1
      fi

      if [ "$STALE_LOCK" -ne 1 ]; then
        return 1
      fi
      rm -f "$IMPORT_LOCK/info" 2>/dev/null || true
      rmdir "$IMPORT_LOCK" 2>/dev/null || return 1
      if mkdir "$IMPORT_LOCK" 2>/dev/null; then
        write_import_lock_info || true
        return 0
      fi
      return 1
    }
    if ! acquire_import_lock; then
      exit 0
    fi
    trap 'rm -f "$IMPORT_LOCK/info" 2>/dev/null || true; rmdir "$IMPORT_LOCK" 2>/dev/null || true' EXIT
    if command -v timeout >/dev/null 2>&1; then
      timeout "${IMPORT_TIMEOUT_SECS}s" engram sync --import >/dev/null 2>&1 || true
    else
      engram sync --import >/dev/null 2>&1 &
      IMPORT_PID=$!
      (sleep "$IMPORT_TIMEOUT_SECS"; kill "$IMPORT_PID" 2>/dev/null || true) &
      WAITER_PID=$!
      wait "$IMPORT_PID" 2>/dev/null || true
      kill "$WAITER_PID" 2>/dev/null || true
    fi
  ) >/dev/null 2>&1 &
fi

# Fetch memory context
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

# Inject Memory Protocol + context — stdout goes to Claude as additionalContext
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
- User's FIRST message references the project, a feature, or a problem — call `mem_search` with keywords from their message to check for prior work before responding

### SESSION CLOSE — before saying "done":
Call `mem_session_summary` with: Goal, Discoveries, Accomplished, Next Steps, Relevant Files.
PROTOCOL
else
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
7. Pin (mem_pin) only facts whose loss causes irreversible damage or repeated pitfalls; pinned facts are always in context.
8. MemoryLake-backed projects (check: engram memorylake status): dedup and
   conflict-merge are automatic — mem_save/mem_search/mem_context suffice.
   SQLite projects: on judgment_required, follow the conflict loop in the
   memory SKILL.

Details, examples, and edge cases: load the `memory` SKILL on demand.
SLIM_PROTOCOL
fi

# Inject memory context if available
if [ -n "$CONTEXT" ]; then
  printf "\n%s\n" "$CONTEXT"
fi

exit 0
