package memorylake

import (
	"path/filepath"
	"testing"
)

func TestSessionIndex_CreateGet_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	idx, err := LoadSessionIndex(path)
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}

	if err := idx.Create("sess-1", "proj", "/tmp/proj", "2026-07-22 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec, ok := idx.Get("sess-1")
	if !ok {
		t.Fatal("Get: expected session to be found")
	}
	if rec.Project != "proj" || rec.Directory != "/tmp/proj" || rec.StartedAt != "2026-07-22 00:00:00" {
		t.Fatalf("rec=%+v, unexpected fields", rec)
	}
	if rec.EndedAt != nil || rec.Summary != nil {
		t.Fatalf("rec=%+v, expected EndedAt/Summary unset for a fresh session", rec)
	}
}

func TestSessionIndex_Get_UnknownIDNotFound(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if _, ok := idx.Get("nope"); ok {
		t.Fatal("Get: expected unknown id to be not-found")
	}
}

// TestSessionIndex_Create_RepeatCallKeepsOriginalStartedAt mirrors
// store.go:createSessionTx's upsert: a second Create for the same id must
// not overwrite started_at, only backfill project/directory when they were
// previously empty.
func TestSessionIndex_Create_RepeatCallKeepsOriginalStartedAt(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("sess-1", "", "", "2026-07-22 00:00:00"); err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	if err := idx.Create("sess-1", "proj", "/dir", "2026-07-22 05:00:00"); err != nil {
		t.Fatalf("Create (second): %v", err)
	}
	rec, ok := idx.Get("sess-1")
	if !ok {
		t.Fatal("expected session to be found")
	}
	if rec.StartedAt != "2026-07-22 00:00:00" {
		t.Fatalf("StartedAt=%q, want original 2026-07-22 00:00:00 (must not be overwritten)", rec.StartedAt)
	}
	if rec.Project != "proj" || rec.Directory != "/dir" {
		t.Fatalf("Project/Directory=%q/%q, want backfilled from second call (were empty)", rec.Project, rec.Directory)
	}
}

func TestSessionIndex_End_MarksEndedWithSummary(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("sess-1", "proj", "/dir", "2026-07-22 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.End("sess-1", "2026-07-22 01:00:00", "did the thing"); err != nil {
		t.Fatalf("End: %v", err)
	}
	rec, ok := idx.Get("sess-1")
	if !ok {
		t.Fatal("expected session to be found")
	}
	if rec.EndedAt == nil || *rec.EndedAt != "2026-07-22 01:00:00" {
		t.Fatalf("EndedAt=%v, want 2026-07-22 01:00:00", rec.EndedAt)
	}
	if rec.Summary == nil || *rec.Summary != "did the thing" {
		t.Fatalf("Summary=%v, want %q", rec.Summary, "did the thing")
	}
}

// TestSessionIndex_End_UnknownIDIsNoOp mirrors store.go:EndSession treating
// an UPDATE affecting zero rows as a no-op, not an error.
func TestSessionIndex_End_UnknownIDIsNoOp(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.End("nope", "2026-07-22 01:00:00", "s"); err != nil {
		t.Fatalf("End(unknown): %v, want nil (no-op)", err)
	}
	if _, ok := idx.Get("nope"); ok {
		t.Fatal("End must not have created a session record for an unknown id")
	}
}

func TestSessionIndex_MostRecentActive_PicksLatestUnended(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("sess-old", "proj", "/dir", "2026-07-20 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.Create("sess-new", "proj", "/dir", "2026-07-21 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.Create("sess-ended", "proj", "/dir", "2026-07-22 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.End("sess-ended", "2026-07-22 01:00:00", ""); err != nil {
		t.Fatalf("End: %v", err)
	}

	id, ok := idx.MostRecentActive("proj")
	if !ok {
		t.Fatal("expected an active session")
	}
	if id != "sess-new" {
		t.Fatalf("MostRecentActive=%q, want sess-new (most recent started_at among un-ended sessions)", id)
	}
}

// TestSessionIndex_MostRecentActive_ExcludesManualSave mirrors
// store.go:MostRecentActiveSession excluding the manual-save fallback ids
// (resolution would otherwise become circular — see its doc comment).
func TestSessionIndex_MostRecentActive_ExcludesManualSave(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("manual-save-proj", "proj", "/dir", "2026-07-22 05:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok := idx.MostRecentActive("proj"); ok {
		t.Fatal("MostRecentActive must exclude manual-save* ids")
	}
}

func TestSessionIndex_Recent_OrdersMostRecentFirstAndRespectsLimit(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	for i, id := range []string{"sess-a", "sess-b", "sess-c"} {
		started := []string{"2026-07-20 00:00:00", "2026-07-21 00:00:00", "2026-07-22 00:00:00"}[i]
		if err := idx.Create(id, "proj", "/dir", started); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	got := idx.Recent("proj", 2)
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2 (limit)", len(got))
	}
	if got[0].ID != "sess-c" || got[1].ID != "sess-b" {
		t.Fatalf("order=%q,%q, want sess-c,sess-b (most recent first)", got[0].ID, got[1].ID)
	}
	if got[0].ObservationCount != 0 {
		t.Fatalf("ObservationCount=%d, want 0 (no fact<->session linkage modeled)", got[0].ObservationCount)
	}
}

func TestSessionIndex_Recent_EmptyProjectMatchesAll(t *testing.T) {
	idx, err := LoadSessionIndex(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("sess-a", "proj-a", "/dir", "2026-07-20 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.Create("sess-b", "proj-b", "/dir", "2026-07-21 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := idx.Recent("", 10)
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2 (unfiltered across all projects)", len(got))
	}
}

// TestSessionIndex_PersistsAcrossReload verifies the sidecar round-trips
// through disk exactly like IDMap/TopicIndex.
func TestSessionIndex_PersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	idx, err := LoadSessionIndex(path)
	if err != nil {
		t.Fatalf("LoadSessionIndex: %v", err)
	}
	if err := idx.Create("sess-1", "proj", "/dir", "2026-07-22 00:00:00"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := idx.End("sess-1", "2026-07-22 01:00:00", "summary text"); err != nil {
		t.Fatalf("End: %v", err)
	}

	reloaded, err := LoadSessionIndex(path)
	if err != nil {
		t.Fatalf("LoadSessionIndex (reload): %v", err)
	}
	rec, ok := reloaded.Get("sess-1")
	if !ok {
		t.Fatal("expected session to survive reload")
	}
	if rec.EndedAt == nil || *rec.EndedAt != "2026-07-22 01:00:00" {
		t.Fatalf("EndedAt after reload=%v, want 2026-07-22 01:00:00", rec.EndedAt)
	}
	if rec.Summary == nil || *rec.Summary != "summary text" {
		t.Fatalf("Summary after reload=%v, want %q", rec.Summary, "summary text")
	}
}
