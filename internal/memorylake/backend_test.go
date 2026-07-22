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
	return &MemoryLakeBackend{
		client:  NewClient(cfg),
		cfg:     cfg,
		ws:      "ws-1",
		projID:  "proj-1",
		actorID: "actor-1",
		idmap:   idmap,
		poll:    1 * time.Millisecond,
		maxWait: 500 * time.Millisecond,
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
	// The returned int must reverse-map back to fact-1.
	if f, ok := b.idmap.FactFor(1); !ok || f != "fact-1" {
		t.Fatalf("FactFor(1)=%q,%v; want fact-1,true", f, ok)
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
	if f, ok := b.idmap.FactFor(id); !ok || f != "msg-9" {
		t.Fatalf("FactFor(%d)=%q,%v; want provisional msg-9,true", id, f, ok)
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
	id := b.idmap.IntFor("fact-7") // seed mapping (id=1)

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
	id := b.idmap.IntFor("fact-2")

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
		id := b.idmap.IntFor("fact-3")
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
	id := b.idmap.IntFor("fact-4")
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
	b, err := NewBackend(cfg, "ws-1", "proj-42")
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
