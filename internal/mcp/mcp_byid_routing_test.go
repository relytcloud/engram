package mcp

// Phase: by-id MCP tools (mem_get_observation, mem_update, mem_delete,
// mem_pin/mem_unpin, mem_timeline, mem_compare) must route through the
// cwd/process-detected project's backend, not the project-unaware default
// (sel("")). Before this fix, every one of these handlers hard-coded
// sel(""), so a MemoryLake-enabled project's observations — which live only
// in that project's MemoryLakeBackend, never in the shared local SQLite
// store — could never be fetched/updated/deleted/pinned by id, even though
// mem_search/mem_save (which DO resolve a project) had just handed the agent
// that very id.
//
// These tests use a two-backend selector (twoBackendSelector) that mirrors
// cmd/engram's NewRoutingSelector contract at the unit level: one named
// project ("mlproj") routes to a MemoryLake-shaped stub backend, every other
// project (including "") routes to sqlite. The stub wraps a real
// *store.Store so its own by-id bookkeeping (ids, deletes, pins) is genuine,
// while remaining a distinct Go type from *store.Store — the same shape a
// real internal/memorylake.MemoryLakeBackend presents to these handlers.
//
// Real *memorylake.MemoryLakeBackend is what makes routing-by-detected-project
// safe against a real cross-project id collision: getFact/patchFact/
// forgetFact (internal/memorylake/backend.go) each scope their request URL to
// that backend's own bound project (.../projects/{b.projID}/...), so an id
// from a different project simply 404s server-side on the wrong backend
// rather than leaking (see memorylake's own tests). At this package's level —
// where the two backends are simply two independent *store.Store instances
// rather than one shared MemoryLake tenant — the equivalent, observable
// guarantee is: an id that only exists in one backend's store is not found
// when routed to the other backend. That is exactly what
// TestByIDHandlers_ForeignIDNotFoundAcrossBackends checks.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

// twoBackendSelector mirrors cmd/engram.NewRoutingSelector's contract for a
// single enabled project: enabledProject routes to ml, every other project
// name (including "") routes to sqlite — exactly like a project absent from
// the MemoryLake enablement list.
func twoBackendSelector(sqlite, ml MemoryBackend, enabledProject string) BackendSelector {
	return func(project string) MemoryBackend {
		if project == enabledProject {
			return ml
		}
		return sqlite
	}
}

// byIDRoutingFixture seeds one observation in each backend so tests can
// assert which backend actually answered a by-id call.
type byIDRoutingFixture struct {
	sqlite       *store.Store
	ml           *memoryLakeStubBackend
	sel          BackendSelector
	sqliteID     int64  // "otherproj" observation, sqlite-only
	mlID         int64  // "mlproj" observation, ml-only
	sqliteSyncID string // sync_id of sqliteID — the key by-id handlers accept post sync_id migration
	mlSyncID     string // sync_id of mlID
}

func newByIDRoutingFixture(t *testing.T) *byIDRoutingFixture {
	t.Helper()
	sqlite := newMCPTestStore(t)
	ml := &memoryLakeStubBackend{Store: newMCPTestStore(t)}
	sel := twoBackendSelector(newSQLiteBackend(sqlite), ml, "mlproj")

	if err := sqlite.CreateSession("s-sqlite", "otherproj", "/work/otherproj"); err != nil {
		t.Fatalf("create sqlite session: %v", err)
	}
	// Seed a second sqlite observation so sqliteID (the one tests reference)
	// is guaranteed not to collide with mlID: both stores are fresh, so their
	// first AddObservation would otherwise both return id 1, which would
	// make the "foreign id is not found" assertions vacuous.
	if _, err := sqlite.AddObservation(store.AddObservationParams{
		SessionID: "s-sqlite", Type: "note", Title: "sqlite filler", Content: "filler", Project: "otherproj",
	}); err != nil {
		t.Fatalf("seed sqlite filler observation: %v", err)
	}
	sqliteID, err := sqlite.AddObservation(store.AddObservationParams{
		SessionID: "s-sqlite", Type: "note", Title: "sqlite-only fact", Content: "lives only in the sqlite backend", Project: "otherproj",
	})
	if err != nil {
		t.Fatalf("seed sqlite observation: %v", err)
	}
	sqliteObs, err := sqlite.GetObservation(sqliteID)
	if err != nil {
		t.Fatalf("reload sqlite observation for sync_id: %v", err)
	}

	if err := ml.Store.CreateSession("s-ml", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("create ml session: %v", err)
	}
	mlID, err := ml.Store.AddObservation(store.AddObservationParams{
		SessionID: "s-ml", Type: "note", Title: "ml-only fact", Content: "lives only in the memorylake backend", Project: "mlproj",
	})
	if err != nil {
		t.Fatalf("seed ml observation: %v", err)
	}
	mlObs, err := ml.Store.GetObservation(mlID)
	if err != nil {
		t.Fatalf("reload ml observation for sync_id: %v", err)
	}

	if sqliteID == mlID {
		t.Fatalf("test fixture invariant violated: sqliteID (%d) must differ from mlID (%d) for cross-backend assertions to be meaningful", sqliteID, mlID)
	}

	return &byIDRoutingFixture{
		sqlite: sqlite, ml: ml, sel: sel,
		sqliteID: sqliteID, mlID: mlID,
		sqliteSyncID: sqliteObs.SyncID, mlSyncID: mlObs.SyncID,
	}
}

// ─── mem_get_observation ────────────────────────────────────────────────────

func TestByIDHandlers_GetObservationRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleGetObservation(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.mlSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleGetObservation error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_get_observation to find the ml-backend id via detected project, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "ml-only fact") {
		t.Fatalf("expected observation content from the ml backend, got %q", text)
	}
}

func TestByIDHandlers_GetObservationForeignIDNotFound(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	// sqliteID only exists in the sqlite backend. Under a detected project of
	// "mlproj" the call is routed to the ml backend, which must not find it —
	// proving detected-project routing cannot accidentally surface another
	// project's data via a numeric id collision or fallback.
	res, err := handleGetObservation(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.sqliteSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleGetObservation error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not-found for a sqlite-only id routed to the ml backend, got success: %s", callResultText(t, res))
	}
}

func TestByIDHandlers_GetObservationSQLiteUnchangedWhenProjectNotEnabled(t *testing.T) {
	f := newByIDRoutingFixture(t)
	// "otherproj" is not the enabled project ("mlproj"), so it must resolve
	// to the exact same sqlite backend sel("") does — SQLite behavior is
	// byte-for-byte unchanged for non-MemoryLake projects.
	cfg := MCPConfig{DefaultProject: "otherproj"}

	res, err := handleGetObservation(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.sqliteSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleGetObservation error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_get_observation to find the sqlite id for a non-enabled project, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "sqlite-only fact") {
		t.Fatalf("expected sqlite observation content, got %q", text)
	}
}

// ─── mem_update ─────────────────────────────────────────────────────────────

func TestByIDHandlers_UpdateRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleUpdate(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id":    f.mlSyncID,
		"title": "ml fact updated",
	}}})
	if err != nil {
		t.Fatalf("handleUpdate error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_update to update the ml-backend observation, got error: %s", callResultText(t, res))
	}

	mlObs, err := f.ml.Store.GetObservation(f.mlID)
	if err != nil {
		t.Fatalf("reload ml observation: %v", err)
	}
	if mlObs.Title != "ml fact updated" {
		t.Fatalf("expected ml backend observation title to be updated, got %q", mlObs.Title)
	}

	sqliteObs, err := f.sqlite.GetObservation(f.sqliteID)
	if err != nil {
		t.Fatalf("reload sqlite observation: %v", err)
	}
	if sqliteObs.Title == "ml fact updated" {
		t.Fatalf("sqlite backend must not have been touched by an mlproj-routed update")
	}
}

func TestByIDHandlers_UpdateForeignIDNotFound(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleUpdate(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id":    f.sqliteSyncID,
		"title": "should not apply",
	}}})
	if err != nil {
		t.Fatalf("handleUpdate error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not-found updating a sqlite-only id via the ml-routed backend, got success: %s", callResultText(t, res))
	}
}

// ─── mem_delete ─────────────────────────────────────────────────────────────

func TestByIDHandlers_DeleteRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleDelete(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.mlSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleDelete error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_delete to soft-delete the ml-backend observation, got error: %s", callResultText(t, res))
	}

	// GetObservation filters out soft-deleted rows, so a successful delete
	// makes the reload fail — that failure IS the proof of deletion.
	if _, err := f.ml.Store.GetObservation(f.mlID); err == nil {
		t.Fatalf("expected ml backend observation to be soft-deleted (reload should now fail)")
	}

	if _, err := f.sqlite.GetObservation(f.sqliteID); err != nil {
		t.Fatalf("sqlite backend observation must not have been deleted by an mlproj-routed delete: %v", err)
	}
}

// ─── mem_pin / mem_unpin ────────────────────────────────────────────────────

func TestByIDHandlers_PinRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handlePin(f.sel, cfg, true)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.mlSyncID,
	}}})
	if err != nil {
		t.Fatalf("handlePin error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_pin to pin the ml-backend observation, got error: %s", callResultText(t, res))
	}

	mlObs, err := f.ml.Store.GetObservation(f.mlID)
	if err != nil {
		t.Fatalf("reload ml observation: %v", err)
	}
	if !mlObs.Pinned {
		t.Fatalf("expected ml backend observation to be pinned")
	}

	sqliteObs, err := f.sqlite.GetObservation(f.sqliteID)
	if err != nil {
		t.Fatalf("reload sqlite observation: %v", err)
	}
	if sqliteObs.Pinned {
		t.Fatalf("sqlite backend observation must not have been pinned by an mlproj-routed pin")
	}
}

// ─── mem_timeline ───────────────────────────────────────────────────────────

func TestByIDHandlers_TimelineRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleTimeline(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"observation_id": f.mlSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleTimeline error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_timeline to find the ml-backend id via detected project, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "ml-only fact") {
		t.Fatalf("expected timeline output to reflect the ml backend observation, got %q", text)
	}
}

func TestByIDHandlers_TimelineExplicitProjectOverrideStillWorks(t *testing.T) {
	f := newByIDRoutingFixture(t)
	// DefaultProject deliberately left empty/wrong; an explicit project=
	// argument must still take precedence, exactly as it did before this fix.
	cfg := MCPConfig{DefaultProject: "otherproj"}

	res, err := handleTimeline(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"observation_id": f.mlSyncID,
		"project":        "mlproj",
	}}})
	if err != nil {
		t.Fatalf("handleTimeline error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected explicit project override to route to the ml backend, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "ml-only fact") {
		t.Fatalf("expected timeline output to reflect the ml backend observation, got %q", text)
	}
}

func TestByIDHandlers_TimelineForeignIDNotFound(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleTimeline(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"observation_id": f.sqliteSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleTimeline error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not-found for a sqlite-only id routed to the ml backend, got success: %s", callResultText(t, res))
	}
}

// ─── mem_compare ────────────────────────────────────────────────────────────

func TestByIDHandlers_CompareRoutesToDetectedMemoryLakeProject(t *testing.T) {
	sqlite := newMCPTestStore(t)
	ml := &memoryLakeStubBackend{Store: newMCPTestStore(t)}
	sel := twoBackendSelector(newSQLiteBackend(sqlite), ml, "mlproj")

	if err := ml.Store.CreateSession("s-ml-compare", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("create ml session: %v", err)
	}
	idA, err := ml.Store.AddObservation(store.AddObservationParams{
		SessionID: "s-ml-compare", Type: "architecture", Title: "ml decision A", Content: "A", Project: "mlproj",
	})
	if err != nil {
		t.Fatalf("seed ml obs A: %v", err)
	}
	idB, err := ml.Store.AddObservation(store.AddObservationParams{
		SessionID: "s-ml-compare", Type: "architecture", Title: "ml decision B", Content: "B", Project: "mlproj",
	})
	if err != nil {
		t.Fatalf("seed ml obs B: %v", err)
	}
	obsA, err := ml.Store.GetObservation(idA)
	if err != nil {
		t.Fatalf("reload ml obs A for sync_id: %v", err)
	}
	obsB, err := ml.Store.GetObservation(idB)
	if err != nil {
		t.Fatalf("reload ml obs B for sync_id: %v", err)
	}

	cfg := MCPConfig{DefaultProject: "mlproj"}
	res, err := handleCompare(sel, cfg, NewSessionActivity(0))(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"memory_id_a": obsA.SyncID,
		"memory_id_b": obsB.SyncID,
		"relation":    "related",
		"confidence":  float64(0.9),
		"reasoning":   "both are ml-backend decisions",
	}}})
	if err != nil {
		t.Fatalf("handleCompare error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_compare to resolve both ml-backend ids via detected project, got error: %s", callResultText(t, res))
	}
}

// ─── mem_review (list + mark_reviewed) ──────────────────────────────────────
//
// mem_review is a spec §5 retained capability. Before Fix #4, handleReview
// resolved its backend via sel("") for mark_reviewed and for a list with no
// explicit project, so both fell to the project-unaware default SQLite
// backend — mem_review was inert for a MemoryLake-enabled project detected
// from cwd. These tests prove list (no explicit project) and mark_reviewed
// now route through the cwd/process-detected project's backend, while SQLite
// projects are unchanged.

// backdateReviewAfter forces obs id in st into needs_review state so
// ObservationsNeedingReview will surface it.
func backdateReviewAfter(t *testing.T, st *store.Store, id int64) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05")
	if _, err := st.DB().Exec(`UPDATE observations SET review_after = ? WHERE id = ?`, past, id); err != nil {
		t.Fatalf("backdate review_after: %v", err)
	}
}

func TestByIDHandlers_ReviewMarkReviewedRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	backdateReviewAfter(t, f.ml.Store, f.mlID)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	res, err := handleReview(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action":         "mark_reviewed",
		"observation_id": f.mlSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleReview mark_reviewed error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_review mark_reviewed to route to the ml backend, got error: %s", callResultText(t, res))
	}

	// The ml observation was backdated into needs_review; after mark_reviewed
	// via the ml-routed backend it must be active again.
	mlObs, err := f.ml.Store.GetObservation(f.mlID)
	if err != nil {
		t.Fatalf("reload ml observation: %v", err)
	}
	if mlObs.State() != store.ObservationStateActive {
		t.Fatalf("expected ml observation to be active after mark_reviewed, got %q", mlObs.State())
	}
}

func TestByIDHandlers_ReviewMarkReviewedForeignIDNotFound(t *testing.T) {
	f := newByIDRoutingFixture(t)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	// sqliteSyncID only exists in the sqlite backend; routed to the ml backend
	// via detected project it must not be found — proving mark_reviewed cannot
	// reach across to another project's data.
	res, err := handleReview(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action":         "mark_reviewed",
		"observation_id": f.sqliteSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleReview mark_reviewed error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not-found marking a sqlite-only id via the ml-routed backend, got success: %s", callResultText(t, res))
	}
}

func TestByIDHandlers_ReviewListRoutesToDetectedMemoryLakeProject(t *testing.T) {
	f := newByIDRoutingFixture(t)
	backdateReviewAfter(t, f.ml.Store, f.mlID)
	// Also backdate the sqlite-only observation so, if the list wrongly fell to
	// the sqlite backend, it would surface "sqlite-only fact" instead — making
	// this a non-vacuous routing assertion.
	backdateReviewAfter(t, f.sqlite, f.sqliteID)
	cfg := MCPConfig{DefaultProject: "mlproj"}

	// No explicit project argument: routing must come from the detected
	// project (mlproj via DefaultProject process override).
	res, err := handleReview(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list",
		"limit":  10.0,
	}}})
	if err != nil {
		t.Fatalf("handleReview list error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_review list to route to the ml backend, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "ml-only fact") {
		t.Fatalf("expected review list to surface the ml backend observation, got %q", text)
	}
	if strings.Contains(text, "sqlite-only fact") {
		t.Fatalf("review list must not surface the sqlite backend's observation when routed to mlproj, got %q", text)
	}
}

func TestByIDHandlers_ReviewListSQLiteUnchangedWhenProjectNotEnabled(t *testing.T) {
	f := newByIDRoutingFixture(t)
	backdateReviewAfter(t, f.sqlite, f.sqliteID)
	// "otherproj" is not the enabled project, so it resolves to the same
	// sqlite backend sel("") does; the no-explicit-project list keeps its
	// cross-project SQLite semantics (query filter stays "") and surfaces the
	// sqlite observation exactly as before Fix #4.
	cfg := MCPConfig{DefaultProject: "otherproj"}

	res, err := handleReview(f.sel, cfg)(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"action": "list",
		"limit":  10.0,
	}}})
	if err != nil {
		t.Fatalf("handleReview list error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_review list to succeed for a non-enabled project, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "sqlite-only fact") {
		t.Fatalf("expected review list to surface the sqlite observation for a non-enabled project, got %q", text)
	}
}

// ─── resolveByIDBackend unit-level guarantees ──────────────────────────────

// TestResolveByIDBackend_NotEnabledProjectMatchesDefaultBackend is the core
// "SQLite unchanged" guarantee: for any project that is not MemoryLake
// enabled, resolveByIDBackend must select the exact same backend instance
// sel("") does.
func TestResolveByIDBackend_NotEnabledProjectMatchesDefaultBackend(t *testing.T) {
	sqlite := newMCPTestStore(t)
	ml := &memoryLakeStubBackend{Store: newMCPTestStore(t)}
	sel := twoBackendSelector(newSQLiteBackend(sqlite), ml, "mlproj")

	cfg := MCPConfig{DefaultProject: "not-enabled-project"}
	backend, detRes, err := resolveByIDBackend(sel, cfg)
	if err != nil {
		t.Fatalf("resolveByIDBackend error: %v", err)
	}
	if detRes.Project != "not-enabled-project" {
		t.Fatalf("detRes.Project=%q, want not-enabled-project", detRes.Project)
	}
	if backend != sel("") {
		t.Fatalf("expected resolveByIDBackend to resolve to the same backend as sel(\"\") for a non-enabled project")
	}
}

// TestResolveByIDBackend_EnabledProjectMatchesRoutedBackend confirms the
// positive counterpart: an enabled project resolves to that project's own
// backend, not sel("").
func TestResolveByIDBackend_EnabledProjectMatchesRoutedBackend(t *testing.T) {
	sqlite := newMCPTestStore(t)
	ml := &memoryLakeStubBackend{Store: newMCPTestStore(t)}
	sel := twoBackendSelector(newSQLiteBackend(sqlite), ml, "mlproj")

	backend, detRes, err := resolveByIDBackend(sel, MCPConfig{DefaultProject: "mlproj"})
	if err != nil {
		t.Fatalf("resolveByIDBackend error: %v", err)
	}
	if detRes.Project != "mlproj" {
		t.Fatalf("detRes.Project=%q, want mlproj", detRes.Project)
	}
	if backend != MemoryBackend(ml) {
		t.Fatalf("expected resolveByIDBackend to resolve to the mlproj backend, not sel(\"\")")
	}
}

// TestResolveByIDBackend_AmbiguousCwdFallsBackWithoutPanicOrEscalatedError
// covers the "detection failure" degrade path: an ambiguous cwd (multiple
// git-repo children, no process override) must not panic and must not
// prevent backend selection — it falls back to the project-unaware default
// backend, and the error is still returned to the caller (by-id handlers
// choose per-handler whether to surface it or silently degrade).
func TestResolveByIDBackend_AmbiguousCwdFallsBackWithoutPanicOrEscalatedError(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"repo-x", "repo-y"} {
		child := filepath.Join(parent, name)
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		initTestGitRepo(t, child)
	}
	t.Chdir(parent)

	sqlite := newMCPTestStore(t)
	ml := &memoryLakeStubBackend{Store: newMCPTestStore(t)}
	sel := twoBackendSelector(newSQLiteBackend(sqlite), ml, "mlproj")

	backend, _, err := resolveByIDBackend(sel, MCPConfig{})
	if err == nil {
		t.Fatal("expected the ambiguous-project detection error to be returned to the caller")
	}
	if backend != sel("") {
		t.Fatalf("expected fallback to the project-unaware default backend on detection failure")
	}
}

// TestByIDHandlers_GetObservationAmbiguousCwdDegradesInsteadOfErroring is the
// end-to-end counterpart: mem_get_observation must still succeed (tolerant
// degrade to plain text, matching its pre-existing behavior) rather than
// hard-failing when cwd detection is ambiguous.
func TestByIDHandlers_GetObservationAmbiguousCwdDegradesInsteadOfErroring(t *testing.T) {
	parent := t.TempDir()
	for _, name := range []string{"repo-x", "repo-y"} {
		child := filepath.Join(parent, name)
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		initTestGitRepo(t, child)
	}
	t.Chdir(parent)

	f := newByIDRoutingFixture(t)

	res, err := handleGetObservation(f.sel, MCPConfig{})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"id": f.sqliteSyncID,
	}}})
	if err != nil {
		t.Fatalf("handleGetObservation error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected degraded success (fallback to default backend) under ambiguous cwd, got error: %s", callResultText(t, res))
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "sqlite-only fact") {
		t.Fatalf("expected sqlite observation content via the fallback backend, got %q", text)
	}
}
