package memorylake

import (
	"path/filepath"
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
