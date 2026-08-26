package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// stubBackend is a minimal mcp.MemoryBackend used to check identity of the
// backend a BackendSelector resolves to, without needing all 29 methods
// implemented — embedding the nil interface satisfies MemoryBackend and
// these tests never invoke a method on it, only compare identity/type.
type stubBackend struct {
	mcp.MemoryBackend
	name string
}

// newRoutingTestStore builds a throwaway sqlite store for the fallback backend
// these selector tests hand to buildRoutingSelector.
func newRoutingTestStore(t *testing.T) *store.Store {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// routingTestActor pins the MemoryLake actor for the routing tests below.
// They exercise backend selection, not identity resolution: with no actor set
// NewBackend consults /defaults/my-actor to find the API key's owner, which
// every fake server here would then have to serve. Pinning it keeps these
// tests about routing and insulated from how the actor is resolved.
const routingTestActor = "test-machine"

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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "ws-1", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
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

// mlStubServer returns an httptest server that satisfies the MemoryLake
// calls memorylake.NewBackend makes for a project whose ProjID is already
// resolved (actor registration against workspace ws-1), mirroring
// TestNewRoutingSelector_EnabledWithProjIDRoutesToMemoryLake.
func mlStubServer(t *testing.T) *httptest.Server {
	t.Helper()
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
	t.Cleanup(srv.Close)
	return srv
}

// Regression for "enable during a live session has no effect": `engram
// memorylake enable --project X` runs in another process and rewrites
// ~/.engram/memorylake.json while an `engram mcp` process is already serving
// a session. The selector must pick the change up on the next call instead
// of pinning the snapshot loaded at process start.
func TestNewRoutingSelector_PicksUpExternalEnableWithoutRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := mlStubServer(t)
	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "ws-1", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}

	// Simulate `engram mcp` starting before any project is enabled: the
	// enablement file does not exist yet.
	enab, err := memorylake.LoadEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		t.Fatalf("LoadEnablement: %v", err)
	}
	sel := NewRoutingSelector(sqlite, cfg, enab)

	if got := sel("myproj"); got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("expected sqlite before enable, got %T", got)
	}

	// Simulate `engram memorylake enable --project myproj` from another
	// process: rewrite the enablement file on disk.
	external := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: "proj-42"},
	}}
	if err := external.Save(memorylake.DefaultEnablementPath()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got := sel("myproj")
	if _, ok := got.(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("expected MemoryLake backend after external enable, got %T", got)
	}
}

// The symmetric half: `engram memorylake disable --project X` during a live
// session must route the project back to sqlite on the next call.
func TestNewRoutingSelector_PicksUpExternalDisableWithoutRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := mlStubServer(t)
	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "ws-1", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}

	initial := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: "proj-42"},
	}}
	if err := initial.Save(memorylake.DefaultEnablementPath()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	enab, err := memorylake.LoadEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		t.Fatalf("LoadEnablement: %v", err)
	}
	sel := NewRoutingSelector(sqlite, cfg, enab)

	if _, ok := sel("myproj").(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("expected MemoryLake backend while enabled")
	}

	// Simulate `engram memorylake disable --project myproj` from another
	// process: rewrite the file without the project.
	external := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{}}
	if err := external.Save(memorylake.DefaultEnablementPath()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := sel("myproj"); got != mcp.MemoryBackend(sqlite) {
		t.Fatalf("expected sqlite after external disable, got %T", got)
	}
}

// The enablement path must be captured when the selector is constructed, not
// re-evaluated at save time. A late in-flight resolution (e.g. a goroutine
// finishing after a test restored $HOME, or any process-level HOME change)
// must never write the backfilled proj_id to whatever ~/.engram/memorylake.json
// resolves to at that moment — that is how test fixtures once destroyed a
// user's real connection config.
func TestNewRoutingSelector_BackfillSavesToConstructionTimePath(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()
	t.Setenv("HOME", homeA)

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

	cfg := memorylake.Config{BaseURL: srv.URL, APIKey: "sk-test", Actor: routingTestActor, Workspace: "engram", TimeoutMS: 5000}
	sqlite := &stubBackend{name: "sqlite"}
	enab := &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{
		"myproj": {ProjID: ""},
	}}
	sel := NewRoutingSelector(sqlite, cfg, enab)

	// HOME moves after construction (as when t.Setenv restores the real HOME
	// while a resolution goroutine is still in flight).
	t.Setenv("HOME", homeB)

	if _, ok := sel("myproj").(*memorylake.MemoryLakeBackend); !ok {
		t.Fatalf("expected MemoryLake backend after resolution")
	}

	saved, err := memorylake.LoadEnablement(filepath.Join(homeA, ".engram", "memorylake.json"))
	if err != nil {
		t.Fatalf("LoadEnablement(homeA): %v", err)
	}
	if saved.EnabledProjects["myproj"].ProjID != "proj-new" {
		t.Fatalf("expected backfill saved under construction-time HOME, got %q", saved.EnabledProjects["myproj"].ProjID)
	}
	if _, err := os.Stat(filepath.Join(homeB, ".engram", "memorylake.json")); !os.IsNotExist(err) {
		t.Fatalf("backfill must not write to the post-construction HOME (stat err=%v)", err)
	}
}

// TestNewRoutingSelector_ConversationSyncToggleAppliesWithoutRestart is the
// point of the whole change: a `memorylake conversations enable` performed by
// another process, while this selector already holds a constructed and cached
// backend, must take effect on the very next resolve.
//
// It asserts through observable behaviour rather than by reading the flag: with
// suppression off a prompt reaches the network, with it on the same call makes
// no request at all. Reading the private field would pass even if the prompt
// path ignored it.
func TestNewRoutingSelector_ConversationSyncToggleAppliesWithoutRestart(t *testing.T) {
	var msgPosts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "actor-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/actors":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&msgPosts, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	setupMemoryLakeEnv(t, srv.URL, "acme", "proj-1")
	enabPath := memorylake.DefaultEnablementPath()

	sel := buildRoutingSelector(mcp.NewSQLiteBackend(newRoutingTestStore(t)))

	// First resolve constructs and caches the backend. Suppression is off, so a
	// prompt is really appended.
	b1 := sel("acme")
	if _, err := b1.AddPrompt(store.AddPromptParams{SessionID: "s1", Project: "acme", Content: "before toggle"}); err != nil {
		t.Fatalf("AddPrompt before toggle: %v", err)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 1 {
		t.Fatalf("msgPosts=%d, want 1 before the toggle", got)
	}

	// Another process flips the switch on. Bump mtime so the fingerprint the
	// selector keys off actually changes (a same-second write with an identical
	// size would otherwise be invisible).
	enab, err := memorylake.LoadEnablement(enabPath)
	if err != nil {
		t.Fatalf("LoadEnablement: %v", err)
	}
	if err := enab.SetConversationSync("acme", true); err != nil {
		t.Fatalf("SetConversationSync: %v", err)
	}
	if err := enab.Save(enabPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(enabPath, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Same selector, no restart, no reconstruction: the append must now be
	// suppressed.
	b2 := sel("acme")
	if _, err := b2.AddPrompt(store.AddPromptParams{SessionID: "s1", Project: "acme", Content: "after toggle"}); err != nil {
		t.Fatalf("AddPrompt after toggle: %v", err)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 1 {
		t.Fatalf("msgPosts=%d, want still 1 — the toggle must apply without a restart", got)
	}

	// And the disable direction, which strands a session the other way if it
	// does not take effect.
	enab2, err := memorylake.LoadEnablement(enabPath)
	if err != nil {
		t.Fatalf("LoadEnablement (2): %v", err)
	}
	if err := enab2.SetConversationSync("acme", false); err != nil {
		t.Fatalf("SetConversationSync(off): %v", err)
	}
	if err := enab2.Save(enabPath); err != nil {
		t.Fatalf("Save (2): %v", err)
	}
	later := future.Add(2 * time.Second)
	if err := os.Chtimes(enabPath, later, later); err != nil {
		t.Fatalf("Chtimes (2): %v", err)
	}

	b3 := sel("acme")
	if _, err := b3.AddPrompt(store.AddPromptParams{SessionID: "s1", Project: "acme", Content: "after disable"}); err != nil {
		t.Fatalf("AddPrompt after disable: %v", err)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 2 {
		t.Fatalf("msgPosts=%d, want 2 — disabling must resume standalone prompt appends", got)
	}
}
