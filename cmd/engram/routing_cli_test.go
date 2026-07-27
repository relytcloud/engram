package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// fakeMemoryLakeRecorder records what newFakeMemoryLakeServer's fake API was
// asked to do, so tests can assert the CLI really went through MemoryLake's
// direct fact-add write path rather than inferring it from the absence of a
// local sqlite row.
type fakeMemoryLakeRecorder struct {
	mu sync.Mutex
	// factPosts counts POST .../projects/proj-1/memories/facts calls (the
	// direct fact-add endpoint AddObservation writes through).
	factPosts int
	// factTexts collects every verbatim fact string posted to that endpoint,
	// in order.
	factTexts []string
}

func (r *fakeMemoryLakeRecorder) snapshot() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.factPosts, append([]string(nil), r.factTexts...)
}

// newFakeMemoryLakeServer stands up a minimal MemoryLake V3 API covering
// exactly what MemoryLakeBackend.AddObservation / Search / NewBackend need:
// actor provisioning, the direct fact-add endpoint, and search.
//
// AddObservation writes verbatim through MemoryLake's direct fact-add
// endpoint — POST /api/v3/workspaces/<ws>/projects/<projID>/memories/facts
// with body {"facts": ["<text>"]}, answered with
// {"success":true,"data":{"facts":[{"id":...,"fact":...}]}} (see
// internal/memorylake/writequeue.go's Client.AddFacts and backend.go's
// AddObservation). It does NOT go through conversations/messages plus async
// fact extraction anymore, so this fixture deliberately serves no
// conversation, message, or fact-backfill/PATCH routes: an attempt to use
// them is a real regression and must fail the test.
//
// Mirrors internal/memorylake/backend_test.go's
// TestBackend_AddObservation_WritesVerbatimFactDirectly fixture.
func newFakeMemoryLakeServer(t *testing.T, searchHits []map[string]any) (*httptest.Server, *fakeMemoryLakeRecorder) {
	t.Helper()
	rec := &fakeMemoryLakeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			// Still live, but only for session lifecycle: CreateSession
			// ensures a conversation keyed by the session id (backend.go).
			// Observations no longer flow through it.
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			var body struct {
				Facts []string `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			rec.mu.Lock()
			rec.factPosts++
			rec.factTexts = append(rec.factTexts, body.Facts...)
			rec.mu.Unlock()
			created := make([]map[string]any, 0, len(body.Facts))
			for i, f := range body.Facts {
				created = append(created, map[string]any{
					"id": fmt.Sprintf("fact-%d", i+1), "fact": f, "metadata": map[string]any{},
				})
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"facts": created}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/search":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"facts": searchHits}})
		default:
			// Must NOT be t.Fatalf: this runs on the http handler goroutine,
			// where runtime.Goexit kills the whole test binary (the package
			// then fails with no named test in CI logs). Fail loudly but let
			// the request — and the test — finish.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestCmdSave_EnabledProjectRoutesToMemoryLake is the RED/GREEN case for
// Task 15: `engram save --project <enabled>` must route through the same
// NewRoutingSelector `engram mcp` uses, landing the observation in
// MemoryLake instead of local sqlite.
func TestCmdSave_EnabledProjectRoutesToMemoryLake(t *testing.T) {
	srv, rec := newFakeMemoryLakeServer(t, nil)
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
	// The printed id is the sync id the backend returned, i.e. the real fact
	// id the direct fact-add endpoint handed back — not a local sqlite rowid.
	if !strings.Contains(stdout, "#fact-1") {
		t.Fatalf("expected save output to report the MemoryLake fact id, got: %q", stdout)
	}

	// The write went through the direct fact-add endpoint, verbatim.
	posts, texts := rec.snapshot()
	if posts != 1 {
		t.Fatalf("fact-add posts=%d, want exactly 1", posts)
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "ml-content") || !strings.Contains(texts[0], "ml-title") {
		t.Fatalf("posted fact texts=%q, want one text carrying the title and content verbatim", texts)
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
		// t.Errorf, not t.Fatalf: this is the http handler goroutine, where
		// a Fatal's runtime.Goexit would tear down the whole test binary.
		t.Errorf("unexpected MemoryLake network call for a non-enabled project: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
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
