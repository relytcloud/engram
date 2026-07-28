[← Back to README](../README.md)

# Installation

- [Quick install (macOS / Linux)](#quick-install-macos--linux)
- [Windows](#windows)
- [Install from source (macOS / Linux)](#install-from-source-macos--linux)
- [Download binary (all platforms)](#download-binary-all-platforms)
- [Requirements](#requirements)
- [Environment Variables](#environment-variables)
- [Windows Config Paths](#windows-config-paths)

---

## Quick install (macOS / Linux)

One command installs the latest binary AND wires the Claude Code plugin
(this fork ships no Homebrew tap — the tap in older docs belongs to upstream):

```bash
curl -fsSL https://raw.githubusercontent.com/relytcloud/engram/main/install.sh | bash
```

The script auto-discovers the latest release, verifies the sha256 checksum,
installs to `~/.local/bin/engram`, and runs `engram setup claude-code`.
Upgrade = re-run the same command. Options go after `bash -s --`:
`--version X.Y.Z`, `--dir <path>`, `--no-plugin`, `--force`, `--dry-run`,
`--protocol slim`. The script supports macOS/Linux (amd64/arm64) only —
Windows users see below.

---

## Windows

**Option A: Build from source (recommended for technical users)**

If you have Go installed, this is the cleanest and most trustworthy path — the binary is compiled on your machine from source, so no antivirus will flag it.

> Do NOT use `go install github.com/...@latest` for this fork: the Go module
> path still points at the upstream repo, so `go install` would fetch
> upstream code, not this fork. Clone and build instead:

```powershell
git clone https://github.com/relytcloud/engram.git
cd engram
go install ./cmd/engram
# Binary goes to %GOPATH%\bin\engram.exe (typically %USERPROFILE%\go\bin\)
```

Ensure `%GOPATH%\bin` (or `%USERPROFILE%\go\bin`) is on your `PATH`.

> **Want a real version string instead of `dev`?**
>
> `go install` always stamps the binary as `dev`. To get a meaningful version, pick one of these — not both. Running them both leaves two binaries on disk and `engram version` keeps reporting `dev` because PATH still resolves to the `go install` build.
>
> **Option B1 — version-stamped `go install` (binary stays on PATH):**
>
> ```powershell
> $v = git describe --tags --always
> go install -ldflags="-X main.version=local-$v" ./cmd/engram
> ```
>
> **Option B2 — `go build` and move the result onto PATH:**
>
> ```powershell
> $v = git describe --tags --always
> go build -ldflags="-X main.version=local-$v" -o engram.exe ./cmd/engram
> Move-Item -Force engram.exe "$env:USERPROFILE\go\bin\engram.exe"
> ```
>
> After either option, `engram version` should print `local-<git-describe>` instead of `dev`.

**Option C: Download the prebuilt binary**

1. Go to [GitHub Releases](https://github.com/relytcloud/engram/releases)
2. Download `engram_<version>_windows_amd64.zip` (or `arm64` for ARM devices)
3. Extract `engram.exe` to a folder in your `PATH` (e.g. `C:\Users\<you>\bin\`)

```powershell
# Example: extract and add to PATH (PowerShell)
Expand-Archive engram_*_windows_amd64.zip -DestinationPath "$env:USERPROFILE\bin"
# Add to PATH permanently (run once):
[Environment]::SetEnvironmentVariable("Path", "$env:USERPROFILE\bin;" + [Environment]::GetEnvironmentVariable("Path", "User"), "User")
```

> **Antivirus false positives on prebuilt binaries**
>
> Windows Defender and other antivirus tools (ESET, Brave's built-in scanner) have flagged some
> engram prebuilt releases as malware (`Trojan:Script/Wacatac.H!ml` or similar). This is a
> **heuristic false positive**. The binary is built reproducibly from the public source code
> via GoReleaser and contains no malicious code.
>
> **Why does this happen?** Prebuilt binaries from small open-source projects are unsigned (code
> signing certificates cost hundreds of dollars per year). Many AV engines automatically flag
> unsigned executables from unknown publishers, especially recently compiled Go binaries. The
> same alert has been observed on Claude Code's own MSIX installer, which confirms this is an
> AV heuristic issue, not a code problem.
>
> **Maintainer stance:** We will not pay for a code signing certificate at this time. This is a
> distribution trust problem, not a security problem. The source code is fully auditable.
>
> **Recommended workaround:** Technical Windows users should prefer **Option A (`go install`)** or
> **Option B (build from source)**. Binaries you compile locally will not trigger AV alerts because
> they originate from your own machine.

> **Other Windows notes:**
> - Data is stored in `%USERPROFILE%\.engram\engram.db`
> - Override with `ENGRAM_DATA_DIR` environment variable
> - All core features work natively: CLI, MCP server, TUI, HTTP API, Git Sync
> - No WSL required for the core binary — it's a native Windows executable

---

## Install from source (macOS / Linux)

```bash
git clone https://github.com/relytcloud/engram.git
cd engram
go install ./cmd/engram
# Binary goes to $GOPATH/bin (typically ~/go/bin/)
```

> **Want a real version string instead of `dev`?**
>
> `go install` always stamps the binary as `dev`. To get a meaningful version, pick one of these — not both. Running them both leaves two binaries on disk and `engram version` keeps reporting `dev` because PATH still resolves to the `go install` build.
>
> **Option 1 — version-stamped `go install` (binary stays on PATH):**
>
> ```bash
> go install -ldflags="-X main.version=local-$(git describe --tags --always)" ./cmd/engram
> ```
>
> **Option 2 — `go build` and move the result onto PATH:**
>
> ```bash
> go build -ldflags="-X main.version=local-$(git describe --tags --always)" -o engram ./cmd/engram
> mv engram "$(go env GOPATH)/bin/engram"
> ```
>
> After either option, `engram version` should print `local-<git-describe>` instead of `dev`.

---

## Download binary (all platforms)

Grab the latest release for your platform from [GitHub Releases](https://github.com/relytcloud/engram/releases).

| Platform | File |
|----------|------|
| macOS (Apple Silicon) | `engram_<version>_darwin_arm64.tar.gz` |
| macOS (Intel) | `engram_<version>_darwin_amd64.tar.gz` |
| Linux (x86_64) | `engram_<version>_linux_amd64.tar.gz` |
| Linux (ARM64) | `engram_<version>_linux_arm64.tar.gz` |
| Windows (x86_64) | `engram_<version>_windows_amd64.zip` |
| Windows (ARM64) | `engram_<version>_windows_arm64.zip` |

---

## Requirements

- **Go 1.25+** to build from source (not needed if using the quick-install script or downloading a binary)
- That's it. No runtime dependencies.

The binary includes SQLite (via [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — pure Go, no CGO). Works natively on **macOS**, **Linux**, and **Windows** (x86_64 and ARM64).

---

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `ENGRAM_DATA_DIR` | Data directory | `~/.engram` (Windows: `%USERPROFILE%\.engram`) |
| `ENGRAM_PORT` | HTTP server port | `7437` |

---

## Windows Config Paths

When using `engram setup`, config files are written to platform-appropriate locations:

| Agent | macOS / Linux | Windows |
|-------|---------------|---------|
| OpenCode | `~/.config/opencode/` | `%APPDATA%\opencode\` |
| Gemini CLI | `~/.gemini/` | `%APPDATA%\gemini\` |
| Codex | `~/.codex/` | `%APPDATA%\codex\` |
| Claude Code | Managed by `claude` CLI | Managed by `claude` CLI |
| Antigravity CLI | `~/.gemini/config/mcp_config.json` + `~/.gemini/GEMINI.md` | `%APPDATA%\gemini\config\mcp_config.json` + `%APPDATA%\gemini\GEMINI.md` |
| Windsurf | `~/.codeium/windsurf/mcp_config.json` + `.../memories/global_rules.md` | `%USERPROFILE%\.codeium\windsurf\...` |
| Qwen Code | `~/.qwen/settings.json` + `~/.qwen/QWEN.md` | `%USERPROFILE%\.qwen\...` |
| Kiro | `~/.kiro/settings/mcp.json` + `~/.kiro/steering/engram.md` | `%USERPROFILE%\.kiro\...` |
| Cursor | `~/.cursor/mcp.json` + `~/.cursor/rules/engram.mdc` | `%USERPROFILE%\.cursor\...` |
| VS Code Copilot | `~/.config/Code/User/mcp.json` + `.../prompts/engram.instructions.md` (macOS: `~/Library/Application Support/Code/User/`) | `%APPDATA%\Code\User\...` |
| Kilo Code | `~/.config/kilo/opencode.json` + `~/.config/kilo/AGENTS.md` | `%USERPROFILE%\.config\kilo\...` |
| Data directory | `~/.engram/` | `%USERPROFILE%\.engram\` |
