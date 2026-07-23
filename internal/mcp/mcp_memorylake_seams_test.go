package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
	mcppkg "github.com/mark3labs/mcp-go/mcp"
)

// memoryLakeStubBackend wraps a real *store.Store but is itself a distinct
// type, so `s.(*store.Store)` type-asserts false against it — exactly the
// shape a MemoryLake-enabled project's backend presents to mem_* handlers
// (internal/memorylake.MemoryLakeBackend is not a *store.Store either).
// Delegating everything but the overridden methods to the embedded store
// keeps this stub honest (real session/observation bookkeeping) while
// letting tests observe and control Stats/ProjectExists/AddPromptIfMissing
// specifically — the three seams Task 17 routes through MemoryBackend
// instead of a concrete *store.Store.
type memoryLakeStubBackend struct {
	*store.Store

	statsCalls int
	statsErr   error

	// projectExistsCalls counts every call. projectExistsErr, if set, is only
	// returned once projectExistsCalls exceeds projectExistsErrAfterCall —
	// this lets a test let project resolution's own ProjectExists call (the
	// first one, made by resolveReadProject before mem_doctor's own checks
	// run) succeed, while still forcing a later call (mem_doctor's
	// connectivity check) to fail, without the two uses stepping on each
	// other.
	projectExistsCalls        int
	projectExistsErr          error
	projectExistsErrAfterCall int

	promptCalls  int
	promptParams []store.AddPromptParams

	// skipCandidateGeneration controls SkipsCandidateGeneration()'s return
	// value below (default false, matching sqliteBackend's absence of the
	// method — see candidateOptOut's doc comment in backend.go). Tests that
	// want to exercise the MemoryLake-style skip-candidates path set this to
	// true. findCandidatesCalls counts every FindCandidates invocation so
	// those tests can assert handleSave never reaches it when skipping.
	skipCandidateGeneration bool
	findCandidatesCalls     int
}

var _ MemoryBackend = (*memoryLakeStubBackend)(nil)

// var _ candidateOptOut = (*memoryLakeStubBackend)(nil) documents that this
// stub always implements candidateOptOut (SkipsCandidateGeneration always
// exists as a method below; its return value is merely field-controlled,
// defaulting to false so every pre-existing test using this stub keeps
// behaving exactly as before).
var _ candidateOptOut = (*memoryLakeStubBackend)(nil)

// SkipsCandidateGeneration reports skipCandidateGeneration. Unlike the real
// *memorylake.MemoryLakeBackend (which always returns true), this stub
// defaults to false so it can also double as a MemoryLake-shaped backend that
// does NOT opt out of candidate generation, for tests that don't care about
// this seam.
func (m *memoryLakeStubBackend) SkipsCandidateGeneration() bool {
	return m.skipCandidateGeneration
}

func (m *memoryLakeStubBackend) Stats() (*store.Stats, error) {
	m.statsCalls++
	if m.statsErr != nil {
		return nil, m.statsErr
	}
	return m.Store.Stats()
}

func (m *memoryLakeStubBackend) ProjectExists(name string) (bool, error) {
	m.projectExistsCalls++
	if m.projectExistsErr != nil && m.projectExistsCalls > m.projectExistsErrAfterCall {
		return false, m.projectExistsErr
	}
	return m.Store.ProjectExists(name)
}

// memoryLakeSyncBackend lazily wraps the stub's embedded *store.Store with
// the same sqliteBackend adapter production code uses, so the by-id and
// AddObservation/AddPrompt* methods below can delegate to it instead of
// duplicating the sync_id<->int64 translation logic (see sqlite_backend.go).
func (m *memoryLakeStubBackend) memoryLakeSyncBackend() *sqliteBackend {
	return newSQLiteBackend(m.Store)
}

func (m *memoryLakeStubBackend) AddObservation(p store.AddObservationParams) (string, error) {
	return m.memoryLakeSyncBackend().AddObservation(p)
}

func (m *memoryLakeStubBackend) GetObservation(syncID string) (*store.Observation, error) {
	return m.memoryLakeSyncBackend().GetObservation(syncID)
}

func (m *memoryLakeStubBackend) UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error) {
	return m.memoryLakeSyncBackend().UpdateObservation(syncID, p)
}

func (m *memoryLakeStubBackend) DeleteObservation(syncID string, hardDelete bool) error {
	return m.memoryLakeSyncBackend().DeleteObservation(syncID, hardDelete)
}

func (m *memoryLakeStubBackend) PinObservation(syncID string) error {
	return m.memoryLakeSyncBackend().PinObservation(syncID)
}

func (m *memoryLakeStubBackend) UnpinObservation(syncID string) error {
	return m.memoryLakeSyncBackend().UnpinObservation(syncID)
}

func (m *memoryLakeStubBackend) Timeline(syncID string, before, after int) (*store.TimelineResult, error) {
	return m.memoryLakeSyncBackend().Timeline(syncID, before, after)
}

func (m *memoryLakeStubBackend) MarkReviewed(syncID string) error {
	return m.memoryLakeSyncBackend().MarkReviewed(syncID)
}

func (m *memoryLakeStubBackend) FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error) {
	m.findCandidatesCalls++
	return m.memoryLakeSyncBackend().FindCandidates(savedSyncID, opts)
}

func (m *memoryLakeStubBackend) AddPrompt(p store.AddPromptParams) (string, error) {
	return m.memoryLakeSyncBackend().AddPrompt(p)
}

func (m *memoryLakeStubBackend) AddPromptIfMissing(p store.AddPromptParams) (string, bool, error) {
	m.promptCalls++
	m.promptParams = append(m.promptParams, p)
	return m.memoryLakeSyncBackend().AddPromptIfMissing(p)
}

func newMemoryLakeStubBackend(t *testing.T) *memoryLakeStubBackend {
	t.Helper()
	return &memoryLakeStubBackend{Store: newMCPTestStore(t)}
}

// TestHandleStatsMemoryLakeBackendReturnsStatsInsteadOfHardError is the
// RED->GREEN case for the mem_stats half of Task 17: prior to the fix,
// handleStats type-asserted the resolved backend down to *store.Store and
// returned "mem_stats requires a local SQLite-backed project" for any
// backend (like this stub) that isn't literally *store.Store. After the
// fix, loadMCPStats is called through the MemoryBackend interface so a
// MemoryLake-shaped backend's own Stats() is used instead.
func TestHandleStatsMemoryLakeBackendReturnsStatsInsteadOfHardError(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	if err := stub.Store.CreateSession("s-ml-stats", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := stub.Store.AddObservation(store.AddObservationParams{
		SessionID: "s-ml-stats", Type: "note", Title: "ml stat", Content: "ml stat content", Project: "mlproj",
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	res, err := handleStats(StaticSelector(stub), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_stats to succeed for a non-*store.Store MemoryBackend, got error: %s", callResultText(t, res))
	}
	if stub.statsCalls != 1 {
		t.Fatalf("expected Stats() to be called exactly once through the MemoryBackend interface, got %d", stub.statsCalls)
	}
	text := callResultText(t, res)
	if !strings.Contains(text, "Observations: 1") {
		t.Fatalf("expected stats output to reflect the seeded observation, got %q", text)
	}
}

// TestHandleStatsLoaderFailureStillErrorsForMemoryLakeBackend confirms the
// loadMCPStats seam's failure-injection path (already covered for SQLite by
// TestHandleStatsReturnsErrorWhenLoaderFails) still surfaces a tool error
// when the underlying backend itself fails, now that loadMCPStats is typed
// as MemoryBackend rather than *store.Store.
func TestHandleStatsLoaderFailureStillErrorsForMemoryLakeBackend(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	stub.statsErr = errors.New("memorylake stats unavailable")

	res, err := handleStats(StaticSelector(stub), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleStats: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected tool error when the MemoryLake-shaped backend's Stats() fails")
	}
}

// TestHandleSaveCapturesPromptViaMemoryLakeBackend is the RED->GREEN case
// for the automatic prompt capture half of Task 17: prior to the fix,
// handleSave only invoked addPromptIfMissing after a `s.(*store.Store)` type
// assertion succeeded, so a MemoryLake-shaped backend silently skipped
// automatic prompt capture. After the fix, addPromptIfMissing is called
// directly on the MemoryBackend interface value.
func TestHandleSaveCapturesPromptViaMemoryLakeBackend(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	activity := NewSessionActivity(10 * time.Minute)
	sessionID := defaultSessionID("engram")
	activity.RecordPrompt(sessionID, "engram", "prompt that should be auto-captured for MemoryLake")

	h := handleSave(StaticSelector(stub), MCPConfig{}, activity)
	res, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title":   "MemoryLake prompt capture",
		"content": "**What**: saved via MemoryLake-shaped backend\n**Why**: regression test",
		"type":    "bugfix",
		"project": "engram",
	}}})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected save error: %s", callResultText(t, res))
	}

	if stub.promptCalls != 1 {
		t.Fatalf("expected AddPromptIfMissing to be called exactly once through the MemoryBackend interface, got %d", stub.promptCalls)
	}

	prompts, err := stub.Store.RecentPrompts("engram", 5)
	if err != nil {
		t.Fatalf("RecentPrompts: %v", err)
	}
	found := false
	for _, p := range prompts {
		if p.Content == "prompt that should be auto-captured for MemoryLake" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the recorded prompt to be persisted, got prompts=%#v", prompts)
	}
}

// TestHandleDoctorMemoryLakeBackendDegradesInsteadOfHardErroring is the
// RED->GREEN case for the mem_doctor half of Task 17: prior to the fix,
// handleDoctor type-asserted down to *store.Store and returned "mem_doctor
// requires a local SQLite-backed project" for any other backend. After the
// fix, a non-*store.Store backend gets the lightweight MemoryLake
// connectivity/stats suite and a normal ok envelope instead.
func TestHandleDoctorMemoryLakeBackendDegradesInsteadOfHardErroring(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	if err := stub.Store.CreateSession("manual-save-mlproj", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := handleDoctor(StaticSelector(stub), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"project": "mlproj",
	}}})
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected mem_doctor to degrade gracefully for a MemoryLake-shaped backend, got error: %s", callResultText(t, res))
	}

	envelope := callResultJSON(t, res)
	if envelope["status"] != "ok" {
		t.Fatalf("envelope=%v", envelope)
	}
	checks, ok := envelope["checks"].([]any)
	if !ok || len(checks) != 2 {
		t.Fatalf("expected exactly 2 MemoryLake-lite checks, got %v", envelope["checks"])
	}
	seen := map[string]bool{}
	for _, c := range checks {
		m := c.(map[string]any)
		seen[m["check_id"].(string)] = true
	}
	if !seen[CheckMemoryLakeConnectivity] || !seen[CheckMemoryLakeStats] {
		t.Fatalf("expected connectivity and stats checks, got %v", checks)
	}
	// ProjectExists is called twice: once by resolveReadProject to validate
	// the explicit "mlproj" override, once by mem_doctor's own connectivity
	// check.
	if stub.projectExistsCalls != 2 {
		t.Fatalf("expected ProjectExists to be called twice (resolution + connectivity check), got %d", stub.projectExistsCalls)
	}
	if stub.statsCalls != 1 {
		t.Fatalf("expected Stats to be called once for the stats check, got %d", stub.statsCalls)
	}
}

// TestHandleDoctorMemoryLakeBackendFailSafeOnBackendError confirms a
// MemoryLake backend error surfaces as a StatusError check (and IsError on
// the tool result) instead of panicking or silently succeeding. The project
// override must resolve successfully first (projectExistsErrAfterCall: 1
// lets that first ProjectExists call through) so this test exercises the
// doctor checks' own fail-safe handling, not project resolution's.
func TestHandleDoctorMemoryLakeBackendFailSafeOnBackendError(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	if err := stub.Store.CreateSession("manual-save-mlproj", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	stub.projectExistsErr = errors.New("memorylake unreachable")
	stub.projectExistsErrAfterCall = 1
	stub.statsErr = errors.New("memorylake unreachable")

	res, err := handleDoctor(StaticSelector(stub), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"project": "mlproj",
	}}})
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError when the MemoryLake backend fails both checks")
	}
	envelope := callResultJSON(t, res)
	if envelope["status"] != "error" {
		t.Fatalf("envelope=%v", envelope)
	}
}

// TestHandleDoctorMemoryLakeBackendUnknownCheckIsNotApplicableNotHardError
// confirms an explicit --check for a SQLite-only diagnostic (or unknown id)
// against a MemoryLake-shaped backend yields a clear, non-error
// "not applicable" result rather than a hard error.
func TestHandleDoctorMemoryLakeBackendUnknownCheckIsNotApplicableNotHardError(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	if err := stub.Store.CreateSession("manual-save-mlproj", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	res, err := handleDoctor(StaticSelector(stub), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"project": "mlproj",
		"check":   "sqlite_lock_contention",
	}}})
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected a not-applicable result, not a hard error: %s", callResultText(t, res))
	}
	envelope := callResultJSON(t, res)
	if envelope["status"] != "ok" {
		t.Fatalf("envelope=%v", envelope)
	}
	checks := envelope["checks"].([]any)
	if len(checks) != 1 {
		t.Fatalf("checks=%v", checks)
	}
	first := checks[0].(map[string]any)
	if first["reason_code"] != "check_not_applicable_memorylake" {
		t.Fatalf("expected check_not_applicable_memorylake reason code, got %v", first)
	}
}

// TestHandleSaveSkipsCandidateGenerationForCandidateOptOutBackend is the
// RED->GREEN case for the candidateOptOut seam (backend.go): mem_save must
// skip FindCandidates entirely — and omit judgment_required/candidates from
// the response envelope entirely, not merely report them false/empty — for
// any backend whose SkipsCandidateGeneration() returns true. This is the
// shape *memorylake.MemoryLakeBackend presents (see conflict.go), simulated
// here via memoryLakeStubBackend with skipCandidateGeneration set.
//
// Observation B below deliberately overlaps observation A's title/content
// closely enough that, on the SQLite path, it always produces at least one
// FindCandidates hit (this is the same fixture TestConflictLoop_SaveJudgeSearch
// uses to prove the opposite, SQLite-does-generate-candidates behavior) — so
// a passing test here is not vacuous: it proves the skip actively suppressed
// a real would-be candidate, not merely that none happened to exist.
func TestHandleSaveSkipsCandidateGenerationForCandidateOptOutBackend(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	stub.skipCandidateGeneration = true
	if err := stub.Store.CreateSession("s-ml-optout", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	activity := NewSessionActivity(10 * time.Minute)
	h := handleSave(StaticSelector(stub), MCPConfig{}, activity)

	resA, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title":   "We use sessions for authentication middleware",
		"content": "Session-based auth in the middleware layer keeps state server-side",
		"type":    "architecture",
		"project": "mlproj",
	}}})
	if err != nil {
		t.Fatalf("save A: %v", err)
	}
	if resA.IsError {
		t.Fatalf("save A unexpected error: %s", callResultText(t, resA))
	}

	resB, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title":   "Switched from sessions to JWT authentication",
		"content": "JWT tokens replace session-based auth for better scalability across services",
		"type":    "architecture",
		"project": "mlproj",
	}}})
	if err != nil {
		t.Fatalf("save B: %v", err)
	}
	if resB.IsError {
		t.Fatalf("save B unexpected error: %s", callResultText(t, resB))
	}

	if stub.findCandidatesCalls != 0 {
		t.Fatalf("expected FindCandidates to never be called for a candidateOptOut backend, got %d calls", stub.findCandidatesCalls)
	}

	envB := parseEnvelope(t, "candidateOptOut save B", resB)
	if _, present := envB["judgment_required"]; present {
		t.Fatalf("expected envelope to omit judgment_required entirely for a candidateOptOut backend, got %v", envB["judgment_required"])
	}
	if _, present := envB["candidates"]; present {
		t.Fatalf("expected envelope to omit candidates entirely for a candidateOptOut backend, got %v", envB["candidates"])
	}
}

// TestHandleSaveGeneratesCandidatesWhenBackendDoesNotOptOut is the contrast
// case for the test above: memoryLakeStubBackend without
// skipCandidateGeneration set (the zero value, matching sqliteBackend's
// absence of the candidateOptOut method) must still drive handleSave through
// the normal FindCandidates path — proving the opt-out is what suppresses
// candidate generation above, not something specific to the stub type itself.
func TestHandleSaveGeneratesCandidatesWhenBackendDoesNotOptOut(t *testing.T) {
	stub := newMemoryLakeStubBackend(t)
	if err := stub.Store.CreateSession("s-ml-noopt", "mlproj", "/work/mlproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	activity := NewSessionActivity(10 * time.Minute)
	h := handleSave(StaticSelector(stub), MCPConfig{}, activity)

	if _, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title":   "We use sessions for authentication middleware",
		"content": "Session-based auth in the middleware layer keeps state server-side",
		"type":    "architecture",
		"project": "mlproj",
	}}}); err != nil {
		t.Fatalf("save A: %v", err)
	}

	resB, err := h(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{
		"title":   "Switched from sessions to JWT authentication",
		"content": "JWT tokens replace session-based auth for better scalability across services",
		"type":    "architecture",
		"project": "mlproj",
	}}})
	if err != nil {
		t.Fatalf("save B: %v", err)
	}

	if stub.findCandidatesCalls == 0 {
		t.Fatal("expected FindCandidates to be called for a backend that does not opt out")
	}
	envB := parseEnvelope(t, "non-opt-out save B", resB)
	if jr, _ := envB["judgment_required"].(bool); !jr {
		t.Fatalf("expected judgment_required=true when the backend does not opt out, envelope=%v", envB)
	}
}

// TestMemDoctorSQLiteBehaviorUnchangedByMemoryLakeSeam is a regression guard:
// a plain *store.Store project must keep running the full diagnostic.Runner
// suite (not the MemoryLake-lite substitute) exactly as before Task 17.
func TestMemDoctorSQLiteBehaviorUnchangedByMemoryLakeSeam(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("manual-save-engram", "engram", "/work/engram"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	res, err := handleDoctor(StaticSelector(newSQLiteBackend(s)), MCPConfig{})(context.Background(), mcppkg.CallToolRequest{Params: mcppkg.CallToolParams{Arguments: map[string]any{"project": "engram", "check": "manual_session_name_project_mismatch"}}})
	if err != nil {
		t.Fatalf("handleDoctor: %v", err)
	}
	envelope := callResultJSON(t, res)
	if envelope["status"] != "ok" || envelope["project"] != "engram" {
		t.Fatalf("envelope=%v", envelope)
	}
	checks := envelope["checks"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["check_id"] != "manual_session_name_project_mismatch" {
		t.Fatalf("checks=%v", checks)
	}
}
