package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// sessionRecord is the local, MemoryLake-independent record of an engram
// session's lifecycle fields. See SessionIndex's doc comment for why these
// fields live here instead of on the MemoryLake conversation object.
type sessionRecord struct {
	Project   string  `json:"project"`
	Directory string  `json:"directory"`
	StartedAt string  `json:"started_at"`
	EndedAt   *string `json:"ended_at,omitempty"`
	Summary   *string `json:"summary,omitempty"`
}

// SessionIndex persists engram's session lifecycle (project, directory,
// started_at/ended_at/summary) alongside the MemoryLake conversation that
// CreateSession ensures for the same session id.
//
// Only three V3 endpoints are documented/tested as touching conversations at
// all (see task-13 brief): create (POST .../conversations, body
// {custom_id,kind,actor_ids,rw_project_ids}), get-by-id (GET
// .../conversations/{convId}), and a paginated list. None of those confirms
// a "metadata" field, an "end" operation, or a "summary" field on the
// conversation object itself — the only thing MemoryLake's conversation
// schema is confirmed to model is its participants
// (actor_ids/rw_project_ids), not engram's session lifecycle. Guessing at
// (and PATCHing into) an unconfirmed schema field, or resolving a session id
// to a conversation id via ensureConversation as a side effect of a plain
// read/end/list call (silently creating a conversation that a mere lookup
// should never create), would both be exactly the kind of fabrication the
// task brief asks us not to do.
//
// Instead this index tracks the fields engram's callers actually need
// locally — the same accepted pattern IDMap and TopicIndex already use for
// information MemoryLake can't (or isn't confirmed to) answer (see their doc
// comments). CreateSession still calls ensureConversation (the one part of
// "session ↔ conversation" that IS a real, tested MemoryLake write), so the
// conversation genuinely exists in MemoryLake; this index is what supplies
// the rest of store.Session's shape on every subsequent read.
type SessionIndex struct {
	mu sync.Mutex

	Sessions map[string]*sessionRecord `json:"sessions"`

	path string
}

// sessionIndexPath returns the per-project SessionIndex location:
// ~/.engram/memorylake-sessions-<projID>.json.
func sessionIndexPath(projID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake-sessions-"+projID+".json")
}

// LoadSessionIndex reads the session index from path. A missing file is not
// an error — it yields a fresh, empty index.
func LoadSessionIndex(path string) (*SessionIndex, error) {
	s := &SessionIndex{
		Sessions: map[string]*sessionRecord{},
		path:     path,
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Sessions == nil {
		s.Sessions = map[string]*sessionRecord{}
	}
	s.path = path
	return s, nil
}

// Create records a new session id with the given project/directory/startedAt
// if it isn't already known. Repeat calls with the same id mirror
// internal/store's createSessionTx upsert: the original started_at is never
// overwritten, and project/directory are only backfilled when the existing
// record has them empty (see store.go:createSessionTx's `CASE WHEN
// sessions.project = ” THEN excluded.project ELSE sessions.project END`).
func (s *SessionIndex) Create(id, project, directory, startedAt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.Sessions[id]
	if !ok {
		s.Sessions[id] = &sessionRecord{Project: project, Directory: directory, StartedAt: startedAt}
		return s.save()
	}
	changed := false
	if rec.Project == "" && project != "" {
		rec.Project = project
		changed = true
	}
	if rec.Directory == "" && directory != "" {
		rec.Directory = directory
		changed = true
	}
	if !changed {
		return nil
	}
	return s.save()
}

// Get returns the recorded session, if any.
func (s *SessionIndex) Get(id string) (sessionRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Sessions[id]
	if !ok {
		return sessionRecord{}, false
	}
	return *rec, true
}

// End marks session id ended with the given endedAt/summary. Mirrors
// internal/store's EndSession: ending an id that isn't known is a no-op (no
// error), the same way store.go's EndSession treats an UPDATE that affects
// zero rows (see store.go:EndSession). summary == "" leaves Summary unset,
// matching store's nullableString(summary) turning an empty string into
// SQL NULL rather than an empty string.
func (s *SessionIndex) End(id, endedAt, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.Sessions[id]
	if !ok {
		return nil
	}
	rec.EndedAt = &endedAt
	if summary != "" {
		rec.Summary = &summary
	}
	return s.save()
}

// MostRecentActive returns the id of the most recently started, not-yet-ended
// session recorded for project, mirroring internal/store's
// MostRecentActiveSession selection rules: scoped to project, ended_at must
// be unset, "manual-save*" ids are excluded (the manual-save fallback must
// never resolve as "the active session" — see store.go:MostRecentActiveSession's
// doc comment for why), and ties break on the lexically greatest id when
// started_at ties (store.go's `ORDER BY started_at DESC, id DESC` — id is a
// UUID string in this backend, not an autoincrement int, so "greatest" here
// is only a deterministic tie-break, not a temporal one).
func (s *SessionIndex) MostRecentActive(project string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var bestID, bestStarted string
	found := false
	for id, rec := range s.Sessions {
		if rec.Project != project || rec.EndedAt != nil {
			continue
		}
		if strings.HasPrefix(id, "manual-save") {
			continue
		}
		if !found || rec.StartedAt > bestStarted || (rec.StartedAt == bestStarted && id > bestID) {
			bestID, bestStarted, found = id, rec.StartedAt, true
		}
	}
	return bestID, found
}

// Recent returns up to limit sessions for project (all projects when project
// is "", mirroring store.go:RecentSessions' own unfiltered fallback),
// ordered most-recent-first (started_at desc, id desc tie-break).
//
// ObservationCount is always left at its zero value: MemoryLake facts carry
// no session linkage in this backend's stamped metadata (see mapper.go's
// FactMetadata — no session id key), so there is no analogue of the store's
// `LEFT JOIN observations ... COUNT(o.id)` to reproduce. Reporting 0 rather
// than an invented number is the fail-safe choice the task brief asks for.
func (s *SessionIndex) Recent(project string, limit int) []store.SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()

	type entry struct {
		id  string
		rec *sessionRecord
	}
	var matched []entry
	for id, rec := range s.Sessions {
		if project != "" && rec.Project != project {
			continue
		}
		matched = append(matched, entry{id, rec})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].rec.StartedAt != matched[j].rec.StartedAt {
			return matched[i].rec.StartedAt > matched[j].rec.StartedAt
		}
		return matched[i].id > matched[j].id
	})
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}

	out := make([]store.SessionSummary, 0, len(matched))
	for _, e := range matched {
		out = append(out, store.SessionSummary{
			ID:               e.id,
			Project:          e.rec.Project,
			StartedAt:        e.rec.StartedAt,
			EndedAt:          e.rec.EndedAt,
			Summary:          e.rec.Summary,
			ObservationCount: 0,
		})
	}
	return out
}

// Save persists the index to its path, creating parent directories as
// needed.
func (s *SessionIndex) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

// save writes the index to disk. Callers must hold s.mu.
func (s *SessionIndex) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o644)
}
