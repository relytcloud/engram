package plugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTurnCaptureHookIsRegistered checks the Stop hook wiring: the existing
// session-stop entry must survive, the new turn-capture entry must be present,
// and it must be async so it never delays the user's reply.
func TestTurnCaptureHookIsRegistered(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "plugin", "claude-code", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}

	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("hooks.json must stay valid JSON: %v", err)
	}

	stop := cfg.Hooks["Stop"]
	var sawSessionStop, sawTurnCapture bool
	for _, group := range stop {
		for _, h := range group.Hooks {
			if strings.Contains(h.Command, "session-stop.sh") {
				sawSessionStop = true
			}
			if strings.Contains(h.Command, "turn-capture.sh") {
				sawTurnCapture = true
				if !h.Async {
					t.Error("turn-capture must be async so it never delays the reply")
				}
				if h.Command != "\"${CLAUDE_PLUGIN_ROOT}/scripts/turn-capture.sh\"" {
					t.Errorf("command must be the quoted plugin-root form, got %q", h.Command)
				}
				// engram turn (Task 7) clamps each MemoryLake round trip to
				// 10s and makes ~4 sequential round trips before its own 45s
				// watchdog fires. The hook timeout must stay above that 45s
				// watchdog so the watchdog -- which exits 0 cleanly -- is
				// what bounds the process, not an external kill by Claude
				// Code. This costs nothing: the hook is async, so Claude
				// Code never waits on it regardless of the value.
				if h.Timeout != 60 {
					t.Errorf("turn-capture timeout must be 60 (above engram turn's 45s internal watchdog), got %d", h.Timeout)
				}
			}
		}
	}
	if !sawSessionStop {
		t.Error("the existing session-stop hook must not be removed")
	}
	if !sawTurnCapture {
		t.Error("turn-capture.sh must be registered on Stop")
	}
}

// codeOnlyLines strips full-line shell comments (any line whose trimmed form
// starts with "#", including the shebang) out of a script, and joins
// backslash line-continuations into single logical lines. This exists so
// guard assertions below can't be satisfied by prose in a comment describing
// the guard -- they have to find the guard in the executable code itself,
// anchored to the specific command line it belongs to.
func codeOnlyLines(script string) []string {
	var logical []string
	var buf strings.Builder
	continuing := false
	for _, raw := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(raw)
		if !continuing {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
		}
		if strings.HasSuffix(trimmed, "\\") {
			buf.WriteString(strings.TrimSuffix(trimmed, "\\"))
			buf.WriteString(" ")
			continuing = true
			continue
		}
		buf.WriteString(trimmed)
		logical = append(logical, buf.String())
		buf.Reset()
		continuing = false
	}
	return logical
}

// TestTurnCaptureScriptDegradesGracefully pins the three properties that keep
// this hook from ever breaking a session: it checks the binary exists, it exits
// 0 unconditionally, and it swallows the CLI's exit code.
//
// Every assertion here is anchored to executable code, not to a bare
// substring search over the whole file: this script's own explanatory
// comments quote "engram turn", "exit 0", and "|| true" in prose, so a naive
// strings.Contains(script, ...) check would keep passing even if the
// functional guard were deleted and only the comment survived. In
// particular, the `|| true` guard is asserted on the exact logical line that
// invokes `engram turn` -- not merely present somewhere in the file -- so
// that deleting it from the invocation while leaving the comment intact
// fails this test.
func TestTurnCaptureScriptDegradesGracefully(t *testing.T) {
	path := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "turn-capture.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	lines := codeOnlyLines(script)
	code := strings.Join(lines, "\n")

	for _, want := range []string{
		"command -v engram",
		"exit 0",
		"engram turn",
		"--transcript",
		"--session",
		"--cwd",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("turn-capture.sh must contain %q as executable code (comment mentions do not count); code was:\n%s", want, code)
		}
	}

	var invocation string
	for _, line := range lines {
		if strings.Contains(line, "engram turn") {
			invocation = line
			break
		}
	}
	if invocation == "" {
		t.Fatal("could not find the `engram turn` invocation line in executable code")
	}
	if !strings.Contains(invocation, "|| true") {
		t.Errorf("the `engram turn` invocation must be guarded with `|| true` on its own logical line, got: %q", invocation)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("turn-capture.sh must be executable")
	}
}
