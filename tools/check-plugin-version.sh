#!/usr/bin/env bash
# Guard the plugin manifests' `version` fields against drift.
#
# Why this exists: `claude plugin update` decides whether to refresh a plugin by
# comparing version STRINGS, not content. When the manifests keep an unchanged
# version, the update is a silent no-op that still exits 0 — installed users
# never receive new hooks or skills, and nothing in the install output hints at
# it. That is exactly how the per-turn conversation-sync Stop hook shipped on
# main while every installed plugin stayed frozen at 0.1.1 for 32 commits.
#
# Two layers, matching the two ways this breaks:
#   - no argument  → the three manifests must agree with each other. Catches the
#                    "bumped one file, forgot the other two" case on every PR.
#   - EXPECTED arg → they must additionally equal EXPECTED. Release passes the
#                    tag here, catching the "forgot to bump at all" case.

set -eu

usage() {
  cat <<'USAGE'
Usage: check-plugin-version.sh [EXPECTED]

Verify the plugin manifests declare one consistent version.

  (no argument)   assert the three manifests agree with each other
  EXPECTED        additionally assert they equal EXPECTED (e.g. 0.6.1, or v0.6.1
                  — a leading "v" is stripped, so a release tag works as-is)

Exits non-zero with a diagnostic on any mismatch.
USAGE
}

case "${1-}" in
  -h | --help)
    usage
    exit 0
    ;;
esac

command -v jq >/dev/null 2>&1 || {
  printf 'error: jq is required but not installed\n' >&2
  exit 1
}

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

MARKETPLACE=.claude-plugin/marketplace.json
CLAUDE_CODE=plugin/claude-code/.claude-plugin/plugin.json
CODEX=plugin/codex/.codex-plugin/plugin.json

# The marketplace entry is looked up by name: the file is an array, and keying
# off index 0 would silently follow a reordering.
marketplace_version=$(
  jq -er '.plugins[] | select(.name == "engram") | .version' "$MARKETPLACE"
) || {
  printf 'error: no plugin named "engram" in %s\n' "$MARKETPLACE" >&2
  exit 1
}
claude_code_version=$(jq -er '.version' "$CLAUDE_CODE")
codex_version=$(jq -er '.version' "$CODEX")

fail=0
report() {
  printf 'error: %s\n' "$1" >&2
  fail=1
}

if [ "$claude_code_version" != "$marketplace_version" ] ||
  [ "$codex_version" != "$marketplace_version" ]; then
  report "plugin manifests disagree on version:
  ${MARKETPLACE}  -> ${marketplace_version}
  ${CLAUDE_CODE}  -> ${claude_code_version}
  ${CODEX}  -> ${codex_version}
Bump all three together — a partial bump ships a manifest set that cannot be
reasoned about, and 'claude plugin update' keys off the marketplace entry only."
fi

if [ $# -ge 1 ] && [ -n "$1" ]; then
  expected="${1#v}"
  if [ "$marketplace_version" != "$expected" ]; then
    report "plugin version ${marketplace_version} does not match the expected ${expected}.
The release tag and the plugin manifests must agree: a tag that ships unchanged
manifests leaves every installed user on the previous plugin content, because
'claude plugin update' compares version strings and would report
\"already at the latest version\" while exiting 0.
Fix: bump the three manifests to ${expected}, commit, then re-tag."
  fi
fi

[ "$fail" = 0 ] || exit 1

printf 'plugin version OK: %s (all three manifests agree' "$marketplace_version"
if [ $# -ge 1 ] && [ -n "$1" ]; then
  printf ', matches expected %s' "${1#v}"
fi
printf ')\n'
