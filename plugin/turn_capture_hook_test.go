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

// TestTurnCaptureScriptDegradesGracefully pins the three properties that keep
// this hook from ever breaking a session: it checks the binary exists, it exits
// 0 unconditionally, and it swallows the CLI's exit code.
func TestTurnCaptureScriptDegradesGracefully(t *testing.T) {
	path := filepath.Join(repoRoot(t), "plugin", "claude-code", "scripts", "turn-capture.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)

	for _, want := range []string{
		"command -v engram",
		"|| true",
		"exit 0",
		"engram turn",
		"--transcript",
		"--session",
		"--cwd",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("turn-capture.sh must contain %q", want)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("turn-capture.sh must be executable")
	}
}
