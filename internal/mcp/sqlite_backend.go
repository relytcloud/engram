package mcp

import (
	"strconv"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// sqliteBackend is the thin MemoryBackend adapter over the local SQLite
// store.Store. *store.Store itself is int64-keyed by primary key; this
// adapter translates the interface's string sync_id keys to/from that int64
// key via the store's existing store.GetObservationBySyncID, so store.go
// needs no changes at all — see docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md
// §3 (A1').
type sqliteBackend struct {
	s *store.Store
}

// newSQLiteBackend wraps s as a MemoryBackend.
func newSQLiteBackend(s *store.Store) *sqliteBackend {
	return &sqliteBackend{s: s}
}

// NewSQLiteBackend wraps s as a MemoryBackend for callers outside this
// package (internal/server, cmd/engram) that need to hand a *store.Store to
// a BackendSelector or MemoryBackend-typed field. Exported because
// *store.Store no longer implements MemoryBackend directly now that by-id
// methods are keyed by sync_id string instead of int64 (see backend.go).
func NewSQLiteBackend(s *store.Store) MemoryBackend {
	return newSQLiteBackend(s)
}

// StoreOf returns the underlying *store.Store if b is a sqlite-backed
// MemoryBackend (constructed via NewSQLiteBackend/newSQLiteBackend), and
// false otherwise. Exported for callers outside this package (cmd/engram)
// that need to fall back to concrete *store.Store methods/test-injection
// seams outside MemoryBackend's interface surface, now that *store.Store no
// longer implements MemoryBackend directly (see backend.go's sync_id
// migration, Phase 3 Task 1+2).
func StoreOf(b MemoryBackend) (*store.Store, bool) {
	sb, ok := b.(*sqliteBackend)
	if !ok {
		return nil, false
	}
	return sb.s, true
}

// underlyingStore exposes the wrapped *store.Store for the rare callers
// (mem_doctor's SQLite-only diagnostic.Scope) that need to run checks outside
// MemoryBackend's method set — direct SQL not part of the interface. Callers
// type-assert a resolved MemoryBackend down to *sqliteBackend (rather than
// *store.Store, which no longer implements MemoryBackend directly) to reach
// this.
func (b *sqliteBackend) underlyingStore() *store.Store {
	return b.s
}

var _ MemoryBackend = (*sqliteBackend)(nil)

// idFor resolves a sync_id to the underlying int64 primary key that
// *store.Store's own (unchanged) methods still expect. An unknown sync_id
// surfaces as whatever not-found error GetObservationBySyncID returns.
func (b *sqliteBackend) idFor(syncID string) (int64, error) {
	obs, err := b.s.GetObservationBySyncID(syncID)
	if err != nil {
		return 0, err
	}
	return obs.ID, nil
}

// ─── Observation CRUD ────────────────────────────────────────────────────────

func (b *sqliteBackend) AddObservation(p store.AddObservationParams) (string, error) {
	id, err := b.s.AddObservation(p)
	if err != nil {
		return "", err
	}
	obs, err := b.s.GetObservation(id)
	if err != nil {
		return "", err
	}
	return obs.SyncID, nil
}

func (b *sqliteBackend) GetObservation(syncID string) (*store.Observation, error) {
	return b.s.GetObservationBySyncID(syncID)
}

func (b *sqliteBackend) UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error) {
	id, err := b.idFor(syncID)
	if err != nil {
		return nil, err
	}
	return b.s.UpdateObservation(id, p)
}

func (b *sqliteBackend) DeleteObservation(syncID string, hardDelete bool) error {
	id, err := b.idFor(syncID)
	if err != nil {
		return err
	}
	return b.s.DeleteObservation(id, hardDelete)
}

func (b *sqliteBackend) Search(query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	return b.s.Search(query, opts)
}

func (b *sqliteBackend) Timeline(syncID string, before, after int) (*store.TimelineResult, error) {
	id, err := b.idFor(syncID)
	if err != nil {
		return nil, err
	}
	return b.s.Timeline(id, before, after)
}

func (b *sqliteBackend) FormatContext(project, scope string) (string, error) {
	return b.s.FormatContext(project, scope)
}

func (b *sqliteBackend) Stats() (*store.Stats, error) {
	return b.s.Stats()
}

func (b *sqliteBackend) MaxObservationLength() int {
	return b.s.MaxObservationLength()
}

// ─── Pin / review ────────────────────────────────────────────────────────────

func (b *sqliteBackend) PinObservation(syncID string) error {
	id, err := b.idFor(syncID)
	if err != nil {
		return err
	}
	return b.s.PinObservation(id)
}

func (b *sqliteBackend) UnpinObservation(syncID string) error {
	id, err := b.idFor(syncID)
	if err != nil {
		return err
	}
	return b.s.UnpinObservation(id)
}

func (b *sqliteBackend) ObservationsNeedingReview(project string, limit int) ([]store.Observation, error) {
	return b.s.ObservationsNeedingReview(project, limit)
}

func (b *sqliteBackend) MarkReviewed(syncID string) error {
	id, err := b.idFor(syncID)
	if err != nil {
		return err
	}
	return b.s.MarkReviewed(id)
}

// ─── Sessions ────────────────────────────────────────────────────────────────

func (b *sqliteBackend) CreateSession(id, project, directory string) error {
	return b.s.CreateSession(id, project, directory)
}

func (b *sqliteBackend) GetSession(id string) (*store.Session, error) {
	return b.s.GetSession(id)
}

func (b *sqliteBackend) EndSession(id string, summary string) error {
	return b.s.EndSession(id, summary)
}

func (b *sqliteBackend) MostRecentActiveSession(project string) (string, bool, error) {
	return b.s.MostRecentActiveSession(project)
}

func (b *sqliteBackend) RecentSessions(project string, limit int) ([]store.SessionSummary, error) {
	return b.s.RecentSessions(project, limit)
}

// ─── Prompts / passive capture ───────────────────────────────────────────────
//
// user_prompts rows do carry their own sync_id column (store.go stamps
// "prompt-<hex>" at insert time), but no MemoryBackend method looks a prompt
// back up by that sync_id (there is no GetPromptBySyncID in the interface),
// so nothing downstream depends on this id matching that real column value.
// Returning the decimal string of the row's int64 id keeps AddPrompt/
// AddPromptIfMissing's return type-compatible with the interface without
// requiring a store.go change; the true sync_id could be threaded through
// later if a by-id prompt lookup is ever added to the interface.

func (b *sqliteBackend) AddPrompt(p store.AddPromptParams) (string, error) {
	id, err := b.s.AddPrompt(p)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

func (b *sqliteBackend) AddPromptIfMissing(p store.AddPromptParams) (string, bool, error) {
	id, inserted, err := b.s.AddPromptIfMissing(p)
	if err != nil {
		return "", false, err
	}
	return strconv.FormatInt(id, 10), inserted, nil
}

func (b *sqliteBackend) PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error) {
	return b.s.PassiveCapture(p)
}

// ─── Projects ────────────────────────────────────────────────────────────────

func (b *sqliteBackend) ProjectExists(name string) (bool, error) {
	return b.s.ProjectExists(name)
}

func (b *sqliteBackend) ListProjectNames() ([]string, error) {
	return b.s.ListProjectNames()
}

func (b *sqliteBackend) CountObservationsForProject(name string) (int, error) {
	return b.s.CountObservationsForProject(name)
}

func (b *sqliteBackend) MergeProjects(sources []string, canonical string) (*store.MergeResult, error) {
	return b.s.MergeProjects(sources, canonical)
}

// ─── Relations / conflict judging ────────────────────────────────────────────

func (b *sqliteBackend) FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error) {
	id, err := b.idFor(savedSyncID)
	if err != nil {
		return nil, err
	}
	return b.s.FindCandidates(id, opts)
}

func (b *sqliteBackend) GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error) {
	return b.s.GetRelationsForObservations(syncIDs)
}

func (b *sqliteBackend) JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error) {
	return b.s.JudgeRelation(p)
}

func (b *sqliteBackend) JudgeBySemantic(p store.JudgeBySemanticParams) (string, error) {
	return b.s.JudgeBySemantic(p)
}
