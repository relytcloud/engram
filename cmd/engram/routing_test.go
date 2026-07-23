package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// stubBackend is a minimal mcp.MemoryBackend used to check identity of the
// backend a BackendSelector resolves to, without needing all 29 methods
// implemented — embedding the nil interface satisfies MemoryBackend and
// these tests never invoke a method on it, only compare identity/type.
type stubBackend struct {
	mcp.MemoryBackend
	name string
}

func TestNewRoutingSelector_EnvOverrideForcesSQLite(t *testing.T) {
	t.Setenv("ENGRAM_BACKEND", "sqlite")

	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: "proj-1"},
	}}

	sel := NewRoutingSelector(sqlite, memorylake.Config{}, enab)
	got := sel("myproj")
	if got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("expected sqlite fallback when ENGRAM_BACKEND=sqlite, got %T", got)
	}
}

func TestNewRoutingSelector_NotEnabledUsesSQLite(t *testing.T) {
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{}}

	sel := NewRoutingSelector(sqlite, memorylake.Config{}, enab)
	got := sel("unknown-project")
	if got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("expected sqlite for non-enabled project, got %T", got)
	}
}

func TestNewRoutingSelector_EnabledWithProjIDRoutesToMemoryLake(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "ws-1", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: "proj-42"},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)
	got := sel("myproj")
	if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("expected *memorylake.MemoryLakeBackend for enabled project, got %T", got)
	}

	// Second call for the same project must reuse the cached backend rather
	// than constructing a new one (identity check).
	got2 := sel("myproj")
	if got != got2 {
		t.Fatalf("expected cached MemoryLake backend to be reused across calls")
	}
}

func TestNewRoutingSelector_EnabledEmptyProjIDResolvesAndSaves(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items": []map[string]any{{"id": "ws-1", "name": "engram", "custom_id": "engram"}},
			}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "proj-new"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: ""},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)
	got := sel("myproj")
	if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("expected *memorylake.MemoryLakeBackend once projID resolved, got %T", got)
	}

	if enab.EnabledProjects["myproj"].ProjID != "proj-new" {
		t.Fatalf("expected in-memory entry to be backfilled with resolved projID, got %q", enab.EnabledProjects["myproj"].ProjID)
	}

	// NewRoutingSelector saves the resolved projID to memorylake.DefaultEnablementPath()
	// (derived from $HOME, set above), best-effort.
	saved, err := memorylake.LoadEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		t.Fatalf("LoadEnablement: %v", err)
	}
	if saved.EnabledProjects["myproj"].ProjID != "proj-new" {
		t.Fatalf("expected saved enablement file to persist resolved projID, got %q", saved.EnabledProjects["myproj"].ProjID)
	}
}

func TestNewRoutingSelector_ResolutionFailureFallsBackToSQLite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "boom", "error_code": "INTERNAL"})
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: ""},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)
	got := sel("myproj")
	if got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("expected sqlite fallback on resolution failure, got %T", got)
	}
}

// TestNewRoutingSelector_TransientFailureIsNotCachedAndRetries is the FIX #5
// regression: a fallback to sqlite must NOT be cached. A single transient
// failure (network blip / MemoryLake briefly down) must not pin the project to
// sqlite for the process lifetime and silently diverge from the shared
// backend. The first call fails and falls back to sqlite; once the server
// recovers, the next call must retry the resolution and route to MemoryLake
// (not return a cached sqlite fallback).
func TestNewRoutingSelector_TransientFailureIsNotCachedAndRetries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	healthy := false // starts down; flip to healthy between the two selector calls
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := healthy
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "boom", "error_code": "INTERNAL"})
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items": []map[string]any{{"id": "ws-1", "name": "engram", "custom_id": "engram"}},
			}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "proj-new"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: ""},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)

	// First call: server is down → transient failure → sqlite fallback.
	if got := sel("myproj"); got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("first call: expected sqlite fallback while server down, got %T", got)
	}

	// Server recovers.
	mu.Lock()
	healthy = true
	mu.Unlock()

	// Second call must RETRY (the failure was not cached) and route to MemoryLake.
	got := sel("myproj")
	if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("second call: expected retry to construct *memorylake.MemoryLakeBackend after recovery, got %T", got)
	}
}

// TestNewRoutingSelector_ConcurrentEnabledUnresolvedProjectsNoDataRace
// reproduces the data race fixed by guarding enab.EnabledProjects access with
// mu end-to-end: 20 distinct enabled-but-unresolved projects are routed
// concurrently (mirroring mcp-go's stdio server, which dispatches concurrent
// mem_* calls across multiple worker goroutines). Before the fix, the
// unlocked enab.IsEnabled read here raced with the locked
// enab.EnabledProjects[project] = entry write in resolveMemoryLakeBackend and
// `go test -race` reported a concurrent map read/write (and could fatal the
// process with "concurrent map writes" outside of -race too). This must pass
// cleanly under `go test -race ./cmd/engram/`.
func TestNewRoutingSelector_ConcurrentEnabledUnresolvedProjectsNoDataRace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items": []map[string]any{{"id": "ws-1", "name": "engram", "custom_id": "engram"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "resolved-" + body.CustomID}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}

	const numProjects = 20
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{}}
	for i := 0; i < numProjects; i++ {
		enab.EnabledProjects[fmt.Sprintf("concurrent-proj-%d", i)] = memorylake.ProjectEntry{}
	}

	sel := NewRoutingSelector(sqlite, cfg, enab)

	results := make([]mcp.MemoryBackend, numProjects)
	var wg sync.WaitGroup
	for i := 0; i < numProjects; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = sel(fmt.Sprintf("concurrent-proj-%d", i))
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
			t.Fatalf("project %d: expected *memorylake.MemoryLakeBackend, got %T", i, got)
		}
	}
}

// TestNewRoutingSelector_SlowProjectDoesNotBlockOthers is the FIX A regression:
// a project whose MemoryLake resolution is stuck in a slow/failing network call
// must NOT block routing for any other project. Before the fix, the selector
// held one process-wide mutex across the whole resolution (including the
// bounded-but-slow HTTP calls), so an in-flight resolve for one project
// serialized every other project — including unrelated healthy or sqlite
// projects — behind it. This test wedges "slow-proj" mid-network (its POST to
// create the project blocks on a channel) and asserts that both an unrelated
// enabled project ("fast-proj") and a non-enabled sqlite project resolve
// promptly while slow-proj is still stuck.
func TestNewRoutingSelector_SlowProjectDoesNotBlockOthers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	slowReached := make(chan struct{})
	var slowOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items": []map[string]any{{"id": "ws-1", "name": "engram", "custom_id": "engram"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.CustomID == "slow-proj" {
				// Signal we're mid-network for slow-proj (its per-project lock
				// held, but NOT the global lock), then block indefinitely.
				slowOnce.Do(func() { close(slowReached) })
				<-release
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "resolved-" + body.CustomID}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"slow-proj": {ProjID: ""},
		"fast-proj": {ProjID: ""},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)

	// Wedge slow-proj mid-resolution in the background.
	go sel("slow-proj")
	select {
	case <-slowReached:
	case <-time.After(3 * time.Second):
		t.Fatal("slow-proj never reached its blocking network call")
	}

	// While slow-proj is stuck, an unrelated enabled project must still resolve.
	fastDone := make(chan mcp.MemoryBackend, 1)
	go func() { fastDone <- sel("fast-proj") }()
	select {
	case got := <-fastDone:
		if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
			t.Fatalf("fast-proj: expected *memorylake.MemoryLakeBackend while slow-proj blocked, got %T", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fast-proj routing was blocked by slow-proj's in-flight network I/O (global-lock regression)")
	}

	// A non-enabled (sqlite) project must also be unaffected.
	sqliteDone := make(chan mcp.MemoryBackend, 1)
	go func() { sqliteDone <- sel("not-enabled") }()
	select {
	case got := <-sqliteDone:
		if got != mcp.MemoryBackend(sqlite) {
			t.Fatalf("not-enabled: expected sqlite while slow-proj blocked, got %T", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sqlite routing was blocked by slow-proj's in-flight network I/O (global-lock regression)")
	}

	releaseAll()
}

// TestNewRoutingSelector_SameProjectConcurrentFirstCallsResolveOnce is the FIX A
// singleflight guarantee: many concurrent first calls for the SAME project must
// coalesce into a single resolution/construction rather than each firing its
// own network sequence (a construction stampede). The mock counts how many
// times the project is created; it must be exactly one, and every caller must
// receive the identical cached backend instance.
func TestNewRoutingSelector_SameProjectConcurrentFirstCallsResolveOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var createCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"items": []map[string]any{{"id": "ws-1", "name": "engram", "custom_id": "engram"}},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": []map[string]any{}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/projects":
			atomic.AddInt32(&createCount, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "proj-solo"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-x"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"solo": {ProjID: ""},
	}}

	sel := NewRoutingSelector(sqlite, cfg, enab)

	const n = 16
	results := make([]mcp.MemoryBackend, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = sel("solo")
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&createCount); got != 1 {
		t.Fatalf("project created %d times, want exactly 1 (singleflight coalescing)", got)
	}
	first, ok := results[0].(*memorylake.MemoryLakeBackend)
	if !ok {
		t.Fatalf("result 0: expected *memorylake.MemoryLakeBackend, got %T", results[0])
	}
	for i := 1; i < n; i++ {
		if results[i] != mcp.MemoryBackend(first) {
			t.Fatalf("result %d: expected the identical cached backend instance, got %T (%p vs %p)", i, results[i], results[i], first)
		}
	}
}
