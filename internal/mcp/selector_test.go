package mcp

import (
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

func TestDefaultSelectorAlwaysSQLite(t *testing.T) {
	sqlite := &fakeBackend{name: "sqlite"}
	sel := StaticSelector(sqlite)
	if sel("anything") != sqlite {
		t.Fatal("default selector must always return the sqlite backend")
	}
	if sel("") != sqlite {
		t.Fatal("default selector must always return the sqlite backend, even for an empty project")
	}
	if sel("other-project") != sqlite {
		t.Fatal("default selector must ignore the requested project")
	}
}

// fakeBackend is a minimal MemoryBackend stub used only by selector tests.
// Method bodies panic — this test only ever checks identity of the returned
// backend, it never invokes a method on it.
type fakeBackend struct {
	name string
}

var _ MemoryBackend = (*fakeBackend)(nil)

func (f *fakeBackend) AddObservation(p store.AddObservationParams) (string, error) { panic("unused") }
func (f *fakeBackend) GetObservation(syncID string) (*store.Observation, error)    { panic("unused") }
func (f *fakeBackend) UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error) {
	panic("unused")
}
func (f *fakeBackend) DeleteObservation(syncID string, hardDelete bool) error { panic("unused") }
func (f *fakeBackend) Search(query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	panic("unused")
}
func (f *fakeBackend) Timeline(syncID string, before, after int) (*store.TimelineResult, error) {
	panic("unused")
}
func (f *fakeBackend) FormatContext(project, scope string) (string, error) { panic("unused") }
func (f *fakeBackend) Stats() (*store.Stats, error)                        { panic("unused") }
func (f *fakeBackend) MaxObservationLength() int                           { panic("unused") }

func (f *fakeBackend) PinObservation(syncID string) error   { panic("unused") }
func (f *fakeBackend) UnpinObservation(syncID string) error { panic("unused") }
func (f *fakeBackend) ObservationsNeedingReview(project string, limit int) ([]store.Observation, error) {
	panic("unused")
}
func (f *fakeBackend) MarkReviewed(syncID string) error { panic("unused") }

func (f *fakeBackend) CreateSession(id, project, directory string) error { panic("unused") }
func (f *fakeBackend) GetSession(id string) (*store.Session, error)      { panic("unused") }
func (f *fakeBackend) EndSession(id string, summary string) error        { panic("unused") }
func (f *fakeBackend) MostRecentActiveSession(project string) (string, bool, error) {
	panic("unused")
}
func (f *fakeBackend) RecentSessions(project string, limit int) ([]store.SessionSummary, error) {
	panic("unused")
}

func (f *fakeBackend) AddPrompt(p store.AddPromptParams) (string, error) { panic("unused") }
func (f *fakeBackend) AddPromptIfMissing(p store.AddPromptParams) (string, bool, error) {
	panic("unused")
}
func (f *fakeBackend) PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error) {
	panic("unused")
}

func (f *fakeBackend) ProjectExists(name string) (bool, error) { panic("unused") }
func (f *fakeBackend) ListProjectNames() ([]string, error)     { panic("unused") }
func (f *fakeBackend) CountObservationsForProject(name string) (int, error) {
	panic("unused")
}
func (f *fakeBackend) MergeProjects(sources []string, canonical string) (*store.MergeResult, error) {
	panic("unused")
}

func (f *fakeBackend) FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error) {
	panic("unused")
}
func (f *fakeBackend) GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error) {
	panic("unused")
}
func (f *fakeBackend) JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error) {
	panic("unused")
}
func (f *fakeBackend) JudgeBySemantic(p store.JudgeBySemanticParams) (string, error) {
	panic("unused")
}
