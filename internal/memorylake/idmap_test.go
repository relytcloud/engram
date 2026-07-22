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

	i1 := m.IntFor("proj-1", "fact-a")
	i2 := m.IntFor("proj-1", "fact-b")
	if i1 == i2 {
		t.Fatalf("distinct fact ids must get distinct int ids, got %d and %d", i1, i2)
	}
	// Re-requesting the same (proj, fact) pair must return the same int id.
	if again := m.IntFor("proj-1", "fact-a"); again != i1 {
		t.Fatalf("IntFor(fact-a) not stable: first=%d second=%d", i1, again)
	}

	if pid, got, ok := m.FactFor(i1); !ok || pid != "proj-1" || got != "fact-a" {
		t.Fatalf("FactFor(%d)=%q/%q,%v want proj-1/fact-a,true", i1, pid, got, ok)
	}
	if pid, got, ok := m.FactFor(i2); !ok || pid != "proj-1" || got != "fact-b" {
		t.Fatalf("FactFor(%d)=%q/%q,%v want proj-1/fact-b,true", i2, pid, got, ok)
	}
	if _, _, ok := m.FactFor(9999); ok {
		t.Fatal("unknown int id must not resolve")
	}

	// Round trip through disk: a freshly loaded map must rebuild the reverse
	// index and continue allocating ids after Next, not reuse i1/i2.
	reloaded, err := LoadIDMap(p)
	if err != nil {
		t.Fatal(err)
	}
	if pid, got, ok := reloaded.FactFor(i1); !ok || pid != "proj-1" || got != "fact-a" {
		t.Fatalf("reloaded FactFor(%d)=%q/%q,%v want proj-1/fact-a,true", i1, pid, got, ok)
	}
	if pid, got, ok := reloaded.FactFor(i2); !ok || pid != "proj-1" || got != "fact-b" {
		t.Fatalf("reloaded FactFor(%d)=%q/%q,%v want proj-1/fact-b,true", i2, pid, got, ok)
	}
	if got := reloaded.IntFor("proj-1", "fact-a"); got != i1 {
		t.Fatalf("reloaded IntFor(fact-a)=%d want %d", got, i1)
	}
	i3 := reloaded.IntFor("proj-1", "fact-c")
	if i3 == i1 || i3 == i2 {
		t.Fatalf("new fact id after reload must not collide with existing ids: i1=%d i2=%d i3=%d", i1, i2, i3)
	}
}

func TestLoadIDMapMissingFileIsEmpty(t *testing.T) {
	m, err := LoadIDMap(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must be empty, not error: %v", err)
	}
	if _, _, ok := m.FactFor(1); ok {
		t.Fatal("empty map must not resolve any id")
	}
	if got := m.IntFor("proj-1", "first"); got != 1 {
		t.Fatalf("first allocated id should be 1, got %d", got)
	}
}

// TestIDMap_GloballyUniqueAcrossProjects is the core regression for the by-id
// cross-project leak: two DIFFERENT projects whose FIRST fact happens to share
// the same opaque fact id ("fact-1") must still get DISTINCT global int64 ids,
// and each id must reverse-map back to its OWN project — never the other's.
// Before the global IDMap, each project kept its own map starting at 1, so both
// first facts were id=1 and a by-id lookup could return the wrong project.
func TestIDMap_GloballyUniqueAcrossProjects(t *testing.T) {
	m, err := LoadIDMap(filepath.Join(t.TempDir(), "idmap.json"))
	if err != nil {
		t.Fatal(err)
	}

	idA := m.IntFor("proj-A", "fact-1") // A's first fact
	idB := m.IntFor("proj-B", "fact-1") // B's first fact, identical fact id string
	if idA == idB {
		t.Fatalf("first fact of two projects must get distinct global ids, both got %d", idA)
	}

	if pid, fid, ok := m.FactFor(idA); !ok || pid != "proj-A" || fid != "fact-1" {
		t.Fatalf("FactFor(%d)=%q/%q,%v want proj-A/fact-1,true", idA, pid, fid, ok)
	}
	if pid, fid, ok := m.FactFor(idB); !ok || pid != "proj-B" || fid != "fact-1" {
		t.Fatalf("FactFor(%d)=%q/%q,%v want proj-B/fact-1,true", idB, pid, fid, ok)
	}

	// Existence checks are per-project too: a key recorded under proj-A must
	// not read as present under proj-B.
	if _, ok := m.IntIfExists("proj-A", "fact-1"); !ok {
		t.Fatal("IntIfExists(proj-A, fact-1) should be present")
	}
	if _, ok := m.IntIfExists("proj-B", "fact-2"); ok {
		t.Fatal("IntIfExists(proj-B, fact-2) must be absent")
	}
}
