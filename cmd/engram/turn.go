package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/turncapture"
)

// turnUsageExitCode is the only non-zero exit `engram turn` ever produces, and
// only for a malformed invocation. Every runtime outcome — safety valve
// engaged, project not enabled, transcript missing, network down — exits 0:
// this command runs from a Stop hook after every single turn, and a non-zero
// exit there is noise the user can do nothing about. A usage error, by
// contrast, only happens when a human typed the command, and silently
// succeeding on a typo'd flag wastes their afternoon.
const turnUsageExitCode = 2

// defaultTurnMaxBytes caps a single merged turn message. 32 KiB is generous for
// prose while keeping any one turn from dominating a conversation's extraction
// budget.
const defaultTurnMaxBytes = 32768

func printTurnUsage() {
	fmt.Fprintln(os.Stderr, "usage: engram turn --transcript <path> [--session <id>] [--cwd <dir>] [--verbose]")
}

// cmdTurn appends the transcript's last completed turn to the project's
// MemoryLake conversation, when that project has per-turn conversation sync
// enabled. Invoked by the Claude Code Stop hook once per turn; see
// docs/superpowers/specs/2026-08-06-memorylake-turn-sync-design.md.
func cmdTurn() {
	sessionID, transcript, cwd, verbose, ok := parseTurnArgs()
	if !ok {
		return
	}

	// Global safety valve: honored before anything else so it is impossible for
	// this path to reach MemoryLake while the valve is closed.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("ENGRAM_BACKEND")), "sqlite") {
		return
	}

	project := detectProject(cwd)

	enab, err := loadMemorylakeEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("load enablement: %w", err))
		return
	}
	entry, enabled := enab.IsEnabled(project)
	if !enabled || !entry.SyncConversations {
		// The overwhelmingly common case. Nothing has been opened, nothing has
		// been dialed, nothing has been logged.
		return
	}
	if entry.ProjID == "" {
		// Enabled by an older engram that never persisted the resolved project
		// id. Resolving it here would mean creating a MemoryLake project from a
		// fire-and-forget hook; let the next mem_save do it instead.
		return
	}

	turn, err := turncapture.LastTurn(transcript)
	if err != nil {
		logTurnFailure(project, sessionID, err)
		return
	}
	if sessionID == "" {
		sessionID = turn.SessionID
	}
	if sessionID == "" {
		return
	}

	content, mergeOK := turn.Merged(turnMaxBytes())
	if !mergeOK {
		// An interrupted or tool-only turn. Routine, so not logged.
		return
	}

	mlCfg := loadMemorylakeConfig()
	backend, err := memorylake.NewBackend(mlCfg, mlCfg.Workspace, entry.ProjID)
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("construct backend: %w", err))
		return
	}

	msgID, err := backend.AppendTurn(sessionID, content)
	if err != nil {
		logTurnFailure(project, sessionID, fmt.Errorf("append turn: %w", err))
		return
	}

	if verbose {
		fmt.Printf("appended turn to conversation %s (message %s, %d bytes)\n", sessionID, msgID, len(content))
	}
}

// parseTurnArgs reads the flags by hand, matching every other subcommand in
// this binary (there is no flag framework here). ok is false when the caller
// should return immediately because exitFunc has already been invoked.
func parseTurnArgs() (sessionID, transcript, cwd string, verbose, ok bool) {
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--session":
			if i+1 < len(os.Args) {
				sessionID = os.Args[i+1]
				i++
			}
		case "--transcript":
			if i+1 < len(os.Args) {
				transcript = os.Args[i+1]
				i++
			}
		case "--cwd":
			if i+1 < len(os.Args) {
				cwd = os.Args[i+1]
				i++
			}
		case "--verbose":
			verbose = true
		default:
			fmt.Fprintf(os.Stderr, "engram: unknown flag %q\n", os.Args[i])
			printTurnUsage()
			exitFunc(turnUsageExitCode)
			return "", "", "", false, false
		}
	}

	if transcript == "" {
		fmt.Fprintln(os.Stderr, "engram: --transcript <path> is required")
		printTurnUsage()
		exitFunc(turnUsageExitCode)
		return "", "", "", false, false
	}
	return sessionID, transcript, cwd, verbose, true
}

// turnMaxBytes reads the merged-message ceiling from the environment, tolerating
// garbage the same way internal/memorylake's config does.
func turnMaxBytes() int {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_TURN_MAX_BYTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTurnMaxBytes
}

// logTurnFailure appends one line to ~/.engram/logs/turn.log.
//
// Per-turn sync is fire-and-forget: a failure is never retried and never
// reaches the terminal (a Stop hook's stderr can flash in some terminals), so
// this file is its only trace. Everything here is best-effort — a logging
// failure is swallowed, because failing to log must not become a second
// failure mode.
func logTurnFailure(project, sessionID string, cause error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".engram", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "turn.log")

	// Diagnostic log, not an audit log: past 1 MiB start over rather than
	// growing without bound or managing rotated files.
	if st, statErr := os.Stat(path); statErr == nil && st.Size() > 1<<20 {
		_ = os.Remove(path)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "%s project=%s session=%s error=%v\n",
		time.Now().UTC().Format(time.RFC3339), project, sessionID, cause)
}
