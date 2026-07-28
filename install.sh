#!/usr/bin/env bash
# engram installer/upgrader — binary + Claude Code plugin, in one command.
#
#   curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh \
#     | bash -s -- --dir /usr/local/bin --no-plugin
#
# What it does:
#   1. discovers the LATEST release automatically (no hardcoded version),
#   2. downloads the right archive for your OS/arch and verifies its sha256,
#   3. installs the binary (default: ~/.local/bin/engram),
#   4. wires the Claude Code plugin (marketplace + plugin + MCP config) and
#      refreshes plugin assets on upgrades.
#
# Flags (pass after `bash -s --` when piping):
#   --version <X.Y.Z>       pin a specific release instead of the latest
#   --dir <path>            install directory        (default: ~/.local/bin)
#   --no-plugin             skip the Claude Code plugin step
#   --force                 reinstall even if the same version is present
#   --no-verify             skip sha256 verification (not recommended)
#   --protocol <slim|full>  set the memory-protocol verbosity during setup
#   --replace-marketplace   replace a foreign 'engram' plugin marketplace
#   --yes                   never prompt (headless; allowlist step declined)
#   --dry-run               print what would happen, change nothing
#   --help                  this text
#
# Env equivalents: ENGRAM_VERSION, ENGRAM_INSTALL_DIR, ENGRAM_NO_PLUGIN=1,
#   ENGRAM_INSTALL_NO_VERIFY=1, ENGRAM_INSTALL_YES=1, GH_TOKEN/GITHUB_TOKEN
#   (for the API fallback), NO_COLOR. Flags win over env.
#
# NOTE (curl|bash): bash reads THIS SCRIPT from fd 0 while executing it.
# Never `exec 0</dev/null` at the top and never let a child inherit stdin —
# either truncates the script or lets the child eat script bytes. Redirect
# stdin PER COMMAND, and keep `main "$@"` as the last line so bash parses
# the entire file before running anything (a truncated download then fails
# to parse instead of running half an install).
set -euo pipefail

REPO="relytcloud/engram"
RELEASES="https://github.com/${REPO}/releases"
API_LATEST="https://api.github.com/repos/${REPO}/releases/latest"
# Must match .claude-plugin/marketplace.json's owner/repo.
MARKETPLACE="relytcloud/engram"

VERSION="${ENGRAM_VERSION:-}" # empty = auto-discover latest
INSTALL_DIR="${ENGRAM_INSTALL_DIR:-$HOME/.local/bin}"
WITH_PLUGIN=1
[ "${ENGRAM_NO_PLUGIN:-0}" = "1" ] && WITH_PLUGIN=0
VERIFY=1
[ "${ENGRAM_INSTALL_NO_VERIFY:-0}" = "1" ] && VERIFY=0
ASSUME_YES=0
[ "${ENGRAM_INSTALL_YES:-0}" = "1" ] && ASSUME_YES=1
FORCE=0
DRY_RUN=0
REPLACE_MARKETPLACE=0
PROTOCOL=""
WORKDIR=""

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
  BOLD=$'\033[1m' RED=$'\033[31m' YELLOW=$'\033[33m' RESET=$'\033[0m'
else
  BOLD="" RED="" YELLOW="" RESET=""
fi

info() { printf '%s\n' "$*" >&2; }
step() { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*" >&2; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die() {
  printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2
  exit 1
}

usage() {
  # The flag reference lives in the header comment; print that block.
  sed -n '2,32p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' >&2 ||
    info "engram installer — see https://github.com/${REPO}#install"
}

cleanup() { [ -n "$WORKDIR" ] && rm -rf "$WORKDIR"; }

make_workdir() {
  WORKDIR=$(mktemp -d 2>/dev/null || mktemp -d -t engram-install) ||
    die "could not create a temporary directory"
  trap cleanup EXIT
  trap 'cleanup; exit 130' INT
  trap 'cleanup; exit 143' TERM
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)
        [ $# -ge 2 ] || die "--version requires a value (e.g. --version 0.4.0)"
        VERSION="$2"
        shift 2
        ;;
      --dir)
        [ $# -ge 2 ] || die "--dir requires a path"
        INSTALL_DIR="$2"
        shift 2
        ;;
      --protocol)
        [ $# -ge 2 ] || die "--protocol requires slim or full"
        case "$2" in
          slim | full) PROTOCOL="$2" ;;
          *) die "--protocol must be slim or full, got: $2" ;;
        esac
        shift 2
        ;;
      --no-plugin) WITH_PLUGIN=0; shift ;;
      --force) FORCE=1; shift ;;
      --no-verify) VERIFY=0; shift ;;
      --replace-marketplace) REPLACE_MARKETPLACE=1; shift ;;
      --yes | -y) ASSUME_YES=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      --help | -h)
        usage
        exit 0
        ;;
      *)
        usage
        die "unknown flag: $1"
        ;;
    esac
  done
}

require_curl() {
  command -v curl >/dev/null 2>&1 ||
    die "curl is required (this installer is curl-based by design)"
}

detect_platform() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$os" in
    linux | darwin) ;;
    mingw* | msys* | cygwin* | windows*)
      die "Windows is not supported by this script — see https://github.com/${REPO}/blob/main/docs/INSTALLATION.md#windows"
      ;;
    *) die "unsupported OS: ${os} (engram ships linux and darwin builds)" ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture: ${arch} (engram ships amd64 and arm64)" ;;
  esac
  # An x86_64 shell under Rosetta on Apple Silicon should get the native build.
  if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
    [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
    arch=arm64
  fi
  printf '%s_%s\n' "$os" "$arch"
}

latest_version() {
  local url tag token json
  # Primary: follow the /releases/latest redirect — no API rate limit.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    --proto '=https' --tlsv1.2 --retry 2 --retry-delay 1 --max-time 20 \
    "${RELEASES}/latest" 2>/dev/null) || url=""
  tag=${url##*/tag/}
  if [ -n "$url" ] && [ "$tag" != "$url" ] && [ -n "$tag" ]; then
    printf '%s\n' "${tag#v}"
    return 0
  fi
  # Fallback: GitHub API (rate-limited unauthenticated; honors GH_TOKEN).
  # Two branches instead of a conditional array: empty-array expansion under
  # `set -u` breaks on macOS bash 3.2.
  token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
  if [ -n "$token" ]; then
    json=$(curl -fsSL --proto '=https' --max-time 20 \
      -H 'Accept: application/vnd.github+json' \
      -H "Authorization: Bearer ${token}" "$API_LATEST" 2>/dev/null) || json=""
  else
    json=$(curl -fsSL --proto '=https' --max-time 20 \
      -H 'Accept: application/vnd.github+json' "$API_LATEST" 2>/dev/null) || json=""
  fi
  tag=$(printf '%s' "$json" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$tag" ] || die "could not determine the latest engram version.
  Both discovery paths failed (release redirect + GitHub API). Likely causes:
  no network, an HTTPS proxy that blocks redirects, or API rate limiting
  (set GH_TOKEN to authenticate the fallback).
  Workaround: pin a version explicitly, e.g.  --version 0.4.0
  Releases: ${RELEASES}"
  printf '%s\n' "${tag#v}"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    return 1
  fi
}

# $1=archive path  $2=asset name  $3=checksums.txt path
verify_checksum() {
  local expected actual
  # goreleaser writes "<sha256>  <asset>". awk exact-match is portable;
  # `--ignore-missing` is not (BSD shasum lacks it).
  expected=$(awk -v f="$2" '$2 == f { print $1 }' "$3")
  [ -n "$expected" ] || die "no checksum entry for ${2} in checksums.txt"
  actual=$(sha256_of "$1") ||
    die "no sha256 tool found (sha256sum / shasum / openssl); re-run with --no-verify to skip verification"
  [ "$expected" = "$actual" ] && return 0
  die "checksum mismatch for ${2}
  expected ${expected}
  actual   ${actual}
  Refusing to install a corrupted download. Retry; if this persists, open an
  issue: https://github.com/${REPO}/issues"
}

# Prints the installed version of $1, or "" when absent/not runnable.
installed_version() {
  local bin="$1"
  if [ -z "$bin" ] || [ ! -x "$bin" ]; then
    printf ''
    return 0
  fi
  # `engram version` may print an update notice on stderr first — drop it,
  # and never let the child touch our stdin.
  "$bin" version 2>/dev/null </dev/null | awk 'NR==1 {print $2}' || printf ''
}

# $1=version  $2=platform  → prints the installed binary path
install_binary() {
  local ver="$1" plat="$2" asset base target
  asset="engram_${ver}_${plat}.tar.gz"
  base="${RELEASES}/download/v${ver}"
  target="${INSTALL_DIR}/engram"

  step "downloading ${asset}"
  curl -fL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 1 --max-time 300 \
    -o "${WORKDIR}/${asset}" "${base}/${asset}" ||
    die "download failed: ${base}/${asset}
  Does v${ver} exist and ship a ${plat} asset?  ${RELEASES}"

  if [ "$VERIFY" = 1 ]; then
    step "verifying sha256"
    curl -fsSL --proto '=https' --max-time 60 \
      -o "${WORKDIR}/checksums.txt" "${base}/checksums.txt" ||
      die "could not download checksums.txt (use --no-verify to skip verification)"
    verify_checksum "${WORKDIR}/${asset}" "$asset" "${WORKDIR}/checksums.txt"
  else
    warn "checksum verification disabled (--no-verify)"
  fi

  tar -xzf "${WORKDIR}/${asset}" -C "$WORKDIR" engram ||
    die "could not extract 'engram' from ${asset}"

  mkdir -p "$INSTALL_DIR" 2>/dev/null ||
    die "cannot create ${INSTALL_DIR} — pass --dir <writable path>"
  [ -w "$INSTALL_DIR" ] || die "${INSTALL_DIR} is not writable.
  Either pick a user directory:  --dir \"\$HOME/.local/bin\"
  or install system-wide:        curl -fsSL <url> | sudo bash -s -- --dir /usr/local/bin"

  # install(1) replaces the inode, so a running `engram serve` keeps its old
  # image safely.
  if ! install -m 0755 "${WORKDIR}/engram" "$target" 2>/dev/null; then
    if ! cp "${WORKDIR}/engram" "$target" || ! chmod 0755 "$target"; then
      die "could not install to ${target}"
    fi
  fi

  # macOS binaries are ad-hoc signed, not notarized — strip quarantine.
  if [ "$(uname -s)" = Darwin ] && command -v xattr >/dev/null 2>&1; then
    xattr -d com.apple.quarantine "$target" >/dev/null 2>&1 || true
  fi
  printf '%s\n' "$target"
}

tty_available() { [ -e /dev/tty ] && (exec 3</dev/tty) 2>/dev/null; }

# y/N on the real terminal; defaults to N when headless or --yes.
confirm() {
  local answer=""
  # --yes must never auto-approve destroying someone else's marketplace.
  [ "$ASSUME_YES" = 1 ] && return 1
  tty_available || return 1
  printf '%s [y/N] ' "$1" >&2
  read -r answer </dev/tty || return 1
  case "$answer" in y | Y | yes | YES) return 0 ;; *) return 1 ;; esac
}

# $1=marketplace name → prints "owner/repo" of its GitHub source, or "".
claude_marketplace_source() {
  # `claude` colorizes tty output; strip ANSI with a bash $'' ESC literal
  # (GNU sed's \x1b escape is not portable to BSD sed).
  NO_COLOR=1 claude plugin marketplace list </dev/null 2>/dev/null |
    sed $'s/\033\\[[0-9;]*[A-Za-z]//g' |
    awk -v want="$1" '
      /Source:/ {
        if (found && match($0, /\(([^)]+)\)/)) {
          print substr($0, RSTART + 1, RLENGTH - 2)
          exit
        }
        next
      }
      {
        line = $0
        sub(/^[^A-Za-z0-9_.-]*/, "", line)
        sub(/[[:space:]]+$/, "", line)
        if (line == want) found = 1
      }'
}

# $1 = absolute path of the just-installed binary
setup_claude_code() {
  local bin="$1" src
  if ! command -v claude >/dev/null 2>&1; then
    warn "claude CLI not found — skipping the Claude Code plugin step.
  Install Claude Code first: https://docs.anthropic.com/en/docs/claude-code
  Then wire the plugin with:   ${bin} setup claude-code"
    return 0
  fi

  src=$(claude_marketplace_source engram || true)
  if [ -n "$src" ] &&
    [ "$(printf '%s' "$src" | tr '[:upper:]' '[:lower:]')" != "$(printf '%s' "$MARKETPLACE" | tr '[:upper:]' '[:lower:]')" ]; then
    if [ "$REPLACE_MARKETPLACE" = 1 ] ||
      confirm "Plugin marketplace 'engram' currently points at ${src}. Replace it with ${MARKETPLACE}?"; then
      step "replacing the 'engram' marketplace (${src} -> ${MARKETPLACE})"
      claude plugin marketplace remove engram </dev/null >/dev/null 2>&1 ||
        warn "could not remove the existing 'engram' marketplace"
    else
      warn "plugin step skipped — a different 'engram' marketplace (${src}) is configured.
  To switch to this fork's plugin, run:
      claude plugin marketplace remove engram
      ${bin} setup claude-code
  (or re-run this installer with --replace-marketplace)"
      return 0
    fi
  fi

  step "wiring the Claude Code plugin (marketplace + plugin + MCP config)"
  # Pass ONLY known args: an unknown flag makes `engram setup` fall back to
  # its INTERACTIVE menu, which reads stdin (fatal under curl|bash).
  set -- setup claude-code
  [ -n "$PROTOCOL" ] && set -- "$@" "--protocol=${PROTOCOL}"

  if [ -t 0 ]; then
    "$bin" "$@" ||
      warn "engram setup claude-code failed — re-run it manually to finish plugin setup"
  elif [ "$ASSUME_YES" = 0 ] && tty_available; then
    # Under curl|bash stdin is the script pipe; borrow the real terminal so
    # the allowlist (y/N) prompt still works and no script bytes are eaten.
    "$bin" "$@" </dev/tty ||
      warn "engram setup claude-code failed — re-run it manually to finish plugin setup"
  else
    # /dev/null stdin: the allowlist prompt reads EOF and takes its
    # "Skipped" branch cleanly (verified against cmd/engram main.go).
    "$bin" "$@" </dev/null ||
      warn "engram setup claude-code failed — re-run it manually to finish plugin setup"
    info "  note: the settings.json allowlist prompt was auto-declined (non-interactive run)."
    info "        To allow engram's mem_* tools without prompts, run in a terminal:"
    info "            ${bin} setup claude-code"
  fi

  # `claude plugin install` is a no-op when the plugin already exists, so it
  # does NOT refresh hooks/skills on upgrade — the update pair below does.
  # Best-effort: failures here never fail the install.
  claude plugin marketplace update engram </dev/null >/dev/null 2>&1 ||
    warn "could not refresh the engram plugin marketplace (will refresh on next Claude Code restart)"
  claude plugin update engram@engram </dev/null >/dev/null 2>&1 || true
  info "  plugin wired. Restart Claude Code to load the updated plugin."
}

# $1 = installed target path
report_path() {
  local target="$1" resolved
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *)
      warn "${INSTALL_DIR} is not on your PATH. Add it:"
      case "$(basename "${SHELL:-sh}")" in
        zsh) info "    echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.zshrc && exec zsh" ;;
        bash)
          info "    echo 'export PATH=\"${INSTALL_DIR}:\$PATH\"' >> ~/.bashrc && exec bash"
          [ "$(uname -s)" = Darwin ] &&
            info "    (macOS login shells may read ~/.bash_profile instead)"
          ;;
        fish) info "    fish_add_path ${INSTALL_DIR}" ;;
        *) info "    export PATH=\"${INSTALL_DIR}:\$PATH\"   # add to your shell's rc file" ;;
      esac
      ;;
  esac
  resolved=$(command -v engram 2>/dev/null || true)
  if [ -n "$resolved" ] && [ "$resolved" != "$target" ]; then
    warn "another engram is earlier in PATH: ${resolved} (version: $(installed_version "$resolved"))
  ${target} was just installed. Remove the older copy or reorder PATH, then
  re-run '${target} setup claude-code' so the MCP config points at the right binary."
  fi
}

main() {
  parse_args "$@"
  require_curl

  local plat target have ver
  plat=$(detect_platform)
  target="${INSTALL_DIR}/engram"
  have=$(installed_version "$target")
  [ -z "$have" ] && have=$(installed_version "$(command -v engram 2>/dev/null || true)")

  ver="${VERSION#v}"
  if [ -z "$ver" ]; then
    step "resolving the latest release"
    ver=$(latest_version)
  fi

  if [ "$DRY_RUN" = 1 ]; then
    info "dry run — nothing will be changed"
    info "  platform:   ${plat}"
    info "  version:    ${ver}$([ -n "$have" ] && printf ' (installed: %s)' "$have")"
    info "  asset:      ${RELEASES}/download/v${ver}/engram_${ver}_${plat}.tar.gz"
    info "  install to: ${target}"
    info "  plugin:     $([ "$WITH_PLUGIN" = 1 ] && printf 'yes (Claude Code)' || printf 'no (--no-plugin)')"
    exit 0
  fi

  make_workdir

  if [ -n "$have" ] && [ "$have" = "$ver" ] && [ "$FORCE" != 1 ]; then
    info "engram ${ver} is already installed at ${target} — skipping download (--force to reinstall)"
  else
    if [ -n "$have" ]; then
      step "upgrading engram ${have} -> ${ver}"
    else
      step "installing engram ${ver}"
    fi
    target=$(install_binary "$ver" "$plat")
  fi

  "$target" version </dev/null >/dev/null 2>&1 ||
    die "the installed binary does not run.
  macOS Gatekeeper? Try:  xattr -d com.apple.quarantine ${target}
  Otherwise see ${RELEASES}"

  if [ "$WITH_PLUGIN" = 1 ]; then
    setup_claude_code "$target"
  else
    info "plugin step skipped (--no-plugin)"
  fi

  report_path "$target"

  step "done — engram $("$target" version </dev/null 2>/dev/null | awk '{print $2}') at ${target}"
  info "  next steps:"
  info "    restart Claude Code (loads the plugin + MCP tools)"
  info "    engram version        # verify"
  info "    engram doctor         # health check"
}

main "$@"
