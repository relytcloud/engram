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

// defaultTurnRequestTimeoutMS caps how long any single MemoryLake round trip
// (workspace resolution, actor binding, conversation create, message post) may
// take on the `engram turn` path specifically. The general
// ENGRAM_MEMORYLAKE_TIMEOUT_MS default (30s, see internal/memorylake/config.go)
// is sized for interactive `mem_save` calls; a Stop hook firing after every
// single turn cannot afford to wait that long per request when MemoryLake is
// slow or half-dead, so this path clamps down to a tighter ceiling regardless
// of what is configured globally.
const defaultTurnRequestTimeoutMS = 10000

// defaultTurnWatchdogMS bounds the wall-clock lifetime of the whole `engram
// turn` process. NewBackend and AppendTurn together are ~4 sequential HTTP
// round trips; even with defaultTurnRequestTimeoutMS clamping each one, a
// pathological string of near-timeout requests could still run for minutes.
// Because Task 8 wires this command to fire after every completed turn, a
// slow or unreachable MemoryLake could otherwise leave dozens of these
// processes alive across a long session. The watchdog is a hard backstop:
// once it fires the process exits 0 (not 1) because, per this command's exit
// contract, a watchdog timeout is a runtime outcome like any other — network
// failure, missing transcript, disabled project — none of which may surface
// as a non-zero exit to the hook that invoked it. See turnWatchdogFire for
// what happens when it actually fires, and cmdTurn for where it is armed.
const defaultTurnWatchdogMS = 45000

// turnWatchdogFire is the watchdog's callback, extracted into a named
// function so it can be exercised directly in a test rather than contorting a
// test around a real timer. Firing means cmdTurn was still running after
// elapsed had passed — almost always a slow or hung MemoryLake round trip —
// and without this, that outcome would leave nothing behind: turn.log is this
// feature's only diagnostic channel, and a watchdog kill is exactly the
// situation a user would most want to be able to diagnose. So the failure is
// logged first, and only then does the process exit — 0, not 1, because a
// watchdog timeout is a runtime outcome like any other (see
// defaultTurnWatchdogMS), not a usage error.
func turnWatchdogFire(project, sessionID string, elapsed time.Duration) {
	logTurnFailure(project, sessionID, fmt.Errorf("watchdog fired after %s: turn upload aborted", elapsed))
	exitFunc(0)
}

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

	// Read the enablement file before doing anything project-specific.
	// detectProject shells out to git (up to twice, internal/project/detect.go),
	// and this command runs after every single turn for every user — including
	// the overwhelming majority who never enabled MemoryLake at all. Spending
	// those forks before knowing whether *any* project has the switch on would
	// defeat the whole point of this being a cheap no-op on the common path.
	enab, err := loadMemorylakeEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		logTurnFailure("", sessionID, fmt.Errorf("load enablement: %w", err))
		return
	}
	if !anyConversationSyncEnabled(enab) {
		// The overwhelmingly common case. No project has ever asked for this,
		// so nothing has been detected, opened, dialed, or logged.
		return
	}

	project := detectProject(cwd)

	entry, enabled := enab.IsEnabled(project)
	if !enabled || !entry.SyncConversations {
		// Some other project has the switch on, but not this one.
		return
	}
	if entry.ProjID == "" {
		// Enabled by an older engram that never persisted the resolved project
		// id. Resolving it here would mean creating a MemoryLake project from a
		// fire-and-forget hook; let the next mem_save do it instead.
		return
	}

	// Hard watchdog: armed only from here on, once it's known this invocation
	// actually has work to do (backend enabled, conversation sync on, project
	// id resolved). Every return above this point is the hot path — almost
	// every invocation of this command — and none of them now pay for
	// allocating a timer they will never need. sessionID at this point is
	// whatever the Stop hook passed on the command line; the fallback to the
	// transcript's own session id (below) hasn't run yet, so an unresolved id
	// is logged empty rather than delaying arming until after LastTurn runs.
	if d := turnWatchdogTimeout(); d > 0 {
		time.AfterFunc(d, func() { turnWatchdogFire(project, sessionID, d) })
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
	mlCfg = clampTurnRequestTimeout(mlCfg)
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

// anyConversationSyncEnabled reports whether at least one project in enab has
// per-turn conversation sync turned on. It exists so cmdTurn can decide,
// without calling detectProject, whether spending the git-shellout cost of
// project detection is worth it at all: when the answer is no for every
// project, it is no for this one either.
func anyConversationSyncEnabled(enab *memorylake.Enablement) bool {
	if enab == nil {
		return false
	}
	for name := range enab.EnabledProjects {
		if enab.IsConversationSyncEnabled(name) {
			return true
		}
	}
	return false
}

// clampTurnRequestTimeout lowers cfg.TimeoutMS to turnRequestTimeoutLimit()
// when it is unset or larger, so a generously configured (or default, 30s)
// ENGRAM_MEMORYLAKE_TIMEOUT_MS cannot make a single MemoryLake round trip on
// this hook-driven path run longer than that limit. It never raises the
// timeout — a caller who configured something tighter than the limit keeps
// their tighter value.
func clampTurnRequestTimeout(cfg memorylake.Config) memorylake.Config {
	limit := turnRequestTimeoutLimit()
	if cfg.TimeoutMS <= 0 || cfg.TimeoutMS > limit {
		cfg.TimeoutMS = limit
	}
	return cfg
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

// turnRequestTimeoutLimit reads the per-request timeout ceiling used by
// clampTurnRequestTimeout from ENGRAM_TURN_REQUEST_TIMEOUT_MS, tolerating
// garbage the same way turnMaxBytes does.
func turnRequestTimeoutLimit() int {
	if v := strings.TrimSpace(os.Getenv("ENGRAM_TURN_REQUEST_TIMEOUT_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTurnRequestTimeoutMS
}

// turnWatchdogTimeout reads the whole-process deadline from
// ENGRAM_TURN_WATCHDOG_MS, tolerating garbage the same way turnMaxBytes does.
// A non-positive result disables the watchdog entirely (not used by any
// current caller, but keeps the helper's contract honest for callers that
// might want to opt out).
func turnWatchdogTimeout() time.Duration {
	ms := defaultTurnWatchdogMS
	if v := strings.TrimSpace(os.Getenv("ENGRAM_TURN_WATCHDOG_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	return time.Duration(ms) * time.Millisecond
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

	// %q on the error, not %v: an error whose message embeds a newline (an
	// HTTP response body echoed back into it, for one) would otherwise split
	// one failure across multiple lines in a file whose entire format is one
	// line per failure.
	fmt.Fprintf(f, "%s project=%s session=%s error=%q\n",
		time.Now().UTC().Format(time.RFC3339), project, sessionID, cause.Error())
}
