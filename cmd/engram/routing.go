package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
)

// Compile-time assertion that *memorylake.MemoryLakeBackend implements the
// real internal/mcp.MemoryBackend interface end-to-end. internal/mcp and
// internal/memorylake cannot import each other (that would create an import
// cycle), so cmd/engram — which already imports both to assemble the MCP
// server — is where this gets checked for real. (internal/memorylake keeps
// its own byte-for-byte mirrored interface assertion in backend_test.go so
// it fails loudly on its own if the two ever drift; this is the assertion
// that matters against the actual mcp.MemoryBackend type.)
var _ mcp.MemoryBackend = (*memorylake.MemoryLakeBackend)(nil)

// NewRoutingSelector returns an mcp.BackendSelector that routes each project
// to either the shared local SQLite backend or a per-project MemoryLake
// backend, depending on enab.
//
// Routing rules, in order:
//  1. ENGRAM_BACKEND=sqlite is a global safety valve: every project resolves
//     to sqlite, regardless of enablement state.
//  2. A project not present in enab resolves to sqlite (the default —
//     MemoryLake is strictly opt-in per project).
//  3. An enabled project resolves to a MemoryLake backend. If the
//     enablement entry has no MemoryLake project id yet (Task 3 can enable a
//     project before the id is known), the workspace and project id are
//     resolved here, the entry is backfilled, and the enablement file is
//     saved (best-effort — a save failure does not block the request).
//  4. Any failure resolving or constructing the MemoryLake backend (server
//     unreachable, bad API key, ...) falls back to sqlite for *this* call
//     rather than failing the mem_* call outright. Unlike a success, a
//     fallback is deliberately NOT cached: a single transient failure (a
//     network blip, a briefly-down MemoryLake) must not pin the project to
//     sqlite for the rest of the process lifetime and silently diverge from
//     the shared backend. The next call retries the resolution.
//
// MemoryLake backends are constructed lazily and cached per project. Crucially,
// the (possibly slow / failing) network resolution for one project must never
// block routing for any other project. A single process-wide mutex held across
// resolveMemoryLakeBackend would do exactly that: with MemoryLake down, every
// enabled project retries the full network sequence (each HTTP call bounded by
// ENGRAM_MEMORYLAKE_TIMEOUT_MS, default 30s) under that lock, serializing even
// unrelated healthy/sqlite projects behind a ~30-60s stall. Instead:
//
//   - A short-lived global mutex (gmu) guards only the shared, mutable state:
//     the per-project entry map and access to enab.EnabledProjects (read via
//     IsEnabled, backfilled + persisted during resolution). gmu is NEVER held
//     across a network call.
//   - Each project gets its own *projEntry with its own mutex. All network I/O
//     for a project happens under that per-project lock, so a hung/failing
//     project blocks only itself. Concurrent first-resolves of the SAME project
//     coalesce behind that lock (singleflight — no construction stampede).
//   - Only a successfully-constructed MemoryLake backend is cached (backend !=
//     nil). A fallback to sqlite is left uncached so a transient failure is
//     retried on the next call for that project — and the retry, too, is
//     confined to that project's lock and never blocks others.
func NewRoutingSelector(sqlite mcp.MemoryBackend, cfg memorylake.Config, enab *memorylake.Enablement) mcp.BackendSelector {
	type projEntry struct {
		mu      sync.Mutex        // serializes resolution for THIS project only
		backend mcp.MemoryBackend // cached genuine MemoryLake backend; nil until resolved
	}

	var gmu sync.Mutex
	entries := map[string]*projEntry{}

	return func(project string) mcp.MemoryBackend {
		if os.Getenv("ENGRAM_BACKEND") == "sqlite" {
			return sqlite
		}
		if enab == nil {
			return sqlite
		}

		// Short global critical section: read enablement state and fetch (or
		// create) this project's entry. enab.EnabledProjects is a shared map
		// that resolveMemoryLakeBackend backfills, so every read/write of it is
		// serialized by gmu — but gmu is released before any network I/O.
		gmu.Lock()
		entry, ok := enab.IsEnabled(project)
		if !ok {
			gmu.Unlock()
			return sqlite
		}
		pe := entries[project]
		if pe == nil {
			pe = &projEntry{}
			entries[project] = pe
		}
		gmu.Unlock()

		// Per-project critical section. Different projects hold different
		// pe.mu, so slow/failing resolution here cannot block another project
		// (or a sqlite/non-enabled project, which returned above without ever
		// reaching this lock). Concurrent first calls for the same project
		// coalesce here: the first resolves, the rest see the cached backend.
		pe.mu.Lock()
		defer pe.mu.Unlock()

		if pe.backend != nil {
			return pe.backend
		}

		backend, ok := resolveMemoryLakeBackend(project, entry, cfg, enab, &gmu, sqlite)
		if ok {
			// Only cache a genuine MemoryLake backend. A fallback to sqlite is
			// left uncached so a transient failure is retried on the next call.
			pe.backend = backend
		}
		return backend
	}
}

// resolveMemoryLakeBackend does the actual (possibly network-calling)
// resolution for one enabled project: filling in a missing MemoryLake
// project id when necessary, then constructing the backend. It returns
// (backend, true) on success and (sqlite, false) on any failure along the
// way (with a stderr warning) instead of propagating the error to the
// caller. The bool lets the caller distinguish a real MemoryLake backend
// (safe to cache) from a transient fallback (must not be cached, so the next
// call retries).
//
// The caller holds this project's per-project lock (never the global lock),
// so the network calls below (each bounded by ENGRAM_MEMORYLAKE_TIMEOUT_MS)
// serialize only this project. gmu is acquired for the single short critical
// section that mutates and persists the shared enab.EnabledProjects map — and
// released again — so backfilling one project's resolved id can never race
// with, or stall, another project's routing.
func resolveMemoryLakeBackend(project string, entry memorylake.ProjectEntry, cfg memorylake.Config, enab *memorylake.Enablement, gmu *sync.Mutex, sqlite mcp.MemoryBackend) (mcp.MemoryBackend, bool) {
	ws := cfg.Workspace
	projID := entry.ProjID

	if projID == "" {
		client := memorylake.NewClient(cfg)

		// Network I/O — deliberately NOT under gmu.
		resolvedWS, err := client.ResolveWorkspaceID(cfg.Workspace)
		if err != nil {
			warnMemoryLakeFallback(project, "resolving workspace", err)
			return sqlite, false
		}

		newProjID, err := client.EnsureProject(resolvedWS, project)
		if err != nil {
			warnMemoryLakeFallback(project, "ensuring project", err)
			return sqlite, false
		}

		ws = resolvedWS
		projID = newProjID

		// Backfill the resolved id into the shared enablement map and persist
		// it. This touches enab.EnabledProjects (shared across all projects)
		// and enab.Save, so it must be serialized by gmu — but the network
		// calls above were not, and gmu is released again immediately (the
		// enclosing per-project lock still guards this project's resolution).
		entry.ProjID = projID
		gmu.Lock()
		if enab.EnabledProjects == nil {
			enab.EnabledProjects = map[string]memorylake.ProjectEntry{}
		}
		enab.EnabledProjects[project] = entry
		// Best-effort: a failed save must not block the current request —
		// resolution will simply be retried (and the file re-saved) next
		// time this project's cache entry is evicted (e.g. process restart).
		saveErr := enab.Save(memorylake.DefaultEnablementPath())
		gmu.Unlock()
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "[engram] memorylake: failed to persist resolved project id for %q (continuing): %v\n", project, saveErr)
		}
	}

	// Network I/O — deliberately NOT under gmu.
	backend, err := memorylake.NewBackend(cfg, ws, projID)
	if err != nil {
		warnMemoryLakeFallback(project, "constructing backend", err)
		return sqlite, false
	}
	return backend, true
}

func warnMemoryLakeFallback(project, step string, err error) {
	fmt.Fprintf(os.Stderr, "[engram] memorylake: %s for project %q failed, falling back to sqlite: %v\n", step, project, err)
}
