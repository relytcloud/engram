package memorylake

import (
	"path/filepath"
	"testing"
)

func TestIDMapAssignsAndPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "idmap.json")

	m, err := LoadIDMap(p)
	if err != nil {
		t.Fatal(err)
	}

	i1 := m.IntFor("fact-a")
	i2 := m.IntFor("fact-b")
	if i1 == i2 {
		t.Fatalf("distinct fact ids must get distinct int ids, got %d and %d", i1, i2)
	}
	// Re-requesting the same fact id must return the same int id, not a new one.
	if again := m.IntFor("fact-a"); again != i1 {
		t.Fatalf("IntFor(fact-a) not stable: first=%d second=%d", i1, again)
	}

	if got, ok := m.FactFor(i1); !ok || got != "fact-a" {
		t.Fatalf("FactFor(%d)=%q,%v want fact-a,true", i1, got, ok)
	}
	if got, ok := m.FactFor(i2); !ok || got != "fact-b" {
		t.Fatalf("FactFor(%d)=%q,%v want fact-b,true", i2, got, ok)
	}
	if _, ok := m.FactFor(9999); ok {
		t.Fatal("unknown int id must not resolve")
	}

	// Round trip through disk: a freshly loaded map must rebuild the reverse
	// index and continue allocating ids after Next, not reuse i1/i2.
	reloaded, err := LoadIDMap(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.FactFor(i1); !ok || got != "fact-a" {
		t.Fatalf("reloaded FactFor(%d)=%q,%v want fact-a,true", i1, got, ok)
	}
	if got, ok := reloaded.FactFor(i2); !ok || got != "fact-b" {
		t.Fatalf("reloaded FactFor(%d)=%q,%v want fact-b,true", i2, got, ok)
	}
	if got := reloaded.IntFor("fact-a"); got != i1 {
		t.Fatalf("reloaded IntFor(fact-a)=%d want %d", got, i1)
	}
	i3 := reloaded.IntFor("fact-c")
	if i3 == i1 || i3 == i2 {
		t.Fatalf("new fact id after reload must not collide with existing ids: i1=%d i2=%d i3=%d", i1, i2, i3)
	}
}

func TestLoadIDMapMissingFileIsEmpty(t *testing.T) {
	m, err := LoadIDMap(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must be empty, not error: %v", err)
	}
	if _, ok := m.FactFor(1); ok {
		t.Fatal("empty map must not resolve any id")
	}
	if got := m.IntFor("first"); got != 1 {
		t.Fatalf("first allocated id should be 1, got %d", got)
	}
}
