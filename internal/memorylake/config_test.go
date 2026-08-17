package memorylake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnablementRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	e.EnabledProjects["acme"] = ProjectEntry{ProjID: "proj-1", EnabledAt: "2026-07-22T00:00:00Z"}
	if err := e.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := got.IsEnabled("acme"); !ok || entry.ProjID != "proj-1" {
		t.Fatalf("want proj-1 enabled, got %+v ok=%v", entry, ok)
	}
	if _, ok := got.IsEnabled("other"); ok {
		t.Fatal("other must not be enabled")
	}
}

func TestLoadEnablementMissingFileIsEmpty(t *testing.T) {
	got, err := LoadEnablement(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must be empty, not error: %v", err)
	}
	if _, ok := got.IsEnabled("x"); ok {
		t.Fatal("empty enablement")
	}
}

func TestSetConversationSyncRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{
		"acme": {ProjID: "proj-1", EnabledAt: "2026-08-06T00:00:00Z"},
	}}

	if e.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must default to off")
	}

	if err := e.SetConversationSync("acme", true); err != nil {
		t.Fatalf("SetConversationSync(on): %v", err)
	}
	if err := e.Save(p); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must survive a save/load round trip")
	}
	// Turning it back off must also persist, and must not drop the entry.
	if err := got.SetConversationSync("acme", false); err != nil {
		t.Fatalf("SetConversationSync(off): %v", err)
	}
	if err := got.Save(p); err != nil {
		t.Fatal(err)
	}
	again, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if again.IsConversationSyncEnabled("acme") {
		t.Fatal("conversation sync must be off after disable")
	}
	if entry, ok := again.IsEnabled("acme"); !ok || entry.ProjID != "proj-1" {
		t.Fatalf("disabling conversation sync must not touch backend enablement: %+v ok=%v", entry, ok)
	}
}

// TestSetConversationSyncRequiresBackendEnablement locks decision D1: per-turn
// conversation sync attaches to a project that already routes to MemoryLake.
func TestSetConversationSyncRequiresBackendEnablement(t *testing.T) {
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	err := e.SetConversationSync("not-enabled", true)
	if err == nil {
		t.Fatal("enabling conversation sync on a non-MemoryLake project must fail")
	}
	if !strings.Contains(err.Error(), "memorylake enable") {
		t.Fatalf("error must tell the user how to fix it, got: %v", err)
	}
	if e.IsConversationSyncEnabled("not-enabled") {
		t.Fatal("a rejected SetConversationSync must not mutate state")
	}
}

// TestLegacyEnablementFileDefaultsConversationSyncOff is the backward-compat
// guard: a memorylake.json written before this feature existed must read back
// with conversation sync off, with no migration step.
func TestLegacyEnablementFileDefaultsConversationSyncOff(t *testing.T) {
	p := filepath.Join(t.TempDir(), "memorylake.json")
	legacy := `{"enabled_projects":{"acme":{"proj_id":"proj-1","enabled_at":"2026-07-22T00:00:00Z"}}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnablement(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.IsEnabled("acme"); !ok {
		t.Fatal("legacy entry must still be backend-enabled")
	}
	if got.IsConversationSyncEnabled("acme") {
		t.Fatal("legacy entry must have conversation sync off")
	}
}
