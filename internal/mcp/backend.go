package mcp

import "github.com/Gentleman-Programming/engram/internal/store"

// MemoryBackend abstracts the storage capabilities that mem_* tool handlers
// depend on. *store.Store (SQLite, the default local backend) implements it
// today; a future *memorylake.MemoryLakeBackend can implement it as an
// opt-in per-project alternative. Parameter/result types are reused from the
// store package so handler bodies require zero changes when depending on
// this interface instead of the concrete *store.Store.
//
// The method set below is the call surface that existing mem_* handlers in
// mcp.go actually invoke on *store.Store, verified against the real
// signatures in internal/store/*.go (not merely the handler call sites).
type MemoryBackend interface {
	// Observation CRUD
	AddObservation(p store.AddObservationParams) (int64, error)
	GetObservation(id int64) (*store.Observation, error)
	UpdateObservation(id int64, p store.UpdateObservationParams) (*store.Observation, error)
	DeleteObservation(id int64, hardDelete bool) error
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
	Timeline(observationID int64, before, after int) (*store.TimelineResult, error)
	FormatContext(project, scope string) (string, error)
	Stats() (*store.Stats, error)
	MaxObservationLength() int

	// Pin / review
	PinObservation(id int64) error
	UnpinObservation(id int64) error
	ObservationsNeedingReview(project string, limit int) ([]store.Observation, error)
	MarkReviewed(id int64) error

	// Sessions
	CreateSession(id, project, directory string) error
	GetSession(id string) (*store.Session, error)
	EndSession(id string, summary string) error
	MostRecentActiveSession(project string) (string, bool, error)
	RecentSessions(project string, limit int) ([]store.SessionSummary, error)

	// Prompts / passive capture
	AddPrompt(p store.AddPromptParams) (int64, error)
	AddPromptIfMissing(p store.AddPromptParams) (int64, bool, error)
	PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error)

	// Projects
	ProjectExists(name string) (bool, error)
	ListProjectNames() ([]string, error)
	CountObservationsForProject(name string) (int, error)
	MergeProjects(sources []string, canonical string) (*store.MergeResult, error)

	// Relations / conflict judging
	FindCandidates(savedID int64, opts store.CandidateOptions) ([]store.Candidate, error)
	GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error)
	JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error)
	JudgeBySemantic(p store.JudgeBySemanticParams) (string, error)
}

// Compile-time assertion that *store.Store (SQLite) implements MemoryBackend.
// Re-asserted in backend_test.go via TestStoreSatisfiesMemoryBackend.
var _ MemoryBackend = (*store.Store)(nil)
