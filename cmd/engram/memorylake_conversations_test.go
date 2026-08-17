package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// writeEnablement seeds $HOME/.engram/memorylake.json with the given entries.
// HOME must already be redirected to a temp dir by the caller.
func writeEnablement(t *testing.T, entries map[string]memorylake.ProjectEntry) string {
	t.Helper()
	path := memorylake.DefaultEnablementPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &memorylake.Enablement{EnabledProjects: entries}
	if err := e.Save(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMemorylakeConversationsEnableAndDisable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := writeEnablement(t, map[string]memorylake.ProjectEntry{
		"acme": {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z"},
	})

	withArgs(t, "engram", "memorylake", "conversations", "enable", "--project", "acme")
	stdout, _ := captureOutput(t, func() { cmdMemorylakeConversations() })
	if !strings.Contains(stdout, "Enabled per-turn conversation sync") {
		t.Fatalf("stdout should confirm the enable, got: %q", stdout)
	}

	enab, err := memorylake.LoadEnablement(path)
	if err != nil {
		t.Fatal(err)
	}
	if !enab.IsConversationSyncEnabled("acme") {
		t.Fatal("enable must persist sync_conversations=true")
	}

	withArgs(t, "engram", "memorylake", "conversations", "disable", "--project", "acme")
	stdout, _ = captureOutput(t, func() { cmdMemorylakeConversations() })
	if !strings.Contains(stdout, "Disabled per-turn conversation sync") {
		t.Fatalf("stdout should confirm the disable, got: %q", stdout)
	}

	enab, err = memorylake.LoadEnablement(path)
	if err != nil {
		t.Fatal(err)
	}
	if enab.IsConversationSyncEnabled("acme") {
		t.Fatal("disable must persist sync_conversations=false")
	}
	if _, ok := enab.IsEnabled("acme"); !ok {
		t.Fatal("disabling conversation sync must not disable the backend")
	}
}

func TestMemorylakeConversationsRejectsUnenabledProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeEnablement(t, map[string]memorylake.ProjectEntry{})

	withArgs(t, "engram", "memorylake", "conversations", "enable", "--project", "ghost")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "memorylake enable") {
		t.Fatalf("stderr must point at the fix, got: %q", stderr)
	}
}

func TestMemorylakeConversationsRequiresAction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	withArgs(t, "engram", "memorylake", "conversations", "--project", "acme")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "enable|disable") {
		t.Fatalf("stderr must name the valid actions, got: %q", stderr)
	}
}

func TestMemorylakeConversationsRequiresProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	withArgs(t, "engram", "memorylake", "conversations", "enable")
	_, stderr, code := captureExitPanic(t, func() { cmdMemorylakeConversations() })

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "--project") {
		t.Fatalf("stderr must name the missing flag, got: %q", stderr)
	}
}

func TestMemorylakeStatusShowsConversationSync(t *testing.T) {
	cfg := testConfig(t)
	t.Setenv("HOME", t.TempDir())
	writeEnablement(t, map[string]memorylake.ProjectEntry{
		"on-proj":  {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z", SyncConversations: true},
		"off-proj": {ProjID: "proj-2", EnabledAt: "2026-08-06T00:00:00Z"},
	})

	withArgs(t, "engram", "memorylake", "status")
	stdout, _ := captureOutput(t, func() { cmdMemorylakeStatus(cfg) })

	if !strings.Contains(stdout, "conversations=on") {
		t.Fatalf("status must show conversations=on for the enabled project, got: %q", stdout)
	}
	if !strings.Contains(stdout, "conversations=off") {
		t.Fatalf("status must show conversations=off for the other project, got: %q", stdout)
	}
}
