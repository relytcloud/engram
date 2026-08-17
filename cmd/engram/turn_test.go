package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// failingMemoryLake is a server that fails the test on any request. Used to
// prove the "no network" claims.
func failingMemoryLake(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no MemoryLake request may be made (%s %s)", r.Method, r.URL.Path)
	}))
}

// writeTurnTranscript writes a minimal one-turn transcript and returns its path.
func writeTurnTranscript(t *testing.T, sessionID string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := []string{
		`{"type":"user","sessionId":"` + sessionID + `","message":{"role":"user","content":"fix the uploader"}}`,
		`{"type":"assistant","sessionId":"` + sessionID + `","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCmdTurnUnenabledProjectMakesNoNetworkCall is the hot path: almost every
// invocation of `engram turn` lands here and must cost nothing.
func TestCmdTurnUnenabledProjectMakesNoNetworkCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", t.TempDir())

	captureOutput(t, func() { cmdTurn() })
}

// TestCmdTurnRespectsSqliteSafetyValve: ENGRAM_BACKEND=sqlite disables the
// MemoryLake path globally, and per-turn sync must honor it too.
func TestCmdTurnRespectsSqliteSafetyValve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ENGRAM_BACKEND", "sqlite")
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	// Fully enabled — only the safety valve should stop it.
	seedTurnEnablement(t, "acme", true)
	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })
}

func TestCmdTurnMissingTranscriptExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withArgs(t, "engram", "turn", "--session", "sess-1")

	_, stderr, code := captureExitPanic(t, func() { cmdTurn() })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a usage error", code)
	}
	if !strings.Contains(stderr, "--transcript") {
		t.Fatalf("stderr must name the missing flag, got %q", stderr)
	}
}

func TestCmdTurnUnknownFlagExitsTwo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withArgs(t, "engram", "turn", "--transcirpt", "/tmp/x")

	_, stderr, code := captureExitPanic(t, func() { cmdTurn() })
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for an unknown flag", code)
	}
	if !strings.Contains(stderr, "--transcirpt") {
		t.Fatalf("stderr must echo the bad flag, got %q", stderr)
	}
}

// TestCmdTurnMissingTranscriptFileExitsZero: a runtime problem is never a
// non-zero exit — the hook must not surface anything to the user.
func TestCmdTurnMissingTranscriptFileExitsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTurnEnablement(t, "acme", true)
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	withArgs(t, "engram", "turn",
		"--session", "sess-1",
		"--transcript", filepath.Join(t.TempDir(), "does-not-exist.jsonl"),
		"--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })

	// The failure must be recorded in the log file, not the terminal.
	logPath := filepath.Join(home, ".engram", "logs", "turn.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("a parse failure must be logged to %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "session=sess-1") {
		t.Fatalf("log line must identify the session, got %q", data)
	}
}

func TestCmdTurnAppendsTurnForEnabledProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedTurnEnablement(t, "acme", true)

	var msgPosts int32
	var gotText, gotConvCustomID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// NewBackend: EnsureActor create + workspace binding.
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		// AppendTurn.
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&msgPosts, 1)
			var body struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) > 0 {
				gotText = body.Content[0].Text
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")
	// A "ws-" prefixed workspace short-circuits ResolveWorkspaceID, so the
	// test server does not need a workspace list endpoint.
	t.Setenv("ENGRAM_MEMORYLAKE_WORKSPACE", "ws-1")

	transcript := writeTurnTranscript(t, "sess-9")
	withArgs(t, "engram", "turn",
		"--session", "sess-9",
		"--transcript", transcript,
		"--cwd", turnProjectDir(t, "acme"),
		"--verbose")

	stdout, _ := captureOutput(t, func() { cmdTurn() })

	if msgPosts != 1 {
		t.Fatalf("msgPosts = %d, want exactly 1", msgPosts)
	}
	if gotConvCustomID != "sess-9" {
		t.Fatalf("conversation custom_id = %q, want sess-9", gotConvCustomID)
	}
	if !strings.Contains(gotText, "**User:**") || !strings.Contains(gotText, "fix the uploader") {
		t.Fatalf("posted text must be the merged turn, got %q", gotText)
	}
	if !strings.Contains(gotText, "**Assistant:**") || !strings.Contains(gotText, "done") {
		t.Fatalf("posted text must include the assistant reply, got %q", gotText)
	}
	if !strings.Contains(stdout, "appended turn to conversation sess-9") {
		t.Fatalf("--verbose must report the append, got %q", stdout)
	}
}

// TestCmdTurnEnabledBackendButConversationSyncOffMakesNoCall covers the
// half-enabled state: MemoryLake backend on, per-turn sync off.
func TestCmdTurnEnabledBackendButConversationSyncOffMakesNoCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTurnEnablement(t, "acme", false)
	srv := failingMemoryLake(t)
	defer srv.Close()
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srv.URL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")

	transcript := writeTurnTranscript(t, "sess-1")
	withArgs(t, "engram", "turn", "--session", "sess-1", "--transcript", transcript, "--cwd", turnProjectDir(t, "acme"))

	captureOutput(t, func() { cmdTurn() })
}

// seedTurnEnablement writes $HOME/.engram/memorylake.json with project enabled
// for the MemoryLake backend and conversation sync set to syncOn.
func seedTurnEnablement(t *testing.T, project string, syncOn bool) {
	t.Helper()
	path := memorylake.DefaultEnablementPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		project: {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z", SyncConversations: syncOn},
	}}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
}

// turnProjectDir returns a directory whose detected project name is `project`,
// by writing an .engram/config that names it explicitly. This keeps the test
// independent of git remotes and of the directory's own basename.
func turnProjectDir(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, ".engram")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// internal/project/detect.go's readConfigAt reads .engram/config.json and
	// unmarshals it into configFile{ProjectName string `json:"project_name"`}.
	body := []byte(`{"project_name":"` + project + `"}`)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}
