package mcp

import "github.com/Gentleman-Programming/engram/internal/store"

// MemoryBackend abstracts the storage capabilities that mem_* tool handlers
// depend on. A thin sqliteBackend adapter (sqlite_backend.go) implements it
// today for the local SQLite store; a future *memorylake.MemoryLakeBackend
// implements it as an opt-in per-project alternative.
//
// By-id methods are keyed by the opaque string sync_id (SQLite:
// store.Observation.SyncID, already exposed by search/save responses;
// MemoryLake: the fact id) rather than the SQLite-only int64 primary key —
// see docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md §3
// (A1'). store.Observation.ID stays int64 and *store.Store's own methods are
// unchanged; sqliteBackend is what translates sync_id <-> int64 at the
// interface boundary via store.GetObservationBySyncID.
//
// The method set below is the call surface that existing mem_* handlers in
// mcp.go actually invoke on a backend, verified against the real signatures
// in internal/store/*.go (not merely the handler call sites).
type MemoryBackend interface {
	// Observation CRUD
	AddObservation(p store.AddObservationParams) (string, error)
	GetObservation(syncID string) (*store.Observation, error)
	UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error)
	DeleteObservation(syncID string, hardDelete bool) error
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
	Timeline(syncID string, before, after int) (*store.TimelineResult, error)
	FormatContext(project, scope string) (string, error)
	Stats() (*store.Stats, error)
	MaxObservationLength() int

	// Pin / review
	PinObservation(syncID string) error
	UnpinObservation(syncID string) error
	ObservationsNeedingReview(project string, limit int) ([]store.Observation, error)
	MarkReviewed(syncID string) error

	// Sessions
	CreateSession(id, project, directory string) error
	GetSession(id string) (*store.Session, error)
	EndSession(id string, summary string) error
	MostRecentActiveSession(project string) (string, bool, error)
	RecentSessions(project string, limit int) ([]store.SessionSummary, error)

	// Prompts / passive capture
	AddPrompt(p store.AddPromptParams) (string, error)
	AddPromptIfMissing(p store.AddPromptParams) (string, bool, error)
	PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error)

	// Projects
	ProjectExists(name string) (bool, error)
	ListProjectNames() ([]string, error)
	CountObservationsForProject(name string) (int, error)
	MergeProjects(sources []string, canonical string) (*store.MergeResult, error)

	// Relations / conflict judging
	FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error)
	GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error)
	JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error)
	JudgeBySemantic(p store.JudgeBySemanticParams) (string, error)
}

// candidateOptOut is an optional capability a MemoryBackend implementation
// may satisfy to tell handleSave (mem_save) it should skip generating and
// returning conflict candidates for this backend entirely, rather than
// calling FindCandidates and reporting judgment_required/judgment_status/
// candidates as usual.
//
// This exists for *memorylake.MemoryLakeBackend under the Option A thin
// adapter (docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md
// §4/§7): mem0 already owns dedup/conflict detection asynchronously
// downstream of a save, so Engram's own save-time candidate surfacing would
// be redundant, and there is no local judgment_required loop to route an
// agent into for that backend. internal/mcp cannot import
// internal/memorylake (see NewRoutingSelector's doc comment in
// cmd/engram/routing.go on the import-cycle constraint that assembles both),
// so this is a plain structural (duck-typed) interface check — handleSave
// type-asserts a resolved MemoryBackend against it via `s.(candidateOptOut)`
// rather than a concrete type switch. sqliteBackend does not implement this,
// so the check is always a no-op (skipCandidates stays false) for the
// default SQLite path.
type candidateOptOut interface {
	SkipsCandidateGeneration() bool
}
