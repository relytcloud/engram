package memorylake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	AddObservation(p store.AddObservationParams) (string, error)
	GetObservation(syncID string) (*store.Observation, error)
	UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error)
	DeleteObservation(syncID string, hardDelete bool) error
	Search(query string, opts store.SearchOptions) ([]store.SearchResult, error)
	Timeline(syncID string, before, after int) (*store.TimelineResult, error)
	FormatContext(project, scope string) (string, error)
	Stats() (*store.Stats, error)
	MaxObservationLength() int

	PinObservation(syncID string) error
	UnpinObservation(syncID string) error
	ObservationsNeedingReview(project string, limit int) ([]store.Observation, error)
	MarkReviewed(syncID string) error

	CreateSession(id, project, directory string) error
	GetSession(id string) (*store.Session, error)
	EndSession(id string, summary string) error
	MostRecentActiveSession(project string) (string, bool, error)
	RecentSessions(project string, limit int) ([]store.SessionSummary, error)

	AddPrompt(p store.AddPromptParams) (string, error)
	AddPromptIfMissing(p store.AddPromptParams) (string, bool, error)
	PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error)

	ProjectExists(name string) (bool, error)
	ListProjectNames() ([]string, error)
	CountObservationsForProject(name string) (int, error)
	MergeProjects(sources []string, canonical string) (*store.MergeResult, error)

	FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error)
	GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error)
	JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error)
	JudgeBySemantic(p store.JudgeBySemanticParams) (string, error)
}

// Compile-time self-check: *MemoryLakeBackend must satisfy the same method set
// as internal/mcp.MemoryBackend (mirrored above).
var _ memoryBackend = (*MemoryLakeBackend)(nil)

// newTestBackend builds a MemoryLakeBackend wired to srvURL, with a fresh
// SessionIndex sidecar file so session state never bleeds across tests.
func newTestBackend(t *testing.T, srvURL string) *MemoryLakeBackend {
	t.Helper()
	cfg := Config{BaseURL: srvURL, APIKey: "sk-test", TimeoutMS: 5000}
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
		sessions: sessions,
	}
}

// TestBackend_AddObservation_WritesVerbatimFactDirectly is the direct-write
// path: AddObservation posts the observation verbatim to the direct fact-add
// endpoint (no conversations/messages, no async extraction), preserves the
// title by prepending it, and returns the real MemoryLake fact id.
func TestBackend_AddObservation_WritesVerbatimFactDirectly(t *testing.T) {
	var factPosts int32
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			atomic.AddInt32(&factPosts, 1)
			var body struct {
				Facts []string `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Facts) > 0 {
				gotText = body.Facts[0]
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{{"id": "fact-1", "fact": gotText}},
			}})
		default:
			t.Fatalf("unexpected request %s %s (AddObservation must use the direct fact-add endpoint, not conversations/messages)", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id, err := b.AddObservation(store.AddObservationParams{
		SessionID: "sess-1", Type: "decision", Title: "Use Postgres", Content: "Postgres 15 for users.", Scope: "global",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id != "fact-1" {
		t.Fatalf("id=%q, want the real fact id fact-1", id)
	}
	if atomic.LoadInt32(&factPosts) != 1 {
		t.Fatalf("factPosts=%d, want 1", factPosts)
	}
	if gotText != "Use Postgres\n\nPostgres 15 for users." {
		t.Fatalf("stored text=%q, want title prepended to content verbatim", gotText)
	}
}

// TestBackend_AddObservation_EmptyFactIDFallsBackToContentHash covers the
// defensive fallback when the direct fact-add response carries no fact id.
func TestBackend_AddObservation_EmptyFactIDFallsBackToContentHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"facts": []map[string]any{}}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	id, err := b.AddObservation(store.AddObservationParams{Content: "some content"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id != contentHash("some content") {
		t.Fatalf("id=%q, want content-hash fallback %q", id, contentHash("some content"))
	}
}

// TestBackend_AddObservation_EmptyContentErrors verifies an observation with no
// content (nothing to store as a fact) is rejected rather than posting a blank
// fact the server would reject anyway.
func TestBackend_AddObservation_EmptyContentErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no request expected for empty content, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if _, err := b.AddObservation(store.AddObservationParams{Title: "   ", Content: ""}); err == nil {
		t.Fatal("expected an error for empty content, got nil")
	}
}

// TestBackend_AddObservation_ConcurrentSavesAllSucceed exercises the
// concurrent-save path under `-race`: distinct content must yield distinct fact
// ids with no cross-save interference.
func TestBackend_AddObservation_ConcurrentSavesAllSucceed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts" {
			var body struct {
				Facts []string `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			text := ""
			if len(body.Facts) > 0 {
				text = body.Facts[0]
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{{"id": "fact-" + contentHash(text), "fact": text}},
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	contents := []string{
		"observation ALPHA, verbatim and distinct",
		"observation BRAVO, verbatim and distinct",
		"observation CHARLIE, verbatim and distinct",
		"observation DELTA, verbatim and distinct",
	}

	type result struct {
		id  string
		err error
	}
	results := make(chan result, len(contents))
	for _, c := range contents {
		go func(c string) {
			id, err := b.AddObservation(store.AddObservationParams{
				SessionID: "sess-1", Type: "note", Title: "t", Content: c, Scope: "global",
			})
			results <- result{id, err}
		}(c)
	}

	seen := map[string]bool{}
	for range contents {
		r := <-results
		if r.err != nil {
			t.Fatalf("AddObservation: %v", r.err)
		}
		if seen[r.id] {
			t.Fatalf("duplicate id %q returned across concurrent saves of distinct content", r.id)
		}
		seen[r.id] = true
	}
}

// TestBackend_GetObservation_FetchesFactDirectlyByID verifies sync_id → fact
// id → fact → Observation, with no id-mapping indirection: the syncID passed
// in *is* the MemoryLake fact id.
func TestBackend_GetObservation_FetchesFactDirectlyByID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-7" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id":         "fact-7",
				"fact":       "mem0's own text",
				"metadata":   map[string]any{metaTitle: "title", metaType: "note", metaScope: "global"},
				"created_at": "2026-07-22T00:00:00Z",
				"updated_at": "2026-07-22T01:00:00Z",
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	obs, err := b.GetObservation("fact-7")
	if err != nil {
		t.Fatalf("GetObservation: %v", err)
	}
	if obs.SyncID != "fact-7" {
		t.Fatalf("obs.SyncID=%q, want fact-7", obs.SyncID)
	}
	if obs.Content != "mem0's own text" {
		t.Fatalf("Content=%q, want mem0's own text (no more engram_raw preference)", obs.Content)
	}
	if obs.CreatedAt != "2026-07-22T00:00:00Z" || obs.UpdatedAt != "2026-07-22T01:00:00Z" {
		t.Fatalf("timestamps not carried through: %q / %q", obs.CreatedAt, obs.UpdatedAt)
	}
}

// TestBackend_GetObservation_UnknownID errors when the fact doesn't exist.
func TestBackend_GetObservation_UnknownID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"success": false, "error_code": "NOT_FOUND", "message": "no such fact"})
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)
	if _, err := b.GetObservation("fact-does-not-exist"); err == nil {
		t.Fatal("expected error for unknown fact id")
	}
}

// TestBackend_UpdateObservation merges metadata and sends the new text via
// the V3 `fact` field on PATCH (not `content`), addressed directly by fact id.
func TestBackend_UpdateObservation(t *testing.T) {
	var patchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-2"
		switch {
		case r.Method == "GET" && r.URL.Path == p:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-2", "fact": "old",
				"metadata": map[string]any{metaTitle: "old title", metaType: "note"},
			}})
		case r.Method == "PATCH" && r.URL.Path == p:
			json.NewDecoder(r.Body).Decode(&patchBody)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-2", "fact": "new content",
				"metadata": map[string]any{metaTitle: "new title", metaType: "note"},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	newTitle := "new title"
	newContent := "new content"
	obs, err := b.UpdateObservation("fact-2", store.UpdateObservationParams{Title: &newTitle, Content: &newContent})
	if err != nil {
		t.Fatalf("UpdateObservation: %v", err)
	}
	if obs.Content != "new content" {
		t.Fatalf("Content=%q, want new content", obs.Content)
	}
	if obs.SyncID != "fact-2" {
		t.Fatalf("obs.SyncID=%q, want fact-2", obs.SyncID)
	}
	if patchBody["fact"] != "new content" {
		t.Fatalf("PATCH fact=%v, want new content", patchBody["fact"])
	}
	if _, hasContent := patchBody["content"]; hasContent {
		t.Fatalf("PATCH body must not send a `content` field (V3 uses `fact`): %v", patchBody)
	}
	md, _ := patchBody["metadata"].(map[string]any)
	if md[metaTitle] != "new title" {
		t.Fatalf("PATCH metadata not merged correctly: %v", md)
	}
	if md[metaType] != "note" {
		t.Fatalf("PATCH metadata dropped preserved key engram_type: %v", md)
	}
	if _, hasRaw := md["engram_raw"]; hasRaw {
		t.Fatalf("PATCH metadata must not write engram_raw any more: %v", md)
	}
}

// TestBackend_DeleteObservation_CallsForget verifies soft-delete maps to the
// V3 forget endpoint (both for soft and hard delete requests), addressed
// directly by fact id.
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
		if err := b.DeleteObservation("fact-3", hard); err != nil {
			t.Fatalf("DeleteObservation(hard=%v): %v", hard, err)
		}
		if !forgot {
			t.Fatalf("hard=%v: expected forget POST", hard)
		}
		srv.Close()
	}
}

// TestBackend_Search_PassesThrough verifies Search delegates to SearchFacts
// and that the resulting Observation carries the fact id as SyncID.
func TestBackend_Search_PassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search" {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{
					{"id": "fact-1", "fact": "content", "score": 0.9, "metadata": map[string]any{}},
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
	if res[0].SyncID != "fact-1" {
		t.Fatalf("Search result SyncID=%q, want fact-1", res[0].SyncID)
	}
}

// TestBackend_PinObservation_PatchesMetadataPinned verifies pin flips the
// pinned metadata flag while preserving existing metadata, addressed
// directly by fact id.
func TestBackend_PinObservation_PatchesMetadataPinned(t *testing.T) {
	var md map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-4"
		switch {
		case r.Method == "GET" && r.URL.Path == p:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-4", "fact": "f", "metadata": map[string]any{metaTitle: "keep"},
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
	if err := b.PinObservation("fact-4"); err != nil {
		t.Fatalf("PinObservation: %v", err)
	}
	if md["pinned"] != true {
		t.Fatalf("metadata.pinned=%v, want true", md["pinned"])
	}
	if md[metaTitle] != "keep" {
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

// TestNewBackend exercises the constructor's resolve → actor flow.
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
		Actor: "cli-machine",
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
}

// ─── ProjectExists / ListProjectNames ────────────────────────────────────────

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

// ─── Stats / CountObservationsForProject (paginated, expired-excluding) ──────

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

// ─── Timeline ────────────────────────────────────────────────────────────────

func TestBackend_Timeline_OrdersByCreatedAtAndExcludesExpiredNeighbors(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-a", "fact": "A", "created_at": "2026-07-20T00:00:00Z"},
		{"id": "fact-b", "fact": "B", "expired": true, "created_at": "2026-07-20T00:30:00Z"},
		{"id": "fact-c", "fact": "C", "created_at": "2026-07-20T01:00:00Z"},
		{"id": "fact-d", "fact": "D", "created_at": "2026-07-20T02:00:00Z"},
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

	res, err := b.Timeline("fact-c", 5, 5)
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

func TestBackend_Timeline_UnknownAnchor_Errors(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-a", "fact": "A", "created_at": "2026-07-20T00:00:00Z"},
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

	if _, err := b.Timeline("fact-nonexistent", 5, 5); err == nil {
		t.Fatal("expected error for an anchor id not present in the project fact list")
	}
}

// ─── FormatContext ────────────────────────────────────────────────────────────

func TestBackend_FormatContext_PinnedFirstThenRecentExcludingExpired(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-1", "fact": "unpinned content", "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaTitle: "U", metaType: "note", metaScope: "global"}},
		{"id": "fact-2", "fact": "pinned content", "created_at": "2026-07-20T01:00:00Z",
			"metadata": map[string]any{metaTitle: "P", metaType: "decision", metaScope: "global", "pinned": true}},
		{"id": "fact-3", "fact": "expired content", "expired": true, "created_at": "2026-07-20T02:00:00Z",
			"metadata": map[string]any{metaTitle: "E", metaType: "note", metaScope: "global"}},
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
		{"id": "fact-1", "fact": "global scope content", "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaTitle: "G", metaType: "note", metaScope: "global"}},
		{"id": "fact-2", "fact": "project scope content", "created_at": "2026-07-20T01:00:00Z",
			"metadata": map[string]any{metaTitle: "P", metaType: "note", metaScope: "project"}},
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

// TestBackend_FormatContext_TruncatesPinnedContentTo300 is the I3 regression:
// internal/store's FormatContext truncates a pinned observation's content to
// 300 runes before rendering it (store.go's truncate(obs.Content,
// contextPinnedTruncLen)); the MemoryLake backend must match that so a
// project's rendered context block has the same shape regardless of which
// backend produced it.
//
// Since the layered rewrite (Phase 2 R3b) this 300-rune cut applies to the
// pinned block only — recent-observation lines are cut to their first sentence
// (contextSummaryTruncLen) on both backends, which
// TestBackend_FormatContext_LayeredBudget covers. The fact below is therefore
// pinned; the unpinned tail of this test asserts the 160-rune summary cap.
func TestBackend_FormatContext_TruncatesPinnedContentTo300(t *testing.T) {
	long := strings.Repeat("a", 400)
	items := []map[string]any{
		{"id": "fact-1", "fact": long, "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaTitle: "T", metaType: "note", metaScope: "global", "pinned": true}},
		{"id": "fact-2", "fact": strings.Repeat("b", 400), "created_at": "2026-07-20T01:00:00Z",
			"metadata": map[string]any{metaTitle: "U", metaType: "note", metaScope: "global"}},
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
	wantSummary := strings.Repeat("b", contextSummaryTruncLen) + "…"
	if !strings.Contains(out, wantSummary) {
		t.Fatalf("FormatContext missing expected %d-rune summary cap on the recent line:\n%s",
			contextSummaryTruncLen, out)
	}
}

// ─── listAllFacts pagination is bounded (I2) ────────────────────────────────

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

// ─── Layered mem_context budget (Phase 2 R3b) ────────────────────────────────

// TestBackend_FormatContext_LayeredBudget mirrors internal/store's
// TestFormatContextLayeredBudget: the MemoryLake rendering must produce the
// same layered block (single "## Memory Context" heading, at most
// contextRecentObs observation summaries cut to their first sentence, the
// expand-on-demand footer, whole block under contextByteCap) so a project's
// context looks the same on either backend. This backend has no sessions, so
// the "### Recent Sessions" section is simply absent.
func TestBackend_FormatContext_LayeredBudget(t *testing.T) {
	items := []map[string]any{
		{"id": "fact-pin", "fact": "PINNED-MARKER pinned body. second pinned sentence.",
			"created_at": "2026-07-20T00:00:00Z",
			"metadata":   map[string]any{metaTitle: "pinned-title", metaType: "decision", metaScope: "global", "pinned": true}},
	}
	for i := 0; i < 10; i++ {
		items = append(items, map[string]any{
			"id":         fmt.Sprintf("fact-%d", i),
			"fact":       fmt.Sprintf("title-%d first sentence. tail sentence that must not appear.", i),
			"created_at": fmt.Sprintf("2026-07-21T%02d:00:00Z", i),
			"metadata":   map[string]any{metaTitle: fmt.Sprintf("title-%d", i), metaType: "note", metaScope: "global"},
		})
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
	if len(out) > contextByteCap {
		t.Fatalf("context %d bytes, cap %d:\n%s", len(out), contextByteCap, out)
	}
	if !strings.HasPrefix(out, "## Memory Context\n") {
		t.Fatalf("missing layered heading:\n%s", out)
	}
	if !strings.Contains(out, "PINNED-MARKER") {
		t.Fatalf("pinned content must never be dropped:\n%s", out)
	}
	if !strings.Contains(out, "Full bodies: mem_search / mem_get_observation.") {
		t.Fatalf("missing expand-on-demand footer:\n%s", out)
	}
	if got := strings.Count(out, "title-"); got != contextRecentObs*2 {
		// Each rendered observation prints its title once and its first
		// sentence (which starts with the same "title-N" token) once.
		t.Fatalf("rendered %d title- tokens, want %d (%d observations):\n%s",
			got, contextRecentObs*2, contextRecentObs, out)
	}
	if strings.Contains(out, "tail sentence that must not appear") {
		t.Fatalf("observation body not cut to first sentence:\n%s", out)
	}
	if strings.Contains(out, "title-0 first sentence") {
		t.Fatalf("oldest observation must be dropped:\n%s", out)
	}
	if !strings.Contains(out, "title-9") {
		t.Fatalf("newest observation must be rendered:\n%s", out)
	}
	assertBackendContextSectionOrder(t, out)
}

// TestBackend_FormatContext_ByteCapDropsOldestObservationNeverPinned is the
// MemoryLake twin of internal/store's
// TestFormatContextByteCapDropsObservationsThenSessionsNeverPinned. The fixture
// below (three 300-rune pinned facts plus long recent facts) renders past
// contextByteCap BEFORE any dropping — see the pre-cap guard — so the cap loop
// in renderLayeredContext genuinely runs. Survivors are asserted by name: the
// oldest of the contextRecentObs rendered facts is dropped first, the newest
// survive, and every pinned line survives. This backend has no session lines,
// so the store's "observations before sessions" stage has no analogue here.
func TestBackend_FormatContext_ByteCapDropsOldestObservationNeverPinned(t *testing.T) {
	pinnedBody := "PINNED-MARKER " + strings.Repeat("p", 400)
	var items []map[string]any
	for i := 0; i < 3; i++ {
		items = append(items, map[string]any{
			"id":         fmt.Sprintf("fact-pin-%d", i),
			"fact":       pinnedBody,
			"created_at": fmt.Sprintf("2026-07-20T%02d:00:00Z", i),
			"metadata": map[string]any{
				metaTitle: fmt.Sprintf("pinned-%d", i), metaType: "decision",
				metaScope: "global", "pinned": true,
			},
		})
	}
	recentBody := func(i int) string {
		return fmt.Sprintf("title-%d %s.", i, strings.Repeat("filler words ", 30))
	}
	for i := 0; i < 10; i++ {
		items = append(items, map[string]any{
			"id":         fmt.Sprintf("fact-%d", i),
			"fact":       recentBody(i),
			"created_at": fmt.Sprintf("2026-07-21T%02d:00:00Z", i),
			"metadata":   map[string]any{metaTitle: fmt.Sprintf("title-%d", i), metaType: "note", metaScope: "global"},
		})
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

	// The fixture must overflow the cap BEFORE dropping, otherwise the drop
	// assertions below would be vacuously satisfied by a loop that never runs.
	var pinnedLines []string
	for i := 0; i < 3; i++ {
		pinnedLines = append(pinnedLines, fmt.Sprintf("- [decision] **pinned-%d**: %s\n",
			i, truncate(pinnedBody, contextPinnedTruncLen)))
	}
	var obsLines []string
	for i := 9; i > 9-contextRecentObs; i-- {
		obsLines = append(obsLines, fmt.Sprintf("- [note] **title-%d**: %s\n", i, firstSentence(recentBody(i))))
	}
	if precap := len(joinLayeredContext(pinnedLines, obsLines)); precap <= contextByteCap {
		t.Fatalf("fixture renders %d bytes before dropping, already under the %d-byte cap: "+
			"the cap loop is not exercised, enlarge the fixture", precap, contextByteCap)
	}
	// Sanity in the other direction: the fixture must NOT trip the separate
	// pinned-section cap. This test asserts all three pinned lines survive; a
	// pin-cap trip would drop the oldest behind a marker and fail below with a
	// misleading "found 2" instead of naming the real cause. See the arithmetic
	// note under the survival assertion — the fixture sits 5 bytes clear.
	if pinBytes := pinnedSectionBytes(pinnedLines, ""); pinBytes > pinnedSectionByteCap {
		t.Fatalf("fixture pinned section is %d bytes, over the %d-byte pin cap: the pin cap now "+
			"drops entries this test expects to survive, shrink the pinned fixture",
			pinBytes, pinnedSectionByteCap)
	}

	if len(out) > contextByteCap {
		t.Fatalf("context %d bytes, cap %d:\n%s", len(out), contextByteCap, out)
	}
	// Pinned lines are never dropped by the OUTER cap: all three survive, each
	// truncated to contextPinnedTruncLen runes. Note the fixture also sits just
	// under the separate pinned-section cap — heading (25) + 3 × 331-byte lines
	// + blank separator = 1019 of pinnedSectionByteCap's 1024 bytes — so a
	// fourth pinned fact (or a longer title here) would legitimately trip the
	// pin cap and make this assertion fail; TestBackend_FormatContext_
	// PinnedSectionCapAndOrder is where that behavior belongs.
	if got := strings.Count(out, "PINNED-MARKER"); got != 3 {
		t.Fatalf("all 3 pinned lines must survive the byte cap, found %d:\n%s", got, out)
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(out, fmt.Sprintf("**pinned-%d**", i)) {
			t.Fatalf("pinned line %d must survive the byte cap:\n%s", i, out)
		}
	}
	if !strings.Contains(out, "Full bodies: mem_search / mem_get_observation.") {
		t.Fatalf("footer must survive the byte cap:\n%s", out)
	}
	// Drop is oldest-first: the two newest recent facts survive...
	for _, want := range []string{"**title-9**", "**title-8**"} {
		if !strings.Contains(out, want) {
			t.Fatalf("newest observation %s must survive the byte cap (drop is oldest-first):\n%s", want, out)
		}
	}
	// ...and the oldest of the contextRecentObs rendered facts is dropped.
	if strings.Contains(out, "**title-7**") {
		t.Fatalf("oldest rendered observation title-7 must be dropped by the byte cap:\n%s", out)
	}
	if got := strings.Count(out, "**title-"); got != contextRecentObs-1 {
		t.Fatalf("rendered %d observation lines, want %d after the cap drop:\n%s",
			got, contextRecentObs-1, out)
	}
	assertBackendContextSectionOrder(t, out)
}

// backendPinnedSectionBody returns the rendered pinned section of a context
// block — heading, entry lines, overflow marker when present, and the blank
// separator writeContextSection appends. Twin of internal/store's
// pinnedSectionBody: sections are separated by exactly one blank line and no
// entry line is empty, so the first "\n\n" after the heading ends the section.
//
// Same caveat as the twin: an entry whose content contains a BLANK line ends the
// section early here and would be under-measured. Production accounting sums the
// entry strings (pinnedSectionBytes) and is unaffected — keep fixture bodies free
// of blank lines.
func backendPinnedSectionBody(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, pinnedSectionHeading)
	if start < 0 {
		t.Fatalf("pinned heading %q not found in context block:\n%s", pinnedSectionHeading, out)
	}
	rest := out[start:]
	end := strings.Index(rest[len(pinnedSectionHeading):], "\n\n")
	if end < 0 {
		t.Fatalf("pinned section is not terminated by a blank line:\n%s", out)
	}
	return rest[:len(pinnedSectionHeading)+end+2]
}

// TestBackend_FormatContext_PinnedSectionCapAndOrder is the MemoryLake twin of
// internal/store's TestFormatContextPinnedSectionCapAndOrder: the pinned section
// is capped at pinnedSectionByteCap on its own, entries render in pin-time
// ASCENDING order, and overflow drops the OLDEST pins behind a marker naming how
// many were withheld.
//
// The fixture pins 15 facts whose rendered lines are exactly 120 bytes each
// (1800 bytes, well past the cap) with updated_at ASCENDING by pin index and
// created_at DESCENDING — MemoryLake records no pin timestamp (setPinned only
// PATCHes metadata["pinned"]), so UpdatedAt is the documented pin-time proxy and
// the inversion makes an implementation that ordered by created_at fail here.
//
// Regression net for the same four mutations as the store twin: (a) deleting the
// cap loop, (b) reversing to newest-first, (c) ordering by created_at, (d)
// dropping more pins than the cap requires.
func TestBackend_FormatContext_PinnedSectionCapAndOrder(t *testing.T) {
	const (
		totalPins = 15
		// "- [decision] **pin-NN**: " (25) + body (94) + "\n" = 120 bytes.
		lineBytes = 120
	)
	body := func(i int) string {
		return fmt.Sprintf("core fact %02d ", i) + strings.Repeat("y", 81)
	}
	var items []map[string]any
	for i := 0; i < totalPins; i++ {
		items = append(items, map[string]any{
			"id":   fmt.Sprintf("fact-pin-%02d", i),
			"fact": body(i),
			// created_at descends with i, updated_at (the pin-time proxy) ascends.
			"created_at": fmt.Sprintf("2026-02-%02dT00:00:00Z", 20-i),
			"updated_at": fmt.Sprintf("2026-03-01T%02d:00:00Z", i),
			"metadata": map[string]any{
				metaTitle: fmt.Sprintf("pin-%02d", i), metaType: "decision",
				metaScope: "global", "pinned": true,
			},
		})
	}
	// One unpinned fact so the block still has a Recent Observations section.
	items = append(items, map[string]any{
		"id": "fact-recent", "fact": "recent unpinned body.",
		"created_at": "2026-03-02T00:00:00Z",
		"metadata":   map[string]any{metaTitle: "recent", metaType: "note", metaScope: "global"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	// Sanity: the fixture must render past the pinned cap before any dropping.
	if precap := len(pinnedSectionHeading) + totalPins*lineBytes + 1; precap <= pinnedSectionByteCap {
		t.Fatalf("fixture renders %d pinned bytes, already under the %d-byte cap: enlarge it",
			precap, pinnedSectionByteCap)
	}

	out, err := b.FormatContext("proj", "global")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}

	section := backendPinnedSectionBody(t, out)
	if len(section) > pinnedSectionByteCap {
		t.Fatalf("pinned section is %d bytes, cap %d:\n%s", len(section), pinnedSectionByteCap, section)
	}

	shown := strings.Count(section, "**pin-")
	if shown == 0 {
		t.Fatalf("pinned section dropped every entry; core memory must keep the newest:\n%s", section)
	}
	if shown == totalPins {
		t.Fatalf("all %d pinned entries rendered: the cap was not enforced:\n%s", totalPins, section)
	}

	marker := fmt.Sprintf(pinnedCapMarkerFmt, totalPins-shown)
	if !strings.Contains(section, marker) {
		t.Fatalf("expected overflow marker %q in:\n%s", marker, section)
	}

	for i := 0; i < totalPins-shown; i++ {
		if strings.Contains(section, fmt.Sprintf("**pin-%02d**", i)) {
			t.Fatalf("oldest pin pin-%02d must be dropped from rendering:\n%s", i, section)
		}
	}
	for i := totalPins - shown; i < totalPins; i++ {
		if !strings.Contains(section, fmt.Sprintf("**pin-%02d**", i)) {
			t.Fatalf("newest pin pin-%02d must survive the pinned cap:\n%s", i, section)
		}
	}

	prev := -1
	for i := totalPins - shown; i < totalPins; i++ {
		at := strings.Index(section, fmt.Sprintf("**pin-%02d**", i))
		if at <= prev {
			t.Fatalf("pinned entries must render in pin-time ascending order; pin-%02d at %d follows %d:\n%s",
				i, at, prev, section)
		}
		prev = at
	}

	if len(section)+lineBytes <= pinnedSectionByteCap {
		t.Fatalf("pinned section is %d bytes with %d entries: one more would still fit under %d, "+
			"the cap drops more than necessary:\n%s", len(section), shown, pinnedSectionByteCap, section)
	}

	if len(out) > contextByteCap {
		t.Fatalf("context %d bytes, cap %d:\n%s", len(out), contextByteCap, out)
	}
	if !strings.Contains(out, "Full bodies: mem_search / mem_get_observation.") {
		t.Fatalf("footer must survive:\n%s", out)
	}
}

// singlePinnedFactBackend serves exactly one pinned fact (title/body given) from
// the facts endpoint, so the pinned-section degenerate paths below can be driven
// without repeating the httptest wiring.
func singlePinnedFactBackend(t *testing.T, title, body string) *MemoryLakeBackend {
	t.Helper()
	items := []map[string]any{{
		"id": "fact-solo-pin", "fact": body, "created_at": "2026-07-20T00:00:00Z",
		"metadata": map[string]any{
			metaTitle: title, metaType: "decision", metaScope: "global", "pinned": true,
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return newTestBackend(t, srv.URL)
}

// TestBackend_FormatContext_PinnedLoneOverCapEntryRendersWithoutMarker is the
// MemoryLake twin of internal/store's
// TestFormatContextPinnedLoneOverCapEntryRendersWithoutMarker: a SINGLE pinned
// entry that alone exceeds pinnedSectionByteCap still renders (core memory is
// never emptied) and emits NO overflow marker — nothing was withheld, so
// "(pin cap reached: 0 pinned facts not shown …)" would be false.
func TestBackend_FormatContext_PinnedLoneOverCapEntryRendersWithoutMarker(t *testing.T) {
	// contextPinnedTruncLen emoji runes: exactly at the rune cut (body renders
	// whole, no "...") at 4 bytes each, so the line is ~1220 bytes — past the
	// 998 bytes an entry may occupy once the heading (25) and blank separator
	// (1) are charged against the 1024-byte cap.
	body := strings.Repeat("🔥", contextPinnedTruncLen)
	b := singlePinnedFactBackend(t, "solo-pin", body)

	out, err := b.FormatContext("proj", "global")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}

	// Sanity: the lone entry must genuinely be over the cap, otherwise this
	// test passes on the ordinary "everything fits" path and proves nothing.
	section := backendPinnedSectionBody(t, out)
	if len(section) <= pinnedSectionByteCap {
		t.Fatalf("fixture pinned section is %d bytes, within the %d-byte cap: the lone-entry "+
			"over-cap path is not exercised, enlarge the fixture:\n%s",
			len(section), pinnedSectionByteCap, section)
	}

	if !strings.Contains(out, "**solo-pin**") {
		t.Fatalf("the lone pinned entry must render even over cap:\n%s", out)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("the lone pinned entry's body must render whole:\n%s", out)
	}
	if got := fmt.Sprintf(pinnedCapMarkerFmt, 0); strings.Contains(out, got) {
		t.Fatalf("emitted a zero-count pin-cap marker %q — nothing was withheld:\n%s", got, out)
	}
	if strings.Contains(out, "pin cap reached") {
		t.Fatalf("no pin-cap marker may appear when nothing is withheld:\n%s", out)
	}
	if strings.Contains(out, "not shown") {
		t.Fatalf("no pin-cap marker may appear when nothing is withheld:\n%s", out)
	}
}

// TestBackend_FormatContext_TruncatesTitleKeepingPinnedBlockUnderOuterCap is the
// MemoryLake twin of internal/store's
// TestFormatContextTruncatesTitleKeepingPinnedBlockUnderOuterCap: the title cut
// (contextTitleTruncLen) is cap enforcement, not cosmetics — a pinned line is the
// one thing NEITHER cap can drop, so an unbounded title was the remaining way to
// push the block past contextByteCap with nothing left to trim.
func TestBackend_FormatContext_TruncatesTitleKeepingPinnedBlockUnderOuterCap(t *testing.T) {
	longTitle := strings.Repeat("T", 300) // 300 bytes of title
	body := strings.Repeat("🔥", contextPinnedTruncLen)
	b := singlePinnedFactBackend(t, longTitle, body)

	out, err := b.FormatContext("proj", "global")
	if err != nil {
		t.Fatalf("FormatContext: %v", err)
	}

	// Pre-cap guard: with the title rendered whole, the pinned line ALONE (which
	// the outer cap never drops) already carries the block over contextByteCap.
	untruncated := fmt.Sprintf("- [decision] **%s**: %s\n", longTitle, body)
	if precap := len(joinLayeredContext([]string{untruncated}, nil)); precap <= contextByteCap {
		t.Fatalf("fixture renders %d bytes with the title untruncated, already under the %d-byte "+
			"cap: the bypass is not exercised, enlarge the title or body", precap, contextByteCap)
	}

	if len(out) > contextByteCap {
		t.Fatalf("context %d bytes, cap %d:\n%s", len(out), contextByteCap, out)
	}
	wantTitle := strings.Repeat("T", contextTitleTruncLen) + "…"
	if !strings.Contains(out, "**"+wantTitle+"**") {
		t.Fatalf("title must render cut to %d runes with an ellipsis:\n%s", contextTitleTruncLen, out)
	}
	if strings.Contains(out, longTitle) {
		t.Fatalf("untruncated title must not render:\n%s", out)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("the pinned body must still render whole (only the title is cut):\n%s", out)
	}
}

// assertBackendContextSectionOrder pins the layered section order of a
// MemoryLake context block: Pinned before Recent Observations, and no
// "### Recent Sessions" section at all (this backend has no session tracking).
// Mirrors internal/store's assertContextSectionOrder, minus sessions.
func assertBackendContextSectionOrder(t *testing.T, out string) {
	t.Helper()
	pinned := strings.Index(out, pinnedSectionHeading)
	observations := strings.Index(out, "### Recent Observations")
	if pinned < 0 || observations < 0 {
		t.Fatalf("expected both section headings (pinned=%d observations=%d):\n%s", pinned, observations, out)
	}
	if pinned >= observations {
		t.Fatalf("section order must be Pinned < Recent Observations, got %d/%d:\n%s", pinned, observations, out)
	}
	if strings.Contains(out, "### Recent Sessions") {
		t.Fatalf("MemoryLake backend has no sessions to render:\n%s", out)
	}
}
