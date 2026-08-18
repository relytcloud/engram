#!/bin/bash
# Engram — per-turn conversation sync for Claude Code (async)
#
# Feeds each completed turn (one user message plus the assistant's final reply)
# into the project's MemoryLake conversation, so MemoryLake's own extraction
# pipeline can mint memories from it without the agent having to remember to
# call mem_save.
#
# This script only moves values; it decides nothing. Whether the project has
# per-turn sync enabled is `engram turn`'s call — the project name must be
# resolved by Go (it reads .engram/config, which detect_project in _helpers.sh
# does not), or a project renamed via config would silently never sync.
#
# Never blocks and never fails a session: missing binary, old binary, broken
# network — all exit 0.

INPUT=$(cat)
command -v engram >/dev/null 2>&1 || exit 0

SESSION_ID=$(echo "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // empty' 2>/dev/null)
CWD=$(echo "$INPUT" | jq -r '.cwd // empty' 2>/dev/null)

[ -n "$SESSION_ID" ] && [ -n "$TRANSCRIPT" ] || exit 0

# No backgrounding needed — hooks.json marks this hook async:true, so Claude
# Code does not wait for it. `|| true` swallows the non-zero an older engram
# returns for an unknown subcommand.
engram turn --session "$SESSION_ID" --transcript "$TRANSCRIPT" --cwd "$CWD" \
  >/dev/null 2>&1 || true

exit 0
