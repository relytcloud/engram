package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// setupMemoryLakeEnv points ENGRAM_MEMORYLAKE_* at srv and pre-enables
// project with the given MemoryLake project id, writing the enablement file
// buildRoutingSelector (via loadMemorylakeEnablement) reads. Using a
// "ws-"-prefixed workspace lets memorylake.ResolveWorkspaceID short-circuit
// (see identity.go), so the fake server doesn't need to answer
// GET /api/v3/workspaces.
func setupMemoryLakeEnv(t *testing.T, srvURL, project, projID string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ENGRAM_MEMORYLAKE_BASE_URL", srvURL)
	t.Setenv("ENGRAM_MEMORYLAKE_API_KEY", "sk-test")
	t.Setenv("ENGRAM_MEMORYLAKE_WORKSPACE", "ws-1")
	t.Setenv("ENGRAM_MEMORYLAKE_TIMEOUT_MS", "5000")
	t.Setenv("ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS", "5")
	t.Setenv("ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS", "2000")

	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		project: {ProjID: projID},
	}}
	if err := enab.Save(memorylake.DefaultEnablementPath()); err != nil {
		t.Fatalf("save enablement: %v", err)
	}
}

// newFakeMemoryLakeServer stands up a minimal MemoryLake V3 API covering
// exactly what MemoryLakeBackend.AddObservation / Search / NewBackend need:
// actor provisioning, conversation+message append, fact snapshot/backfill
// polling, and search. Mirrors internal/memorylake/backend_test.go's fixture
// servers (TestBackend_AddObservation_ReturnsInt64ViaBackfill /
// TestBackend_Search_PassesThrough).
func newFakeMemoryLakeServer(t *testing.T, searchHits []map[string]any) (*httptest.Server, *int32) {
	t.Helper()
	var appended int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.StoreInt32(&appended, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts"):
			var items []map[string]any
			if atomic.LoadInt32(&appended) == 1 {
				items = []map[string]any{
					{"id": "fact-1", "fact": "extracted", "metadata": map[string]any{}},
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
		case r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "extracted", "metadata": map[string]any{},
			}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"facts": searchHits}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &appended
}

// TestCmdSave_EnabledProjectRoutesToMemoryLake is the RED/GREEN case for
// Task 15: `engram save --project <enabled>` must route through the same
// NewRoutingSelector `engram mcp` uses, landing the observation in
// MemoryLake instead of local sqlite.
func TestCmdSave_EnabledProjectRoutesToMemoryLake(t *testing.T) {
	srv, _ := newFakeMemoryLakeServer(t, nil)
	setupMemoryLakeEnv(t, srv.URL, "myproj", "proj-1")

	cfg := testConfig(t)
	withArgs(t, "engram", "save", "ml-title", "ml-content", "--project", "myproj")

	stdout, stderr := captureOutput(t, func() { cmdSave(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Memory saved:") {
		t.Fatalf("unexpected save output: %q", stdout)
	}

	// Verify the local sqlite store received NOTHING for this project — the
	// save went to MemoryLake, not sqlite (split-brain would show it in both
	// or only in sqlite).
	s, err := storeNew(cfg)
	if err != nil {
		t.Fatalf("storeNew: %v", err)
	}
	defer s.Close()
	results, err := s.Search("ml-content", store.SearchOptions{Project: "myproj"})
	if err != nil {
		t.Fatalf("sqlite search: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected observation to NOT be in local sqlite (should be routed to MemoryLake), found %d", len(results))
	}
}

// TestCmdSearch_EnabledProjectRoutesToMemoryLake mirrors the save case for
// `engram search --project <enabled>`.
func TestCmdSearch_EnabledProjectRoutesToMemoryLake(t *testing.T) {
	hit := map[string]any{"id": "fact-1", "fact": "hit", "score": 0.9, "metadata": map[string]any{
		"engram_raw": "memorylake content", "engram_title": "ml-hit", "engram_type": "decision",
	}}
	srv, _ := newFakeMemoryLakeServer(t, []map[string]any{hit})
	setupMemoryLakeEnv(t, srv.URL, "myproj", "proj-1")

	cfg := testConfig(t)
	withArgs(t, "engram", "search", "hit", "--project", "myproj")

	stdout, stderr := captureOutput(t, func() { cmdSearch(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "ml-hit") {
		t.Fatalf("expected search output to contain the MemoryLake-only result, got: %q", stdout)
	}
}

// TestCmdSaveAndSearch_NotEnabledProjectNeverHitsMemoryLakeNetwork proves
// non-enabled projects are byte-for-byte unaffected by routing: even with
// ENGRAM_MEMORYLAKE_* configured and an enablement file present, a project
// NOT in it must never make a network call — the fake server fails the test
// if it receives any request.
func TestCmdSaveAndSearch_NotEnabledProjectNeverHitsMemoryLakeNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected MemoryLake network call for a non-enabled project: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	setupMemoryLakeEnv(t, srv.URL, "some-other-enabled-project", "proj-1")

	cfg := testConfig(t)
	withArgs(t, "engram", "save", "sqlite-title", "sqlite-content", "--project", "not-enabled")
	stdout, stderr := captureOutput(t, func() { cmdSave(cfg) })
	if stderr != "" {
		t.Fatalf("expected no stderr, got: %q", stderr)
	}
	if !strings.Contains(stdout, "Memory saved:") {
		t.Fatalf("unexpected save output: %q", stdout)
	}

	withArgs(t, "engram", "search", "sqlite-content", "--project", "not-enabled")
	searchOut, searchErr := captureOutput(t, func() { cmdSearch(cfg) })
	if searchErr != "" {
		t.Fatalf("expected no stderr, got: %q", searchErr)
	}
	if !strings.Contains(searchOut, "sqlite-title") {
		t.Fatalf("expected sqlite-backed search to find the observation, got: %q", searchOut)
	}
}
