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
//     unreachable, bad API key, ...) falls back to sqlite rather than
//     failing the mem_* call outright. The fallback (like a success) is
//     cached per project so a broken MemoryLake config does not retry the
//     failing network calls on every single tool invocation.
//
// MemoryLake backends (and fallback decisions) are constructed lazily and
// cached per project behind a mutex, since BackendSelector is called
// concurrently by mem_* tool handlers.
func NewRoutingSelector(sqlite mcp.MemoryBackend, cfg memorylake.Config, enab *memorylake.Enablement) mcp.BackendSelector {
	var mu sync.Mutex
	cache := map[string]mcp.MemoryBackend{}

	return func(project string) mcp.MemoryBackend {
		if os.Getenv("ENGRAM_BACKEND") == "sqlite" {
			return sqlite
		}
		if enab == nil {
			return sqlite
		}

		// enab.EnabledProjects is a plain map, mutated (backfilled with a
		// resolved MemoryLake project id) inside resolveMemoryLakeBackend
		// below while mu is held. Reading it via IsEnabled must happen under
		// the same mu — not before acquiring it — or a concurrent read here
		// can race with that write (mem-go's stdio server dispatches mem_*
		// calls to multiple worker goroutines, so two different projects can
		// call this selector at the same time).
		mu.Lock()
		defer mu.Unlock()

		entry, ok := enab.IsEnabled(project)
		if !ok {
			return sqlite
		}

		if cached, hit := cache[project]; hit {
			return cached
		}

		backend := resolveMemoryLakeBackend(project, entry, cfg, enab, sqlite)
		cache[project] = backend
		return backend
	}
}

// resolveMemoryLakeBackend does the actual (possibly network-calling)
// resolution for one enabled project: filling in a missing MemoryLake
// project id when necessary, then constructing the backend. Any failure
// along the way falls back to sqlite with a stderr warning instead of
// propagating the error to the caller.
func resolveMemoryLakeBackend(project string, entry memorylake.ProjectEntry, cfg memorylake.Config, enab *memorylake.Enablement, sqlite mcp.MemoryBackend) mcp.MemoryBackend {
	ws := cfg.Workspace
	projID := entry.ProjID

	if projID == "" {
		client := memorylake.NewClient(cfg)

		resolvedWS, err := client.ResolveWorkspaceID(cfg.Workspace)
		if err != nil {
			warnMemoryLakeFallback(project, "resolving workspace", err)
			return sqlite
		}

		newProjID, err := client.EnsureProject(resolvedWS, project)
		if err != nil {
			warnMemoryLakeFallback(project, "ensuring project", err)
			return sqlite
		}

		ws = resolvedWS
		projID = newProjID

		entry.ProjID = projID
		if enab.EnabledProjects == nil {
			enab.EnabledProjects = map[string]memorylake.ProjectEntry{}
		}
		enab.EnabledProjects[project] = entry
		// Best-effort: a failed save must not block the current request —
		// resolution will simply be retried (and the file re-saved) next
		// time this project's cache entry is evicted (e.g. process restart).
		if err := enab.Save(memorylake.DefaultEnablementPath()); err != nil {
			fmt.Fprintf(os.Stderr, "[engram] memorylake: failed to persist resolved project id for %q (continuing): %v\n", project, err)
		}
	}

	backend, err := memorylake.NewBackend(cfg, ws, projID)
	if err != nil {
		warnMemoryLakeFallback(project, "constructing backend", err)
		return sqlite
	}
	return backend
}

func warnMemoryLakeFallback(project, step string, err error) {
	fmt.Fprintf(os.Stderr, "[engram] memorylake: %s for project %q failed, falling back to sqlite: %v\n", step, project, err)
}
