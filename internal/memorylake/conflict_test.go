package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// conflictTestServer builds an httptest server that serves a fixed set of
// conflicts for project "proj-1" plus a fixed set of facts, and records
// resolve() calls so tests can assert on strategy/keep_fact_id. It rejects
// (with 404-ish success:false) any request whose project_id query param
// isn't "proj-1", modeling the real controller's project scoping.
type conflictTestServer struct {
	conflicts   map[string]conflictItem  // conflict id -> item
	facts       map[string]Fact          // fact id -> fact
	resolves    []conflictResolveRequest // recorded resolve() bodies, in call order
	resolvedIDs []string                 // conflict ids resolve() was called on, in call order
}

func newConflictTestServer() *conflictTestServer {
	return &conflictTestServer{
		conflicts: map[string]conflictItem{},
		facts:     map[string]Fact{},
	}
}

func (s *conflictTestServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projID := r.URL.Query().Get("project_id")
		if projID != "proj-1" {
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "error_code": "NOT_FOUND", "message": "unknown project",
			})
			return
		}

		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conflicts":
			items := make([]conflictItem, 0, len(s.conflicts))
			for _, c := range s.conflicts {
				items = append(items, c)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    conflictListPage{Items: items},
			})

		case r.Method == "GET" && len(r.URL.Path) > len("/api/v3/workspaces/ws-1/memories/conflicts/") &&
			r.URL.Path[:len("/api/v3/workspaces/ws-1/memories/conflicts/")] == "/api/v3/workspaces/ws-1/memories/conflicts/":
			id := r.URL.Path[len("/api/v3/workspaces/ws-1/memories/conflicts/"):]
			c, ok := s.conflicts[id]
			if !ok {
				json.NewEncoder(w).Encode(map[string]any{
					"success": false, "error_code": "NOT_FOUND", "message": "no such conflict",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": c})

		case r.Method == "POST":
			// .../conflicts/{id}/resolve
			const prefix = "/api/v3/workspaces/ws-1/memories/conflicts/"
			const suffix = "/resolve"
			path := r.URL.Path
			if len(path) <= len(prefix)+len(suffix) || path[:len(prefix)] != prefix || path[len(path)-len(suffix):] != suffix {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			id := path[len(prefix) : len(path)-len(suffix)]
			var body conflictResolveRequest
			json.NewDecoder(r.Body).Decode(&body)
			s.resolves = append(s.resolves, body)
			s.resolvedIDs = append(s.resolvedIDs, id)
			if c, ok := s.conflicts[id]; ok {
				c.Resolved = true
				s.conflicts[id] = c
			}
			// Mirrors the real controller: ResponseWrapper<Void>, data is
			// always null regardless of what the service computed.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": nil})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// factHandler serves GET .../projects/proj-1/memories/facts/{id} from
// s.facts, for use alongside conflictTestServer.handler() in a single mux.
func (s *conflictTestServer) factHandler(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/"
	id := r.URL.Path[len(prefix):]
	f, ok := s.facts[id]
	if !ok {
		json.NewEncoder(w).Encode(map[string]any{"success": false, "error_code": "NOT_FOUND", "message": "no such fact"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "data": f})
}

func newConflictBackend(t *testing.T, s *conflictTestServer) *MemoryLakeBackend {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/workspaces/ws-1/memories/conflicts", s.handler())
	mux.HandleFunc("/api/v3/workspaces/ws-1/memories/conflicts/", s.handler())
	mux.HandleFunc("/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/", s.factHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return newTestBackend(t, srv.URL)
}

// ─── FindCandidates ───────────────────────────────────────────────────────────

func TestFindCandidates_MapsUnresolvedConflictToCandidate(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		Category: "m2m",
		FactIDs:  []string{"fact-saved", "fact-other"},
		Resolved: false,
	}
	s.facts["fact-other"] = Fact{
		ID: "fact-other",
		Metadata: map[string]any{
			metaRaw:   "other content",
			metaTitle: "other title",
			metaType:  "decision",
			metaScope: "global",
		},
	}
	b := newConflictBackend(t, s)

	savedID := b.idmap.IntFor(b.projID, "fact-saved")

	candidates, err := b.FindCandidates(savedID, store.CandidateOptions{})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(candidates), candidates)
	}
	c := candidates[0]
	if c.SyncID != "fact-other" {
		t.Errorf("SyncID=%q, want fact-other", c.SyncID)
	}
	if c.Title != "other title" {
		t.Errorf("Title=%q, want 'other title'", c.Title)
	}
	if c.Type != "decision" {
		t.Errorf("Type=%q, want decision", c.Type)
	}
	if c.JudgmentID != "conf-1" {
		t.Errorf("JudgmentID=%q, want conf-1 (the conflict id)", c.JudgmentID)
	}
}

func TestFindCandidates_ResolvedConflictExcluded(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		FactIDs:  []string{"fact-saved", "fact-other"},
		Resolved: true,
	}
	b := newConflictBackend(t, s)
	savedID := b.idmap.IntFor(b.projID, "fact-saved")

	candidates, err := b.FindCandidates(savedID, store.CandidateOptions{})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("got %d candidates, want 0 (conflict already resolved)", len(candidates))
	}
}

func TestFindCandidates_NoConflicts_ReturnsEmptyNotError(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)
	savedID := b.idmap.IntFor(b.projID, "fact-saved")

	candidates, err := b.FindCandidates(savedID, store.CandidateOptions{})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("got %d candidates, want 0", len(candidates))
	}
}

func TestFindCandidates_UnmappedObservationID_ReturnsEmptyNotError(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)

	// An id never registered in the IDMap (e.g. a fresh backend that never
	// saved this observation) must fail-safe rather than error.
	candidates, err := b.FindCandidates(99999, store.CandidateOptions{})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if candidates != nil {
		t.Fatalf("candidates=%v, want nil", candidates)
	}
}

func TestFindCandidates_ScopeFilter(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:      "conf-1",
		FactIDs: []string{"fact-saved", "fact-other"},
	}
	s.facts["fact-other"] = Fact{
		ID:       "fact-other",
		Metadata: map[string]any{metaScope: "project-a"},
	}
	b := newConflictBackend(t, s)
	savedID := b.idmap.IntFor(b.projID, "fact-saved")

	candidates, err := b.FindCandidates(savedID, store.CandidateOptions{Scope: "project-b"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("got %d candidates, want 0 (scope mismatch)", len(candidates))
	}
}

func TestFindCandidates_LimitCaps(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:      "conf-1",
		FactIDs: []string{"fact-saved", "fact-a", "fact-b", "fact-c"},
	}
	s.facts["fact-a"] = Fact{ID: "fact-a"}
	s.facts["fact-b"] = Fact{ID: "fact-b"}
	s.facts["fact-c"] = Fact{ID: "fact-c"}
	b := newConflictBackend(t, s)
	savedID := b.idmap.IntFor(b.projID, "fact-saved")

	candidates, err := b.FindCandidates(savedID, store.CandidateOptions{Limit: 2})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 (Limit=2)", len(candidates))
	}
}

// ─── GetRelationsForObservations ─────────────────────────────────────────────

func TestGetRelationsForObservations_UnresolvedConflictIsPending(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:          "conf-1",
		Description: "these two facts disagree",
		FactIDs:     []string{"fact-a", "fact-b"},
		Resolved:    false,
	}
	b := newConflictBackend(t, s)

	rels, err := b.GetRelationsForObservations([]string{"fact-a"})
	if err != nil {
		t.Fatalf("GetRelationsForObservations: %v", err)
	}
	entry, ok := rels["fact-a"]
	if !ok {
		t.Fatalf("no entry for fact-a: %+v", rels)
	}
	if len(entry.AsSource) != 1 {
		t.Fatalf("AsSource len=%d, want 1", len(entry.AsSource))
	}
	if len(entry.AsTarget) != 0 {
		t.Fatalf("AsTarget len=%d, want 0 (MemoryLake conflicts have no directionality)", len(entry.AsTarget))
	}
	r := entry.AsSource[0]
	if r.Relation != store.RelationConflictsWith {
		t.Errorf("Relation=%q, want conflicts_with", r.Relation)
	}
	if r.JudgmentStatus != store.JudgmentStatusPending {
		t.Errorf("JudgmentStatus=%q, want pending", r.JudgmentStatus)
	}
	if r.TargetID != "fact-b" {
		t.Errorf("TargetID=%q, want fact-b", r.TargetID)
	}
	if r.Reason == nil || *r.Reason != "these two facts disagree" {
		t.Errorf("Reason=%v, want 'these two facts disagree'", r.Reason)
	}
}

func TestGetRelationsForObservations_ResolvedConflictIsJudgedButStaysConflictsWith(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		FactIDs:  []string{"fact-a", "fact-b"},
		Resolved: true,
	}
	b := newConflictBackend(t, s)

	rels, err := b.GetRelationsForObservations([]string{"fact-a"})
	if err != nil {
		t.Fatalf("GetRelationsForObservations: %v", err)
	}
	r := rels["fact-a"].AsSource[0]
	if r.JudgmentStatus != store.JudgmentStatusJudged {
		t.Errorf("JudgmentStatus=%q, want judged", r.JudgmentStatus)
	}
	if r.Relation != store.RelationConflictsWith {
		t.Errorf("Relation=%q, want conflicts_with (verb unrecoverable from resolved conflict — see doc comment)", r.Relation)
	}
}

func TestGetRelationsForObservations_EmptyInput(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)
	rels, err := b.GetRelationsForObservations(nil)
	if err != nil {
		t.Fatalf("GetRelationsForObservations: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("got %d entries, want 0", len(rels))
	}
}

func TestGetRelationsForObservations_NoMatchingConflict(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-x", "fact-y"}}
	b := newConflictBackend(t, s)

	rels, err := b.GetRelationsForObservations([]string{"fact-a"})
	if err != nil {
		t.Fatalf("GetRelationsForObservations: %v", err)
	}
	if _, ok := rels["fact-a"]; ok {
		t.Fatalf("expected no entry for fact-a, got %+v", rels["fact-a"])
	}
}

// ─── JudgeRelation ────────────────────────────────────────────────────────────

func TestJudgeRelation_Supersedes_ResolvesKeepFactWithNewerFact(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		Category: "m2m",
		FactIDs:  []string{"fact-old", "fact-new"},
	}
	s.facts["fact-old"] = Fact{ID: "fact-old", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	s.facts["fact-new"] = Fact{ID: "fact-new", CreatedAt: "2026-07-22T00:00:00Z", UpdatedAt: "2026-07-22T00:00:00Z"}
	b := newConflictBackend(t, s)

	rel, err := b.JudgeRelation(store.JudgeRelationParams{
		JudgmentID: "conf-1",
		Relation:   store.RelationSupersedes,
	})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}
	if rel == nil {
		t.Fatal("rel is nil")
	}
	if len(s.resolves) != 1 {
		t.Fatalf("got %d resolve calls, want 1", len(s.resolves))
	}
	if s.resolves[0].Strategy != resolveStrategyKeepFact {
		t.Errorf("Strategy=%q, want keep_fact", s.resolves[0].Strategy)
	}
	if s.resolves[0].KeepFactID != "fact-new" {
		t.Errorf("KeepFactID=%q, want fact-new (most recently updated)", s.resolves[0].KeepFactID)
	}
	if rel.JudgmentStatus != store.JudgmentStatusJudged {
		t.Errorf("JudgmentStatus=%q, want judged", rel.JudgmentStatus)
	}
}

func TestJudgeRelation_Supersedes_M2D_UsesTrustFact(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		Category: "m2d",
		FactIDs:  []string{"fact-1"},
	}
	s.facts["fact-1"] = Fact{ID: "fact-1"}
	b := newConflictBackend(t, s)

	_, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-1", Relation: store.RelationSupersedes})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}
	if s.resolves[0].Strategy != resolveStrategyTrustFact {
		t.Errorf("Strategy=%q, want trust_fact for m2d category", s.resolves[0].Strategy)
	}
}

func TestJudgeRelation_NotConflict_ResolvesDismiss(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
	b := newConflictBackend(t, s)

	rel, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-1", Relation: store.RelationNotConflict})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}
	if len(s.resolves) != 1 || s.resolves[0].Strategy != resolveStrategyDismiss {
		t.Fatalf("resolves=%+v, want one dismiss call", s.resolves)
	}
	if rel.JudgmentStatus != store.JudgmentStatusJudged {
		t.Errorf("JudgmentStatus=%q, want judged", rel.JudgmentStatus)
	}
}

// TestJudgeRelation_ConflictsWithIsNoOp verifies the documented no-op: no
// resolve() call is made, and the conflict remains unresolved.
func TestJudgeRelation_ConflictsWithIsNoOp(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
	b := newConflictBackend(t, s)

	rel, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-1", Relation: store.RelationConflictsWith})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}
	if len(s.resolves) != 0 {
		t.Fatalf("got %d resolve calls, want 0 (conflicts_with must be a no-op)", len(s.resolves))
	}
	if rel == nil || rel.Relation != store.RelationConflictsWith {
		t.Fatalf("rel=%+v, want non-nil with Relation=conflicts_with", rel)
	}
	if rel.JudgmentStatus != store.JudgmentStatusPending {
		t.Errorf("JudgmentStatus=%q, want pending (nothing was actually judged server-side)", rel.JudgmentStatus)
	}
	if s.conflicts["conf-1"].Resolved {
		t.Error("conflict was resolved server-side, want untouched")
	}
}

// TestJudgeRelation_RelatedCompatibleScopedAreNoOps verifies the three verbs
// with no MemoryLake analogue never call resolve().
func TestJudgeRelation_RelatedCompatibleScopedAreNoOps(t *testing.T) {
	for _, verb := range []string{store.RelationRelated, store.RelationCompatible, store.RelationScoped} {
		t.Run(verb, func(t *testing.T) {
			s := newConflictTestServer()
			s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
			b := newConflictBackend(t, s)

			rel, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-1", Relation: verb})
			if err != nil {
				t.Fatalf("JudgeRelation(%s): %v", verb, err)
			}
			if len(s.resolves) != 0 {
				t.Fatalf("verb=%s: got %d resolve calls, want 0", verb, len(s.resolves))
			}
			if rel == nil || rel.Relation != verb {
				t.Fatalf("verb=%s: rel=%+v", verb, rel)
			}
		})
	}
}

func TestJudgeRelation_InvalidVerb_Errors(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
	b := newConflictBackend(t, s)

	_, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-1", Relation: "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid relation verb")
	}
}

// TestJudgeRelation_CrossProjectConflict_Errors verifies the structural
// cross-project guard: a conflict id that only exists under a different
// project_id 404s through getConflictByID (which always queries with this
// backend's own b.projID), so JudgeRelation errors rather than silently
// resolving a conflict from another project.
func TestJudgeRelation_CrossProjectConflict_Errors(t *testing.T) {
	s := newConflictTestServer()
	// No conflicts registered under proj-1 at all — simulates a conflict id
	// that belongs to some other project.
	b := newConflictBackend(t, s)

	_, err := b.JudgeRelation(store.JudgeRelationParams{JudgmentID: "conf-other-project", Relation: store.RelationSupersedes})
	if err == nil {
		t.Fatal("expected error for unknown/cross-project conflict id")
	}
}

// ─── JudgeBySemantic ──────────────────────────────────────────────────────────

func TestJudgeBySemantic_Supersedes_ResolvesKeepFactWithSourceID(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{
		ID:       "conf-1",
		Category: "m2m",
		FactIDs:  []string{"fact-a", "fact-b"},
	}
	b := newConflictBackend(t, s)

	syncID, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID:   "fact-a",
		TargetID:   "fact-b",
		Relation:   store.RelationSupersedes,
		Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID != "conf-1" {
		t.Errorf("syncID=%q, want conf-1", syncID)
	}
	if len(s.resolves) != 1 {
		t.Fatalf("got %d resolve calls, want 1", len(s.resolves))
	}
	if s.resolves[0].Strategy != resolveStrategyKeepFact || s.resolves[0].KeepFactID != "fact-a" {
		t.Errorf("resolve=%+v, want strategy=keep_fact keep_fact_id=fact-a (SourceID)", s.resolves[0])
	}
}

func TestJudgeBySemantic_NotConflict_IsNoOp(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
	b := newConflictBackend(t, s)

	syncID, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID: "fact-a", TargetID: "fact-b", Relation: store.RelationNotConflict, Confidence: 0.5,
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID != "" {
		t.Errorf("syncID=%q, want empty (no-op)", syncID)
	}
	if len(s.resolves) != 0 {
		t.Fatalf("got %d resolve calls, want 0", len(s.resolves))
	}
}

// TestJudgeBySemantic_NoMatchingMemoryLakeConflict_FailsSafe verifies the
// deepest unmappable gap: an engram-detected pair with no MemoryLake
// conflict counterpart degrades to a benign empty result, not an error.
func TestJudgeBySemantic_NoMatchingMemoryLakeConflict_FailsSafe(t *testing.T) {
	s := newConflictTestServer()
	// No conflicts registered at all — MemoryLake's own detector never
	// flagged this pair.
	b := newConflictBackend(t, s)

	syncID, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID: "fact-a", TargetID: "fact-b", Relation: store.RelationSupersedes, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID != "" {
		t.Errorf("syncID=%q, want empty (fail-safe no-op)", syncID)
	}
	if len(s.resolves) != 0 {
		t.Fatalf("got %d resolve calls, want 0", len(s.resolves))
	}
}

func TestJudgeBySemantic_AlreadyResolvedConflict_IsNoOp(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}, Resolved: true}
	b := newConflictBackend(t, s)

	syncID, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID: "fact-a", TargetID: "fact-b", Relation: store.RelationSupersedes, Confidence: 0.9,
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID != "" {
		t.Errorf("syncID=%q, want empty (already resolved)", syncID)
	}
	if len(s.resolves) != 0 {
		t.Fatalf("got %d resolve calls, want 0", len(s.resolves))
	}
}

func TestJudgeBySemantic_MissingSourceOrTarget_Errors(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)

	if _, err := b.JudgeBySemantic(store.JudgeBySemanticParams{TargetID: "fact-b", Relation: store.RelationSupersedes}); err == nil {
		t.Error("expected error for missing SourceID")
	}
	if _, err := b.JudgeBySemantic(store.JudgeBySemanticParams{SourceID: "fact-a", Relation: store.RelationSupersedes}); err == nil {
		t.Error("expected error for missing TargetID")
	}
}

func TestJudgeBySemantic_ConfidenceOutOfRange_Errors(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)

	_, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID: "fact-a", TargetID: "fact-b", Relation: store.RelationSupersedes, Confidence: 1.5,
	})
	if err == nil {
		t.Fatal("expected error for out-of-range confidence")
	}
}

func TestJudgeBySemantic_InvalidVerb_Errors(t *testing.T) {
	s := newConflictTestServer()
	b := newConflictBackend(t, s)

	_, err := b.JudgeBySemantic(store.JudgeBySemanticParams{SourceID: "fact-a", TargetID: "fact-b", Relation: "bogus"})
	if err == nil {
		t.Fatal("expected error for invalid relation verb")
	}
}

// TestJudgeBySemantic_RelatedIsNoOpButReturnsConflictID mirrors the
// JudgeRelation no-op verbs, confirmed here for the semantic entry point too.
func TestJudgeBySemantic_RelatedIsNoOpButReturnsConflictID(t *testing.T) {
	s := newConflictTestServer()
	s.conflicts["conf-1"] = conflictItem{ID: "conf-1", FactIDs: []string{"fact-a", "fact-b"}}
	b := newConflictBackend(t, s)

	syncID, err := b.JudgeBySemantic(store.JudgeBySemanticParams{
		SourceID: "fact-a", TargetID: "fact-b", Relation: store.RelationRelated, Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("JudgeBySemantic: %v", err)
	}
	if syncID != "conf-1" {
		t.Errorf("syncID=%q, want conf-1", syncID)
	}
	if len(s.resolves) != 0 {
		t.Fatalf("got %d resolve calls, want 0 (related is a no-op)", len(s.resolves))
	}
}
