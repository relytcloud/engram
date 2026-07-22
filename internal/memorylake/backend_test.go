package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// memoryBackend is a local copy of the method set declared by
// internal/mcp.MemoryBackend. internal/memorylake must not import internal/mcp
// (that would create an import cycle), so instead of asserting against the real
// interface here, we assert against this byte-for-byte copy of its signatures.
// Task 10 (in the mcp package) performs the real `var _ mcp.MemoryBackend`
// assertion; this local check guarantees *MemoryLakeBackend stays assignable to
// it by keeping the signatures aligned.
type memoryBackend interface {
	AddObservation(p store.AddObservationParams) (int64, error)
	GetObservation(id int64) (*store.Observation, error)
	UpdateObservation(id int64, p store.UpdateObservationParams) (*store.Observation, error)
	DeleteObservation(id int64, hardDelete bool) error
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
	Timeline(observationID int64, before, after int) (*store.TimelineResult, error)
	FormatContext(project, scope string) (string, error)
	Stats() (*store.Stats, error)
	MaxObservationLength() int

	PinObservation(id int64) error
	UnpinObservation(id int64) error
	ObservationsNeedingReview(project string, limit int) ([]store.Observation, error)
	MarkReviewed(id int64) error

	CreateSession(id, project, directory string) error
	GetSession(id string) (*store.Session, error)
	EndSession(id string, summary string) error
	MostRecentActiveSession(project string) (string, bool, error)
	RecentSessions(project string, limit int) ([]store.SessionSummary, error)

	AddPrompt(p store.AddPromptParams) (int64, error)
	AddPromptIfMissing(p store.AddPromptParams) (int64, bool, error)
	PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error)

	ProjectExists(name string) (bool, error)
	ListProjectNames() ([]string, error)
	CountObservationsForProject(name string) (int, error)
	MergeProjects(sources []string, canonical string) (*store.MergeResult, error)

	FindCandidates(savedID int64, opts store.CandidateOptions) ([]store.Candidate, error)
	GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error)
	JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error)
	JudgeBySemantic(p store.JudgeBySemanticParams) (string, error)
}

// Compile-time self-check: *MemoryLakeBackend must satisfy the same method set
// as internal/mcp.MemoryBackend (mirrored above).
var _ memoryBackend = (*MemoryLakeBackend)(nil)

// newTestBackend builds a MemoryLakeBackend wired to srvURL with a fresh,
// temp-file-backed IDMap and fast polling so backfill loops resolve in
// milliseconds instead of production-scale intervals.
func newTestBackend(t *testing.T, srvURL string) *MemoryLakeBackend {
	t.Helper()
	cfg := Config{BaseURL: srvURL, APIKey: "sk-test", TimeoutMS: 5000}
	idmap, err := LoadIDMap(t.TempDir() + "/idmap.json")
	if err != nil {
		t.Fatalf("LoadIDMap: %v", err)
	}
	topics, err := LoadTopicIndex(t.TempDir() + "/topics.json")
	if err != nil {
		t.Fatalf("LoadTopicIndex: %v", err)
	}
	sessions, err := LoadSessionIndex(t.TempDir() + "/sessions.json")
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	return &MemoryLakeBackend{
		client:   NewClient(cfg),
		cfg:      cfg,
		ws:       "ws-1",
		projID:   "proj-1",
		actorID:  "actor-1",
		idmap:    idmap,
		topics:   topics,
		sessions: sessions,
		poll:     1 * time.Millisecond,
		maxWait:  500 * time.Millisecond,
	}
}

// TestBackend_AddObservation_ReturnsInt64ViaBackfill drives the core write
// path: AppendObservation posts a message, BackfillFacts finds the extracted
// fact, and AddObservation returns the int64 the IDMap assigns that fact id.
func TestBackend_AddObservation_ReturnsInt64ViaBackfill(t *testing.T) {
	var patched bool
	var appended int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.StoreInt32(&appended, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			// The pre-append snapshot GET must see an empty project so fact-1
			// (extracted from *this* message) is treated as new, not stale.
			var items []map[string]any
			if atomic.LoadInt32(&appended) == 1 {
				items = []map[string]any{
					{"id": "fact-1", "fact": "extracted", "metadata": map[string]any{}},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1":
			patched = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "extracted", "metadata": map[string]any{metaObsID: "obs"},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id, err := b.AddObservation(store.AddObservationParams{
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "some content", Scope: "global",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id != 1 {
		t.Fatalf("id=%d, want 1 (first IDMap assignment for fact-1)", id)
	}
	if !patched {
		t.Fatal("expected backfill PATCH onto fact-1")
	}
	// The returned int must reverse-map back to (this project, fact-1).
	if pid, f, ok := b.idmap.FactFor(1); !ok || pid != b.projID || f != "fact-1" {
		t.Fatalf("FactFor(1)=%q/%q,%v; want %s/fact-1,true", pid, f, ok, b.projID)
	}
}

// TestBackend_AddObservation_TimeoutReturnsProvisional verifies the pending
// path: when extraction produces no fact before maxWait, AddObservation still
// succeeds (err=nil) returning a provisional int keyed off the message id.
func TestBackend_AddObservation_TimeoutReturnsProvisional(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-9"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			// No facts extracted yet — backfill will time out.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	b.maxWait = 20 * time.Millisecond
	id, err := b.AddObservation(store.AddObservationParams{SessionID: "s", Content: "c"})
	if err != nil {
		t.Fatalf("AddObservation (timeout): unexpected error %v", err)
	}
	if id == 0 {
		t.Fatal("expected a provisional non-zero id on extraction timeout")
	}
	if pid, f, ok := b.idmap.FactFor(id); !ok || pid != b.projID || f != "msg-9" {
		t.Fatalf("FactFor(%d)=%q/%q,%v; want %s/provisional msg-9,true", id, pid, f, ok, b.projID)
	}
}

// TestBackend_AddObservation_DoesNotClaimPreExistingUnmarkedFact is the FIX #1
// regression: MemoryLake extraction is asynchronous and bounded here, so an
// earlier save can leave an unmarked fact behind (its own backfill timed out).
// A later AddObservation must NOT claim that stale unmarked fact and stamp its
// own engram_raw onto it (which would corrupt the earlier observation on
// read-back). AddObservation snapshots the project's fact ids before appending
// its message, and BackfillFacts only claims facts absent from that snapshot.
//
// Scenario: the project already has one leftover unmarked fact ("stale-fact")
// at snapshot time; after this message is appended a new fact ("new-fact") is
// extracted. Only new-fact must be PATCHed with this call's metadata.
func TestBackend_AddObservation_DoesNotClaimPreExistingUnmarkedFact(t *testing.T) {
	var appended int32
	var patchedIDs []string
	staleFact := map[string]any{"id": "stale-fact", "fact": "an earlier observation, unmarked", "metadata": map[string]any{}}
	newFact := map[string]any{"id": "new-fact", "fact": "this observation", "metadata": map[string]any{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.StoreInt32(&appended, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			// Before append: only the stale leftover fact exists (the snapshot).
			// After append: MemoryLake has extracted the new fact too.
			items := []map[string]any{staleFact}
			if atomic.LoadInt32(&appended) == 1 {
				items = []map[string]any{staleFact, newFact}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/")
			patchedIDs = append(patchedIDs, id)
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": id, "fact": "this observation", "metadata": body.Metadata,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	_, err := b.AddObservation(store.AddObservationParams{
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "this observation, verbatim", Scope: "global",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if len(patchedIDs) != 1 || patchedIDs[0] != "new-fact" {
		t.Fatalf("patchedIDs=%v, want exactly [new-fact] (stale-fact must not be claimed)", patchedIDs)
	}
}

// TestBackend_AddObservation_ConcurrentSavesNoCrossClaim is the FIX B
// regression: two concurrent AddObservation calls on the SAME backend must not
// let one save claim (and stamp its engram_raw onto) the fact extracted from
// the other save's message. The window exists because AddObservation is a
// snapshot→append→backfill sequence: unserialized, both calls can snapshot
// before either appends, so each sees the other's freshly-extracted fact as
// "new and unmarked" and races to PATCH it. A per-backend write mutex
// serializes the whole sequence, closing the window.
//
// The mock is stateful: appending a message synchronously "extracts" exactly
// one fact whose text equals that message's content and whose metadata starts
// empty; backfill then PATCHes engram metadata onto it. The invariant checked
// at the end is that every fact's engram_raw equals its own fact text — i.e. no
// fact carries another save's verbatim content. Runs under `-race`.
func TestBackend_AddObservation_ConcurrentSavesNoCrossClaim(t *testing.T) {
	type mockFact struct {
		id       string
		fact     string
		metadata map[string]any
	}

	var mu sync.Mutex
	facts := map[string]*mockFact{} // fact id → fact

	factsSnapshot := func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := make([]map[string]any, 0, len(facts))
		for _, f := range facts {
			md := map[string]any{}
			for k, v := range f.metadata {
				md[k] = v
			}
			out = append(out, map[string]any{"id": f.id, "fact": f.fact, "metadata": md})
		}
		return out
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})

		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			// Parse the appended content and synchronously extract one fact
			// keyed by (and carrying) that content, unmarked.
			var body struct {
				CustomID string `json:"custom_id"`
				Content  []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			text := ""
			if len(body.Content) > 0 {
				text = body.Content[0].Text
			}
			factID := "fact-" + contentHash(text)
			mu.Lock()
			if _, exists := facts[factID]; !exists {
				facts[factID] = &mockFact{id: factID, fact: text, metadata: map[string]any{}}
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-" + body.CustomID}})

		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": factsSnapshot()}})

		case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/")
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			f, ok := facts[id]
			if ok {
				for k, v := range body.Metadata {
					f.metadata[k] = v
				}
			}
			var echoFact string
			var echoMD map[string]any
			if ok {
				echoFact = f.fact
				echoMD = map[string]any{}
				for k, v := range f.metadata {
					echoMD[k] = v
				}
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": id, "fact": echoFact, "metadata": echoMD,
			}})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	b.poll = 1 * time.Millisecond
	b.maxWait = 2 * time.Second

	contents := []string{
		"observation ALPHA, verbatim and distinct",
		"observation BRAVO, verbatim and distinct",
		"observation CHARLIE, verbatim and distinct",
		"observation DELTA, verbatim and distinct",
	}

	var wg sync.WaitGroup
	errs := make([]error, len(contents))
	for i, c := range contents {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			_, errs[i] = b.AddObservation(store.AddObservationParams{
				SessionID: "sess-1", Type: "note", Title: "t", Content: c, Scope: "global",
			})
		}(i, c)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("AddObservation[%d]: %v", i, err)
		}
	}

	// Invariant: every fact's engram_raw must equal its own fact text. A
	// cross-claim would stamp one save's content onto another save's fact,
	// breaking this.
	mu.Lock()
	defer mu.Unlock()
	if len(facts) != len(contents) {
		t.Fatalf("expected %d distinct facts, got %d", len(contents), len(facts))
	}
	for id, f := range facts {
		raw, _ := f.metadata[metaRaw].(string)
		if raw == "" {
			t.Fatalf("fact %s (%q) was never backfilled with engram_raw", id, f.fact)
		}
		if raw != f.fact {
			t.Fatalf("fact %s cross-claimed: engram_raw=%q but fact text=%q (a concurrent save hijacked this fact)", id, raw, f.fact)
		}
	}
}

// TestBackend_GetObservation_ViaIDMap verifies id → fact id → fact →
// Observation, with Content sourced from engram_raw metadata.
func TestBackend_GetObservation_ViaIDMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-7" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id":   "fact-7",
				"fact": "paraphrase",
				"metadata": map[string]any{
					metaRaw: "verbatim content", metaTitle: "title", metaType: "note", metaScope: "global",
				},
				"created_at": "2026-07-22T00:00:00Z",
				"updated_at": "2026-07-22T01:00:00Z",
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id := b.idmap.IntFor(b.projID, "fact-7") // seed mapping (id=1)

	obs, err := b.GetObservation(id)
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs.ID != id {
		t.Fatalf("obs.ID=%d, want %d", obs.ID, id)
	}
	if obs.Content != "verbatim content" {
		t.Fatalf("Content=%q, want verbatim content", obs.Content)
	}
	if obs.CreatedAt != "2026-07-22T00:00:00Z" || obs.UpdatedAt != "2026-07-22T01:00:00Z" {
		t.Fatalf("timestamps not carried through: %q / %q", obs.CreatedAt, obs.UpdatedAt)
	}
}

// TestBackend_GetObservation_UnknownID errors when the id has no fact mapping.
func TestBackend_GetObservation_UnknownID(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	if _, err := b.GetObservation(999); err == nil {
		t.Fatal("expected error for unmapped id")
	}
}

// TestBackend_UpdateObservation merges metadata and sends the new text via
// the V3 `fact` field on PATCH (not `content`).
func TestBackend_UpdateObservation(t *testing.T) {
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-2"
		switch {
		case r.Method == "GET" && r.URL.Path == p:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-2", "fact": "old",
				"metadata": map[string]any{metaRaw: "old content", metaTitle: "old title", metaType: "note"},
			}})
		case r.Method == "PATCH" && r.URL.Path == p:
			json.NewDecoder(r.Body).Decode(&patchBody)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-2", "fact": "new content",
				"metadata": map[string]any{metaRaw: "new content", metaTitle: "new title", metaType: "note"},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id := b.idmap.IntFor(b.projID, "fact-2")

	newTitle := "new title"
	newContent := "new content"
	obs, err := b.UpdateObservation(id, store.UpdateObservationParams{Title: &newTitle, Content: &newContent})
	if err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}
	if obs.Content != "new content" {
		t.Fatalf("Content=%q, want new content", obs.Content)
	}
	if patchBody["fact"] != "new content" {
		t.Fatalf("PATCH fact=%v, want new content", patchBody["fact"])
	}
	if _, hasContent := patchBody["content"]; hasContent {
		t.Fatalf("PATCH body must not send a `content` field (V3 uses `fact`): %v", patchBody)
	}
	md, _ := patchBody["metadata"].(map[string]any)
	if md[metaRaw] != "new content" || md[metaTitle] != "new title" {
		t.Fatalf("PATCH metadata not merged correctly: %v", md)
	}
	if md[metaType] != "note" {
		t.Fatalf("PATCH metadata dropped preserved key engram_type: %v", md)
	}
}

// TestBackend_DeleteObservation_CallsForget verifies soft-delete maps to the
// V3 forget endpoint (both for soft and hard delete requests).
func TestBackend_DeleteObservation_CallsForget(t *testing.T) {
	for _, hard := range []bool{false, true} {
		var forgot bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-3/forget" {
				forgot = true
				json.NewEncoder(w).Encode(map[string]any{"success": true})
				return
			}
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}))
		b := newTestBackend(t, srv.URL)
		id := b.idmap.IntFor(b.projID, "fact-3")
		if err := b.DeleteObservation(id, hard); err != nil {
			t.Fatalf("DeleteObservation(hard=%v): %v", hard, err)
		}
		if !forgot {
			t.Fatalf("hard=%v: expected forget POST", hard)
		}
		srv.Close()
	}
}

// TestBackend_Search_PassesThrough verifies Search delegates to SearchFacts.
func TestBackend_Search_PassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{
					{"id": "fact-1", "fact": "hit", "score": 0.9, "metadata": map[string]any{metaRaw: "content"}},
				},
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	res, err := b.Search("query", store.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 || res[0].Content != "content" {
		t.Fatalf("Search result=%+v, want one hit with content", res)
	}
}

// TestBackend_PinObservation_PatchesMetadataPinned verifies pin flips the
// pinned metadata flag while preserving existing metadata.
func TestBackend_PinObservation_PatchesMetadataPinned(t *testing.T) {
	var md map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-4"
		switch {
		case r.Method == "GET" && r.URL.Path == p:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-4", "fact": "f", "metadata": map[string]any{metaRaw: "keep"},
			}})
		case r.Method == "PATCH" && r.URL.Path == p:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			md, _ = body["metadata"].(map[string]any)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "fact-4"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id := b.idmap.IntFor(b.projID, "fact-4")
	if err := b.PinObservation(id); err != nil {
		t.Fatalf("PinObservation: %v", err)
	}
	if md["pinned"] != true {
		t.Fatalf("metadata.pinned=%v, want true", md["pinned"])
	}
	if md[metaRaw] != "keep" {
		t.Fatalf("pin dropped preserved metadata: %v", md)
	}
}

// TestBackend_MergeProjects_Unsupported verifies merge is explicitly rejected.
func TestBackend_MergeProjects_Unsupported(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	res, err := b.MergeProjects([]string{"a"}, "b")
	if err == nil {
		t.Fatal("expected merge to be unsupported")
	}
	if res != nil {
		t.Fatalf("expected nil result, got %+v", res)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "merg") {
		t.Fatalf("error %q should mention merging", err.Error())
	}
}

// TestBackend_MaxObservationLength matches the store default (50000).
func TestBackend_MaxObservationLength(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	if got := b.MaxObservationLength(); got != 50000 {
		t.Fatalf("MaxObservationLength=%d, want 50000 (store default)", got)
	}
}

// TestNewBackend exercises the constructor's resolve → actor → idmap flow.
func TestNewBackend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var actorCreated, bound bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			actorCreated = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			bound = true
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := Config{
		BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000,
		Actor: "cli-machine", ExtractPollMS: 2000, ExtractMaxWaitMS: 30000,
	}
	idmap, err := LoadIDMap(t.TempDir() + "/idmap.json")
	if err != nil {
		t.Fatalf("LoadIDMap: %v", err)
	}
	b, err := NewBackend(cfg, "ws-1", "proj-42", idmap)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if !actorCreated || !bound {
		t.Fatalf("expected actor creation+bind; created=%v bound=%v", actorCreated, bound)
	}
	if b.ws != "ws-1" || b.projID != "proj-42" || b.actorID != "actor-x" {
		t.Fatalf("backend fields wrong: ws=%q proj=%q actor=%q", b.ws, b.projID, b.actorID)
	}
	if b.poll != 2000*time.Millisecond || b.maxWait != 30000*time.Millisecond {
		t.Fatalf("poll/maxWait not derived from cfg: %v / %v", b.poll, b.maxWait)
	}
}

// ─── Task 12: topic_key upsert ───────────────────────────────────────────────

// TestBackend_AddObservation_TopicKeyUpsertHit_PatchesInPlace verifies that
// when the TopicIndex already has a fact for project+scope+topic_key,
// AddObservation PATCHes that fact directly and never appends a conversation
// message (no POST .../messages, no snapshot GET for backfill) — the whole
// point of the upsert fast path.
func TestBackend_AddObservation_TopicKeyUpsertHit_PatchesInPlace(t *testing.T) {
	var patchCount, appendCount int32
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-topic"
		switch {
		case r.Method == "GET" && r.URL.Path == p:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-topic", "fact": "old content",
				"metadata": map[string]any{
					metaRaw: "old content", metaTitle: "old title", metaType: "decision",
					metaScope: "global", metaTopicKey: "arch/db", metaRev: float64(2), "pinned": true,
				},
			}})
		case r.Method == "PATCH" && r.URL.Path == p:
			atomic.AddInt32(&patchCount, 1)
			json.NewDecoder(r.Body).Decode(&patchBody)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-topic", "fact": "new content", "metadata": patchBody["metadata"],
			}})
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/messages"):
			atomic.AddInt32(&appendCount, 1)
			t.Fatalf("topic_key upsert hit must not append a new message, got POST %s", r.URL.Path)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if err := b.topics.Put("proj", "global", "arch/db", "fact-topic"); err != nil {
		t.Fatalf("seed TopicIndex: %v", err)
	}

	id, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		Type: "decision", Title: "new title", Content: "new content",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if patchCount != 1 {
		t.Fatalf("PATCH count=%d, want exactly 1", patchCount)
	}
	if appendCount != 0 {
		t.Fatalf("append count=%d, want 0 (must not append on upsert hit)", appendCount)
	}
	if id != b.idmap.IntFor(b.projID, "fact-topic") {
		t.Fatalf("id=%d, want the int64 mapped to fact-topic", id)
	}

	md, _ := patchBody["metadata"].(map[string]any)
	if md[metaRaw] != "new content" {
		t.Fatalf("PATCH metadata.engram_raw=%v, want new content", md[metaRaw])
	}
	if md[metaTitle] != "new title" {
		t.Fatalf("PATCH metadata.engram_title=%v, want new title", md[metaTitle])
	}
	revF, ok := md[metaRev].(float64)
	if !ok || int(revF) != 3 {
		t.Fatalf("PATCH metadata.engram_rev=%v, want 3 (2+1)", md[metaRev])
	}
	if md["pinned"] != true {
		t.Fatalf("PATCH must preserve unrelated metadata keys (pinned): %v", md)
	}
	if patchBody["fact"] != "new content" {
		t.Fatalf("PATCH fact=%v, want new content", patchBody["fact"])
	}
}

// TestBackend_AddObservation_TopicKeyUpsertMiss_RecordsIndex verifies that
// when the TopicIndex has no entry for project+scope+topic_key,
// AddObservation falls through to the normal append+backfill path and then
// records the first backfilled fact into the TopicIndex so a subsequent save
// with the same key upserts instead of appending again.
func TestBackend_AddObservation_TopicKeyUpsertMiss_RecordsIndex(t *testing.T) {
	var appended int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.StoreInt32(&appended, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var items []map[string]any
			if atomic.LoadInt32(&appended) == 1 {
				items = []map[string]any{
					{"id": "fact-new", "fact": "extracted", "metadata": map[string]any{}},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-new":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-new", "fact": "extracted", "metadata": map[string]any{metaObsID: "obs"},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if _, ok := b.topics.Lookup("proj", "global", "arch/db"); ok {
		t.Fatal("TopicIndex must start empty for this key")
	}

	id, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "some content",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id != b.idmap.IntFor(b.projID, "fact-new") {
		t.Fatalf("id=%d, want the int64 mapped to fact-new", id)
	}

	factID, ok := b.topics.Lookup("proj", "global", "arch/db")
	if !ok || factID != "fact-new" {
		t.Fatalf("TopicIndex.Lookup after miss = (%q, %v), want (fact-new, true)", factID, ok)
	}
}

// TestBackend_AddObservation_TopicKeyUpsert_SecondSaveHitsIndex is an
// end-to-end check that a miss followed by a second save with the same key
// upserts (PATCH only, no second append).
func TestBackend_AddObservation_TopicKeyUpsert_SecondSaveHitsIndex(t *testing.T) {
	var appendCount int32
	var patchCount int32
	factsExtracted := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1"
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&appendCount, 1)
			factsExtracted = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var items []map[string]any
			if factsExtracted {
				items = []map[string]any{{"id": "fact-1", "fact": "extracted", "metadata": map[string]any{}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "GET" && r.URL.Path == p:
			// Realistic stamped metadata: the first save's backfill (see
			// FactMetadata) would have recorded this fact's scope/topic_key,
			// which the second save's hit-validation (isValidTopicKeyHit,
			// task-12 hardening brief C1) now checks before trusting the
			// TopicIndex hit.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "v1", "metadata": map[string]any{
					metaRaw: "v1", metaObsID: "obs", metaScope: "global", metaTopicKey: "arch/db",
				},
			}})
		case r.Method == "PATCH":
			atomic.AddInt32(&patchCount, 1)
			var body struct {
				Metadata map[string]any `json:"metadata"`
				Fact     string         `json:"fact"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": body.Fact, "metadata": body.Metadata,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	id1, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "v1",
	})
	if err != nil {
		t.Fatalf("AddObservation (first): %v", err)
	}
	if appendCount != 1 {
		t.Fatalf("append count after first save=%d, want 1", appendCount)
	}

	id2, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		Type: "decision", Title: "t", Content: "v2",
	})
	if err != nil {
		t.Fatalf("AddObservation (second): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("second save returned a different id: %d vs %d (should upsert same fact)", id1, id2)
	}
	if appendCount != 1 {
		t.Fatalf("append count after second save=%d, want still 1 (no new message)", appendCount)
	}
	if patchCount < 1 {
		t.Fatalf("expected at least one PATCH for the second (upsert) save, got %d", patchCount)
	}
}

// ─── Task 12: ProjectExists / ListProjectNames ───────────────────────────────

func projectListServer(t *testing.T, items []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
}

func TestBackend_ProjectExists_TrueWhenMatchedByCustomIDOrName(t *testing.T) {
	srv := projectListServer(t, []map[string]any{
		{"id": "p-1", "custom_id": "alpha", "name": "Alpha Project"},
		{"id": "p-2", "custom_id": "", "name": "beta"},
	})
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	for _, name := range []string{"alpha", "beta"} {
		ok, err := b.ProjectExists(name)
		if err != nil {
			t.Fatalf("ProjectExists(%q): %v", name, err)
		}
		if !ok {
			t.Fatalf("ProjectExists(%q)=false, want true", name)
		}
	}
}

func TestBackend_ProjectExists_FalseWhenUnknown(t *testing.T) {
	srv := projectListServer(t, []map[string]any{
		{"id": "p-1", "custom_id": "alpha", "name": "Alpha Project"},
	})
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	ok, err := b.ProjectExists("does-not-exist")
	if err != nil {
		t.Fatalf("ProjectExists: %v", err)
	}
	if ok {
		t.Fatal("ProjectExists(unknown)=true, want false")
	}
}

func TestBackend_ListProjectNames_ReturnsCustomIDsFallingBackToName(t *testing.T) {
	srv := projectListServer(t, []map[string]any{
		{"id": "p-1", "custom_id": "alpha", "name": "Alpha Project"},
		{"id": "p-2", "custom_id": "", "name": "beta"},
	})
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	names, err := b.ListProjectNames()
	if err != nil {
		t.Fatalf("ListProjectNames: %v", err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("names=%v, want [alpha beta]", names)
	}
}

// ─── Task 12: Stats / CountObservationsForProject (paginated, expired-excluding) ─

// factsPageServer serves GET .../facts across two pages (continuation_token)
// so tests exercise listAllFacts' pagination loop, and excludes/includes an
// expired fact to verify Stats/CountObservationsForProject skip it.
func factsPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	page1 := []map[string]any{
		{"id": "fact-1", "fact": "a", "expired": false, "created_at": "2026-07-20T00:00:00Z"},
		{"id": "fact-2", "fact": "b", "expired": true, "created_at": "2026-07-20T01:00:00Z"},
	}
	page2 := []map[string]any{
		{"id": "fact-3", "fact": "c", "expired": false, "created_at": "2026-07-20T02:00:00Z"},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			// Stats() also lists project names; not the focus of this test, so
			// just answer with an empty project list.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts"):
			if r.URL.Query().Get("continuation_token") == "next" {
				json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": page2, "continuation_token": ""}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": page1, "continuation_token": "next"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestBackend_Stats_CountsAcrossPagesExcludingExpired(t *testing.T) {
	srv := factsPageServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	stats, err := b.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// 3 facts total across 2 pages, 1 expired (fact-2) excluded => 2.
	if stats.TotalObservations != 2 {
		t.Fatalf("TotalObservations=%d, want 2 (3 facts across pages, 1 expired excluded)", stats.TotalObservations)
	}
}

func TestBackend_CountObservationsForProject_ExcludesExpired(t *testing.T) {
	srv := factsPageServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	n, err := b.CountObservationsForProject("ignored")
	if err != nil {
		t.Fatalf("CountObservationsForProject: %v", err)
	}
	if n != 2 {
		t.Fatalf("count=%d, want 2", n)
	}
}

// ─── Task 12: Timeline ───────────────────────────────────────────────────────

func TestBackend_Timeline_OrdersByCreatedAtAndExcludesExpiredNeighbors(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-a", "fact": "A", "created_at": "2026-07-20T00:00:00Z", "metadata": map[string]any{metaRaw: "A"}},
		{"id": "fact-b", "fact": "B", "expired": true, "created_at": "2026-07-20T00:30:00Z", "metadata": map[string]any{metaRaw: "B"}},
		{"id": "fact-c", "fact": "C", "created_at": "2026-07-20T01:00:00Z", "metadata": map[string]any{metaRaw: "C"}},
		{"id": "fact-d", "fact": "D", "created_at": "2026-07-20T02:00:00Z", "metadata": map[string]any{metaRaw: "D"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)
	anchorID := b.idmap.IntFor(b.projID, "fact-c")

	res, err := b.Timeline(anchorID, 5, 5)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if res.Focus.Content != "C" {
		t.Fatalf("Focus.Content=%q, want C", res.Focus.Content)
	}
	// fact-b is expired and must be excluded from Before, leaving only fact-a.
	if len(res.Before) != 1 || res.Before[0].Content != "A" {
		t.Fatalf("Before=%+v, want exactly [A] (expired fact-b excluded)", res.Before)
	}
	if len(res.After) != 1 || res.After[0].Content != "D" {
		t.Fatalf("After=%+v, want exactly [D]", res.After)
	}
	if res.TotalInRange != 3 {
		t.Fatalf("TotalInRange=%d, want 3", res.TotalInRange)
	}
}

// ─── Task 12: FormatContext ──────────────────────────────────────────────────

func TestBackend_FormatContext_PinnedFirstThenRecentExcludingExpired(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-1", "fact": "f1", "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaRaw: "unpinned content", metaTitle: "U", metaType: "note", metaScope: "global"}},
		{"id": "fact-2", "fact": "f2", "created_at": "2026-07-20T01:00:00Z",
			"metadata": map[string]any{metaRaw: "pinned content", metaTitle: "P", metaType: "decision", metaScope: "global", "pinned": true}},
		{"id": "fact-3", "fact": "f3", "expired": true, "created_at": "2026-07-20T02:00:00Z",
			"metadata": map[string]any{metaRaw: "expired content", metaTitle: "E", metaType: "note", metaScope: "global"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	out, err := b.FormatContext("proj", "global")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if strings.Contains(out, "expired content") {
		t.Fatalf("FormatContext must exclude expired facts:\n%s", out)
	}
	pinnedIdx := strings.Index(out, "pinned content")
	unpinnedIdx := strings.Index(out, "unpinned content")
	if pinnedIdx < 0 || unpinnedIdx < 0 {
		t.Fatalf("FormatContext missing expected content:\n%s", out)
	}
	if pinnedIdx > unpinnedIdx {
		t.Fatalf("pinned content must appear before recent/unpinned content:\n%s", out)
	}
}

func TestBackend_FormatContext_ScopeFilter(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-1", "fact": "f1", "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaRaw: "global scope content", metaTitle: "G", metaType: "note", metaScope: "global"}},
		{"id": "fact-2", "fact": "f2", "created_at": "2026-07-20T01:00:00Z",
			"metadata": map[string]any{metaRaw: "project scope content", metaTitle: "P", metaType: "note", metaScope: "project"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	out, err := b.FormatContext("proj", "project")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if strings.Contains(out, "global scope content") {
		t.Fatalf("FormatContext scope filter leaked global-scope content:\n%s", out)
	}
	if !strings.Contains(out, "project scope content") {
		t.Fatalf("FormatContext scope filter dropped matching content:\n%s", out)
	}
}

// TestBackend_FormatContext_TruncatesContentTo300 is the I3 regression:
// internal/store's FormatContext truncates every observation's content to
// 300 runes before rendering it (store.go's truncate(obs.Content, 300)); the
// MemoryLake backend must match that so a project's rendered context block
// has the same shape regardless of which backend produced it.
func TestBackend_FormatContext_TruncatesContentTo300(t *testing.T) {
	long := strings.Repeat("a", 400)
	items := []map[string]any{
		{"id": "fact-1", "fact": "f1", "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaRaw: long, metaTitle: "T", metaType: "note", metaScope: "global"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	out, err := b.FormatContext("proj", "")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}
	if strings.Contains(out, long) {
		t.Fatalf("FormatContext did not truncate 400-rune content:\n%s", out)
	}
	want := strings.Repeat("a", 300) + "..."
	if !strings.Contains(out, want) {
		t.Fatalf("FormatContext missing expected 300-rune truncation with ellipsis:\n%s", out)
	}
}

// ─── Task-12 hardening (C1): TopicIndex hits are validated, not trusted ─────

// TestBackend_AddObservation_TopicKeyUpsert_ExpiredHitFallsThroughAndRecreates
// is the C1 critical-bug regression: a TopicIndex entry pointing at a fact
// that has since been forgotten (Expired == true) must NOT be PATCHed back
// into existence. Search/Timeline/Stats/FormatContext all exclude expired
// facts, so silently upserting into one would report success while the
// content stays permanently invisible. AddObservation must instead treat the
// hit as a miss, fall through to append+backfill, and re-point the index at
// the freshly created fact.
func TestBackend_AddObservation_TopicKeyUpsert_ExpiredHitFallsThroughAndRecreates(t *testing.T) {
	var appended, patchedExpired int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expiredPath := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-expired"
		switch {
		case r.Method == "GET" && r.URL.Path == expiredPath:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-expired", "fact": "forgotten content", "expired": true,
				"metadata": map[string]any{
					metaRaw: "forgotten content", metaScope: "global", metaTopicKey: "arch/db",
				},
			}})
		case r.Method == "PATCH" && r.URL.Path == expiredPath:
			atomic.AddInt32(&patchedExpired, 1)
			t.Fatal("must not PATCH an expired fact back into existence")
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.StoreInt32(&appended, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var items []map[string]any
			if atomic.LoadInt32(&appended) == 1 {
				items = []map[string]any{{"id": "fact-new", "fact": "new content", "metadata": map[string]any{}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-new":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-new", "fact": "new content", "metadata": map[string]any{metaObsID: "obs"},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if err := b.topics.Put("proj", "global", "arch/db", "fact-expired"); err != nil {
		t.Fatalf("seed TopicIndex: %v", err)
	}

	id, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "new content",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if patchedExpired != 0 {
		t.Fatal("expired fact was PATCHed (revived)")
	}
	if id != b.idmap.IntFor(b.projID, "fact-new") {
		t.Fatalf("id=%d, want the int64 mapped to fact-new (a fresh fact, not the expired one)", id)
	}
	// Self-heal: the index must now point at the new fact, not the expired one.
	factID, ok := b.topics.Lookup("proj", "global", "arch/db")
	if !ok || factID != "fact-new" {
		t.Fatalf("TopicIndex.Lookup after expired-hit fallthrough = (%q, %v), want (fact-new, true)", factID, ok)
	}
}

// TestBackend_AddObservation_TopicKeyUpsert_ScopeEmptyMatchesScopeProject is
// the I1 regression at the AddObservation level (not just topicIndexKey in
// isolation): a first save under scope="" and a second save under
// scope="project" for the same topic_key must be treated as the SAME fact —
// the second save must upsert (PATCH only), never append a second message.
func TestBackend_AddObservation_TopicKeyUpsert_ScopeEmptyMatchesScopeProject(t *testing.T) {
	var appendCount, patchCount int32
	factsExtracted := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		factPath := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1"
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&appendCount, 1)
			factsExtracted = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var items []map[string]any
			if factsExtracted {
				items = []map[string]any{{"id": "fact-1", "fact": "extracted", "metadata": map[string]any{}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "GET" && r.URL.Path == factPath:
			// Stamped with the RAW (unnormalized) scope the first save used
			// ("" — empty string, exactly what FactMetadata stores verbatim).
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "v1",
				"metadata": map[string]any{metaRaw: "v1", metaObsID: "obs", metaScope: "", metaTopicKey: "arch/db"},
			}})
		case r.Method == "PATCH":
			atomic.AddInt32(&patchCount, 1)
			var body struct {
				Metadata map[string]any `json:"metadata"`
				Fact     string         `json:"fact"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": body.Fact, "metadata": body.Metadata,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	id1, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "", TopicKey: "arch/db",
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "v1",
	})
	if err != nil {
		t.Fatalf("AddObservation (first, scope=\"\"): %v", err)
	}
	if appendCount != 1 {
		t.Fatalf("append count after first save=%d, want 1", appendCount)
	}

	id2, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "project", TopicKey: "arch/db",
		Type: "decision", Title: "t", Content: "v2",
	})
	if err != nil {
		t.Fatalf("AddObservation (second, scope=project): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("second save (scope=project) returned a different id than the first (scope=\"\"): %d vs %d, want equal (same normalized key)", id1, id2)
	}
	if appendCount != 1 {
		t.Fatalf("append count after second save=%d, want still 1 (scope=\"\" and scope=project must collide onto the same upsert)", appendCount)
	}
	if patchCount < 1 {
		t.Fatal("expected at least one PATCH for the second (upsert) save")
	}
}

// ─── Task-12 hardening (C2): Delete/Update maintain the TopicIndex ─────────

// TestBackend_DeleteObservation_PurgesTopicIndex_ThenSameTopicKeySaveAppendsNew
// is the C2 end-to-end regression: forgetting a topic_key fact must clear its
// TopicIndex entry so a later save with the same project+scope+topic_key
// cannot upsert-PATCH the now-forgotten fact back into visibility. It must
// instead go through the normal append+backfill path and produce a brand new
// fact.
func TestBackend_DeleteObservation_PurgesTopicIndex_ThenSameTopicKeySaveAppendsNew(t *testing.T) {
	var appendCount, patchOldCount int32
	var forgot, deleted atomic.Bool
	factsExtracted := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldFactPath := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-old"
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&appendCount, 1)
			factsExtracted = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var items []map[string]any
			if factsExtracted {
				items = []map[string]any{{"id": "fact-old", "fact": "v1", "metadata": map[string]any{}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && r.URL.Path == oldFactPath:
			// The very first PATCH here is BackfillFacts' legitimate initial
			// metadata stamp (part of establishing fact-old in the first
			// save, before delete). Any PATCH arriving AFTER the fact has
			// been forgotten would mean the deleted fact got revived via a
			// topic_key upsert hit — that must never happen (C1+C2).
			atomic.AddInt32(&patchOldCount, 1)
			if deleted.Load() {
				t.Fatal("must not PATCH fact-old after it has been forgotten (deleted)")
			}
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-old", "fact": "v1", "metadata": body.Metadata,
			}})
		case r.Method == "POST" && r.URL.Path == oldFactPath+"/forget":
			forgot.Store(true)
			deleted.Store(true)
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	// The mock never extracts a NEW fact for the second save's message (it
	// always reports fact-old, already-known and already-backfilled) — that
	// second save's backfill correctly times out (extraction "pending" is a
	// valid, non-error outcome; see AddObservation's doc comment). Shorten
	// maxWait so this test doesn't pay the default 500ms for that timeout.
	b.maxWait = 30 * time.Millisecond

	// First save: establish fact-old + its TopicIndex entry via the normal
	// append+backfill path.
	id1, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		SessionID: "sess-1", Type: "decision", Title: "t", Content: "v1",
	})
	if err != nil {
		t.Fatalf("AddObservation (first): %v", err)
	}
	if appendCount != 1 {
		t.Fatalf("append count after first save=%d, want 1", appendCount)
	}
	if factID, ok := b.topics.Lookup("proj", "global", "arch/db"); !ok || factID != "fact-old" {
		t.Fatalf("TopicIndex not seeded as expected: (%q, %v)", factID, ok)
	}

	// Forget it.
	if err := b.DeleteObservation(id1, false); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}
	if !forgot.Load() {
		t.Fatal("expected forget POST")
	}
	if factID, ok := b.topics.Lookup("proj", "global", "arch/db"); ok {
		t.Fatalf("TopicIndex entry still present after delete: %q (want purged)", factID)
	}

	// Second save with the identical project+scope+topic_key must NOT hit the
	// (now-cleared) index — it appends a fresh message/fact rather than
	// PATCHing fact-old.
	if _, err := b.AddObservation(store.AddObservationParams{
		Project: "proj", Scope: "global", TopicKey: "arch/db",
		SessionID: "sess-2", Type: "decision", Title: "t2", Content: "v2 after delete",
	}); err != nil {
		t.Fatalf("AddObservation (after delete): %v", err)
	}
	if patchOldCount != 1 {
		t.Fatalf("fact-old was PATCHed %d times, want exactly 1 (the initial backfill stamp only — no revival PATCH after delete)", patchOldCount)
	}
	if appendCount != 2 {
		t.Fatalf("append count after post-delete save=%d, want 2 (must append, not upsert)", appendCount)
	}
}

// TestBackend_UpdateObservation_ScopeChange_PurgesTopicIndex is the C2
// regression for UpdateObservation: changing the scope (or topic_key) that a
// fact was originally upserted under must purge the TopicIndex entry pointing
// at it, so the OLD project+scope+topic_key can never resolve to this
// now-reassigned fact again.
func TestBackend_UpdateObservation_ScopeChange_PurgesTopicIndex(t *testing.T) {
	factPath := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == factPath:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "old",
				"metadata": map[string]any{metaRaw: "old", metaScope: "global", metaTopicKey: "arch/db"},
			}})
		case r.Method == "PATCH" && r.URL.Path == factPath:
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "old", "metadata": body.Metadata,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id := b.idmap.IntFor(b.projID, "fact-1")
	if err := b.topics.Put("proj", "global", "arch/db", "fact-1"); err != nil {
		t.Fatalf("seed TopicIndex: %v", err)
	}

	newScope := "personal"
	if _, err := b.UpdateObservation(id, store.UpdateObservationParams{Scope: &newScope}); err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}

	if factID, ok := b.topics.Lookup("proj", "global", "arch/db"); ok {
		t.Fatalf("TopicIndex entry for the old scope still present after UpdateObservation changed it: %q", factID)
	}
}

// ─── Task-12 hardening (I2): listAllFacts pagination is bounded ────────────

// TestListAllFacts_TerminatesAgainstInfiniteContinuationToken is exercised
// via Stats (a real caller of listAllFacts) against a server that always
// returns a non-empty continuation_token, simulating a misbehaving/malicious
// server. Without a cap the pagination loop in listAllFacts would never
// terminate; with the cap, the call must return (bounded results, nil error)
// rather than hang.
func TestListAllFacts_TerminatesAgainstInfiniteContinuationToken(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts"):
			atomic.AddInt32(&gets, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items":              []map[string]any{{"id": "x", "fact": "y"}},
				"continuation_token": "always-more", // never empty: server never stops
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	done := make(chan struct{})
	var stats *store.Stats
	var err error
	go func() {
		stats, err = b.Stats()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stats (via listAllFacts) did not terminate against an infinite continuation_token")
	}
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got := atomic.LoadInt32(&gets); got != int32(maxListAllFactsPages) {
		t.Fatalf("facts-list GET count=%d, want exactly maxListAllFactsPages=%d", got, maxListAllFactsPages)
	}
	if stats.TotalObservations != maxListAllFactsPages {
		t.Fatalf("TotalObservations=%d, want %d (one fact per page, capped)", stats.TotalObservations, maxListAllFactsPages)
	}
}

// TestBackend_FactForID_RejectsOtherProject is the backend-layer regression for
// the Task-15 Critical: two backends bound to DIFFERENT projects but sharing the
// one process-global IDMap must never resolve each other's ids. An id minted for
// project A reads as not-found on B's backend (and vice versa), so a by-id call
// (GetObservation/Update/Delete/Pin/Timeline/MarkReviewed) can never cross the
// project boundary — the guard in factForID.
func TestBackend_FactForID_RejectsOtherProject(t *testing.T) {
	idmap, err := LoadIDMap(t.TempDir() + "/idmap.json")
	if err != nil {
		t.Fatalf("LoadIDMap: %v", err)
	}
	a := &MemoryLakeBackend{projID: "proj-1", idmap: idmap}
	b := &MemoryLakeBackend{projID: "proj-2", idmap: idmap}

	idA := idmap.IntFor("proj-1", "fact-a")
	idB := idmap.IntFor("proj-2", "fact-b")
	if idA == idB {
		t.Fatalf("distinct projects must get distinct global ids, both got %d", idA)
	}

	if f, ok := a.factForID(idA); !ok || f != "fact-a" {
		t.Fatalf("a.factForID(idA)=%q,%v want fact-a,true", f, ok)
	}
	if _, ok := b.factForID(idA); ok {
		t.Fatal("b (proj-2) must not resolve proj-1's id")
	}
	if f, ok := b.factForID(idB); !ok || f != "fact-b" {
		t.Fatalf("b.factForID(idB)=%q,%v want fact-b,true", f, ok)
	}
	if _, ok := a.factForID(idB); ok {
		t.Fatal("a (proj-1) must not resolve proj-2's id")
	}
	// GetObservation surfaces the guard as a NOT_FOUND rather than another
	// project's content — and crucially without any HTTP call.
	if _, err := b.GetObservation(idA); err == nil {
		t.Fatal("GetObservation of another project's id must return NOT_FOUND, not leak content")
	}
}
