package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/store"
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
//
// The enablement list is hot-reloaded: `engram memorylake enable/disable`
// runs in a separate process and rewrites the enablement file while a
// long-lived `engram mcp` (one per agent session) keeps serving. Pinning the
// snapshot loaded at process start would silently route a freshly-enabled
// project to sqlite until the session restarts. Each call therefore stats
// the file (mtime+size fingerprint, no I/O beyond that when unchanged) and
// re-reads it only when it changed on disk. Only EnabledProjects is swapped
// in — the connection config (cfg) stays as loaded at startup.
func NewRoutingSelector(sqlite mcp.MemoryBackend, cfg memorylake.Config, enab *memorylake.Enablement) mcp.BackendSelector {
	type projEntry struct {
		mu      sync.Mutex        // serializes resolution for THIS project only
		backend mcp.MemoryBackend // cached genuine MemoryLake backend; nil until resolved
	}

	var gmu sync.Mutex
	entries := map[string]*projEntry{}
	enabPath := memorylake.DefaultEnablementPath()
	// Fingerprint of the file backing the enab snapshot we hold. Read/written
	// only under gmu (after construction). enab itself may be an in-memory
	// baseline that never came from the file (missing/unreadable file), which
	// the "" fingerprint represents.
	lastFP := enablementFingerprint(enabPath)

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
		if fp := enablementFingerprint(enabPath); fp != lastFP {
			if fresh, err := loadMemorylakeEnablement(enabPath); err == nil {
				enab.EnabledProjects = fresh.EnabledProjects
				lastFP = fp
			} else {
				// Keep the previous snapshot and leave lastFP stale so the
				// next call retries — e.g. a half-written file during a
				// concurrent `enable` becomes readable a moment later.
				fmt.Fprintf(os.Stderr, "[engram] memorylake: failed to reload enablement list (keeping previous): %v\n", err)
			}
		}
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
			// The enablement list hot-reloads above, so `entry` is current even
			// when the backend is not. Re-derive the one thing on the backend
			// that comes from the entry, so `memorylake conversations
			// enable|disable` in another process takes effect on the very next
			// call instead of at the next process start. A stale flag is not a
			// cosmetic problem: left on, prompts are appended standalone AND
			// inside the merged turn, duplicating every user message in the
			// conversation; left off after a disable, the session's prompts
			// stop reaching MemoryLake with nothing replacing them.
			//
			// Deliberately not discarding and rebuilding the backend — that
			// would cost a workspace resolve plus an actor ensure (two round
			// trips), and this flag is the only part of it derived from the
			// entry.
			syncConversationFlag(pe.backend, entry)
			return pe.backend
		}

		backend, ok := resolveMemoryLakeBackend(project, entry, cfg, enab, enabPath, &gmu, sqlite)
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
func resolveMemoryLakeBackend(project string, entry memorylake.ProjectEntry, cfg memorylake.Config, enab *memorylake.Enablement, enabPath string, gmu *sync.Mutex, sqlite mcp.MemoryBackend) (mcp.MemoryBackend, bool) {
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
		// enabPath was captured at selector construction: re-evaluating
		// DefaultEnablementPath() here would follow a $HOME that changed
		// mid-flight (a test's restored HOME, an env mutation) and backfill
		// into the wrong — possibly the user's real — enablement file.
		saveErr := enab.Save(enabPath)
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
	// With per-turn conversation sync on, the Stop hook's `engram turn` already
	// writes the user's prompt as part of the merged turn message — appending
	// it a second time here would duplicate it in the conversation.
	syncConversationFlag(backend, entry)
	return backend, true
}

// enablementFingerprint returns a cheap change-detection token for the
// enablement file: mtime+size, or "" when the file cannot be stat'ed
// (typically: does not exist yet). Size is included because coarse-mtime
// filesystems can miss a same-second rewrite.
func enablementFingerprint(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d|%d", fi.ModTime().UnixNano(), fi.Size())
}

// syncConversationFlag pushes the entry's per-turn conversation-sync setting
// onto a resolved MemoryLake backend. Split out so the construction path and
// the cache-hit path cannot drift: the flag has to be re-derived on every
// resolve for a toggle to take effect without restarting the process holding
// the backend (see NewRoutingSelector's cache-hit branch).
//
// The type assertion is the honest expression of the situation — the selector
// returns mcp.MemoryBackend, and only the MemoryLake implementation has a
// prompt-append path to suppress. A sqlite backend is a no-op here.
func syncConversationFlag(backend mcp.MemoryBackend, entry memorylake.ProjectEntry) {
	if ml, ok := backend.(*memorylake.MemoryLakeBackend); ok {
		ml.SetSkipPromptAppend(entry.SyncConversations)
	}
}

func warnMemoryLakeFallback(project, step string, err error) {
	fmt.Fprintf(os.Stderr, "[engram] memorylake: %s for project %q failed, falling back to sqlite: %v\n", step, project, err)
}

// buildRoutingSelector loads the MemoryLake config + per-project enablement
// list and returns a BackendSelector wired the same way `engram mcp` (cmdMCP)
// wires it: every project resolves to sqlite unless explicitly enabled.
// Loading failures are non-fatal — an unreadable/missing enablement file just
// means no project is enabled yet (sqlite-only), matching cmdMCP's behavior.
//
// This is the single place `engram save`, `engram search`, and `engram serve`
// (HTTP) assemble their selector from, so all four interfaces (CLI save/
// search, MCP, HTTP) route enabled projects to MemoryLake identically —
// eliminating the split-brain where only `engram mcp` honored enablement.
func buildRoutingSelector(sqlite mcp.MemoryBackend) mcp.BackendSelector {
	mlCfg := loadMemorylakeConfig()
	enab, err := loadMemorylakeEnablement(memorylake.DefaultEnablementPath())
	if err != nil {
		log.Printf("[engram] memorylake: failed to load enablement list (continuing with sqlite-only): %v", err)
		enab = &memorylake.Enablement{EnabledProjects: map[string]memorylake.ProjectEntry{}}
	}
	return NewRoutingSelector(sqlite, mlCfg, enab)
}

// resolveCLIRoutingProject determines which project a CLI save/search call
// should route on: the explicit --project flag when given, otherwise the
// cwd-detected project (mirroring how `engram mcp` resolves an implicit
// project — see resolveReadProject / resolveSaveWriteProject in
// internal/mcp/mcp.go). This is ONLY used to pick a backend; it does not
// change what project value is stored on the observation or used as a search
// filter (those stay exactly as parsed from flags, preserving today's
// behavior byte-for-byte when the resolved project is not MemoryLake-enabled).
func resolveCLIRoutingProject(flagProject string) string {
	if p := strings.TrimSpace(flagProject); p != "" {
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return detectProject(cwd)
}

// backendSearch runs a search against the resolved backend b. When b is the
// local sqlite backend, it goes through the overridable storeSearch hook
// (via mcp.StoreOf to reach the underlying *store.Store — *store.Store no
// longer implements mcp.MemoryBackend directly post sync_id migration, see
// internal/mcp/backend.go) so existing test stubbing (main_extra_test.go)
// keeps working unchanged; any other backend (a genuine MemoryLake backend)
// is called directly through the mcp.MemoryBackend interface.
func backendSearch(b mcp.MemoryBackend, query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	if ss, ok := mcp.StoreOf(b); ok {
		return storeSearch(ss, query, opts)
	}
	return b.Search(query, opts)
}

// backendAddObservation mirrors backendSearch for AddObservation: sqlite goes
// through the overridable storeAddObservation hook (returning the int64 row
// id, formatted as a string so both backends share one return type), and
// everything else (a genuine MemoryLake backend) calls AddObservation
// directly through the interface. A MemoryLake backend's sync_id is a real
// (or pending) MemoryLake identifier — not necessarily numeric (see
// internal/memorylake's AddObservation doc comment) — so callers here must
// treat the returned id as an opaque string, not parse it back to int64.
func backendAddObservation(b mcp.MemoryBackend, p store.AddObservationParams) (string, error) {
	if ss, ok := mcp.StoreOf(b); ok {
		id, err := storeAddObservation(ss, p)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(id, 10), nil
	}
	return b.AddObservation(p)
}
