package memorylake

import (
	"encoding/json"
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

// TestBackend_AddObservation_AppendsAndReturnsImmediately is the Option A
// thin-adapter write path (spec §2/§4/task-7 brief): AddObservation only
// ensures the conversation and appends the message — no snapshot GET, no
// polling/backfill GET, no PATCH. It must return promptly with the MemoryLake
// message id as a pending sync_id.
func TestBackend_AddObservation_AppendsAndReturnsImmediately(t *testing.T) {
	var convPosts, msgPosts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			convPosts++
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			msgPosts++
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s (AddObservation must not GET/PATCH facts any more)", r.Method, r.URL.Path)
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
	if id != "msg-1" {
		t.Fatalf("id=%q, want msg-1 (the MemoryLake message id, returned as a pending sync_id)", id)
	}
	if convPosts != 1 || msgPosts != 1 {
		t.Fatalf("convPosts=%d msgPosts=%d, want 1/1", convPosts, msgPosts)
	}
}

// TestBackend_AddObservation_EmptyMessageIDFallsBackToContentHash covers the
// defensive fallback when MemoryLake's response carries no message id.
func TestBackend_AddObservation_EmptyMessageIDFallsBackToContentHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": ""}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
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

// TestBackend_AddObservation_EmptySessionUsesDefaultConversation verifies the
// convCustomID fallback for a save with no session id.
func TestBackend_AddObservation_EmptySessionUsesDefaultConversation(t *testing.T) {
	var gotConvCustomID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-default"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-default/messages":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if _, err := b.AddObservation(store.AddObservationParams{Content: "no session here"}); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if gotConvCustomID != defaultConversationCustomID {
		t.Fatalf("conversation custom_id=%q, want %q", gotConvCustomID, defaultConversationCustomID)
	}
}

// TestBackend_AddObservation_ConcurrentSavesAllSucceed exercises the
// concurrent-save path under `-race`: since AddObservation no longer reads or
// claims any project-wide fact snapshot, concurrent saves for distinct
// content have no cross-claim window left to regress — this just asserts they
// all succeed and each gets its own message id.
func TestBackend_AddObservation_ConcurrentSavesAllSucceed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-" + body.CustomID}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
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

// TestBackend_FormatContext_TruncatesContentTo300 is the I3 regression:
// internal/store's FormatContext truncates every observation's content to
// 300 runes before rendering it (store.go's truncate(obs.Content, 300)); the
// MemoryLake backend must match that so a project's rendered context block
// has the same shape regardless of which backend produced it.
func TestBackend_FormatContext_TruncatesContentTo300(t *testing.T) {
	long := strings.Repeat("a", 400)
	items := []map[string]any{
		{"id": "fact-1", "fact": long, "created_at": "2026-07-20T00:00:00Z",
			"metadata": map[string]any{metaTitle: "T", metaType: "note", metaScope: "global"}},
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
