package mcp

import (
	"errors"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// existsBackend is a fakeBackend whose ProjectExists / Stats are configurable,
// used to verify resolveReadProject consults the *target project's* backend
// (via the selector) rather than a project-unaware default. All other methods
// panic — these tests never invoke them.
type existsBackend struct {
	*fakeBackend
	exists   bool
	projects []string
}

func (e *existsBackend) ProjectExists(name string) (bool, error) { return e.exists, nil }
func (e *existsBackend) Stats() (*store.Stats, error) {
	return &store.Stats{Projects: e.projects}, nil
}

// TestResolveReadProject_MemoryLakeEnabledExplicitProjectDoesNotErrUnknown is
// the FIX #2 regression: reading a MemoryLake-enabled project with an explicit
// project= must validate existence against that project's MemoryLakeBackend
// (whose ProjectExists reports true), NOT against the sqlite backend — a
// MemoryLake-only project has no local SQLite row, so validating it there
// wrongly returns unknown_project.
//
// The sqlite backend here is a bare fakeBackend whose ProjectExists panics: if
// resolveReadProject ever consults it for the enabled project, the test fails
// loudly. Success proves the check was routed through sel(project) to the
// MemoryLake-like backend instead.
func TestResolveReadProject_MemoryLakeEnabledExplicitProjectDoesNotErrUnknown(t *testing.T) {
	sqlite := &fakeBackend{name: "sqlite"} // ProjectExists panics — must not be consulted
	ml := &existsBackend{fakeBackend: &fakeBackend{name: "ml"}, exists: true}

	sel := func(project string) MemoryBackend {
		if project == "mlproj" {
			return ml
		}
		return sqlite
	}

	res, err := resolveReadProject(sel, "mlproj")
	if err != nil {
		t.Fatalf("resolveReadProject for MemoryLake-enabled project: unexpected error %v (want success, not unknown_project)", err)
	}
	if res.Project != "mlproj" {
		t.Fatalf("res.Project=%q, want mlproj", res.Project)
	}
}

// TestResolveReadProject_NotEnabledProjectStillValidatesAgainstSQLite confirms
// the fix does not change behavior for non-MemoryLake projects: an unknown
// project still resolves through sel(project) to the sqlite backend and, when
// that backend reports it does not exist, still yields unknownProjectError with
// the available-project list from sqlite Stats.
func TestResolveReadProject_NotEnabledProjectStillValidatesAgainstSQLite(t *testing.T) {
	sqlite := &existsBackend{fakeBackend: &fakeBackend{name: "sqlite"}, exists: false, projects: []string{"real-project"}}

	sel := func(project string) MemoryBackend { return sqlite }

	_, err := resolveReadProject(sel, "does-not-exist")
	if err == nil {
		t.Fatal("expected unknown_project error for a project absent from sqlite")
	}
	var upe *unknownProjectError
	if !errors.As(err, &upe) {
		t.Fatalf("error type = %T, want *unknownProjectError", err)
	}
	if upe.Name != "does-not-exist" {
		t.Fatalf("upe.Name=%q, want does-not-exist", upe.Name)
	}
	if len(upe.AvailableProjects) != 1 || upe.AvailableProjects[0] != "real-project" {
		t.Fatalf("AvailableProjects=%v, want [real-project] from sqlite Stats", upe.AvailableProjects)
	}
}
