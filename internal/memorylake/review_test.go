package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBackend_ObservationsNeedingReview_TypeDecayDeterminesOverdue exercises
// the per-type decay table (mirroring internal/store's
// decayReviewAfterMonths, store.go:250): a "decision" fact created 7 months
// ago (decay=6mo) is overdue, a "preference" fact created 1 month ago
// (decay=3mo) is not yet due, and a "note" fact (absent from the decay
// table) never needs review regardless of age.
func TestBackend_ObservationsNeedingReview_TypeDecayDeterminesOverdue(t *testing.T) {
	now := time.Now().UTC()
	sevenMonthsAgo := now.AddDate(0, -7, 0).Format(time.RFC3339)
	oneMonthAgo := now.AddDate(0, -1, 0).Format(time.RFC3339)
	oldNote := now.AddDate(-2, 0, 0).Format(time.RFC3339)

	items := []map[string]any{
		{"id": "fact-decision", "fact": "d", "created_at": sevenMonthsAgo,
			"metadata": map[string]any{metaRaw: "overdue decision", metaTitle: "D", metaType: "decision", metaScope: "global"}},
		{"id": "fact-preference", "fact": "p", "created_at": oneMonthAgo,
			"metadata": map[string]any{metaRaw: "fresh preference", metaTitle: "P", metaType: "preference", metaScope: "global"}},
		{"id": "fact-note", "fact": "n", "created_at": oldNote,
			"metadata": map[string]any{metaRaw: "ancient note", metaTitle: "N", metaType: "note", metaScope: "global"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	got, err := b.ObservationsNeedingReview("proj", 10)
	if err != nil {
		t.Fatalf("ObservationsNeedingReview: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want exactly 1 (only the overdue decision): %+v", len(got), got)
	}
	if got[0].Content != "overdue decision" {
		t.Fatalf("got[0].Content=%q, want %q", got[0].Content, "overdue decision")
	}
}

// TestBackend_ObservationsNeedingReview_ExcludesExpired mirrors the local
// store's deleted_at IS NULL exclusion (store.go:2508): an expired/forgotten
// fact must never appear even if its decay says it's overdue.
func TestBackend_ObservationsNeedingReview_ExcludesExpired(t *testing.T) {
	longAgo := time.Now().UTC().AddDate(-1, 0, 0).Format(time.RFC3339)
	items := []map[string]any{
		{"id": "fact-1", "fact": "f", "created_at": longAgo, "expired": true,
			"metadata": map[string]any{metaRaw: "forgotten decision", metaType: "decision"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	got, err := b.ObservationsNeedingReview("proj", 10)
	if err != nil {
		t.Fatalf("ObservationsNeedingReview: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got=%+v, want empty (expired fact excluded)", got)
	}
}

// TestBackend_ObservationsNeedingReview_OrdersSoonestDueFirstAndRespectsLimit
// mirrors store.go:2498's `ORDER BY review_after ASC ... LIMIT ?`.
func TestBackend_ObservationsNeedingReview_OrdersSoonestDueFirstAndRespectsLimit(t *testing.T) {
	now := time.Now().UTC()
	items := []map[string]any{
		{"id": "fact-a", "fact": "a", "created_at": now.AddDate(0, -8, 0).Format(time.RFC3339),
			"metadata": map[string]any{metaRaw: "A", metaType: "decision"}}, // overdue by 2mo
		{"id": "fact-b", "fact": "b", "created_at": now.AddDate(0, -13, 0).Format(time.RFC3339),
			"metadata": map[string]any{metaRaw: "B", metaType: "policy"}}, // overdue by 1mo
		{"id": "fact-c", "fact": "c", "created_at": now.AddDate(0, -20, 0).Format(time.RFC3339),
			"metadata": map[string]any{metaRaw: "C", metaType: "policy"}}, // overdue by 8mo (most overdue)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts") {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"items": items}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	got, err := b.ObservationsNeedingReview("proj", 2)
	if err != nil {
		t.Fatalf("ObservationsNeedingReview: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2 (limit)", len(got))
	}
	// fact-c's review_after (created + 12mo) is earliest/most overdue, then fact-a.
	if got[0].Content != "C" || got[1].Content != "A" {
		t.Fatalf("order=%q,%q, want C,A (soonest review_after first)", got[0].Content, got[1].Content)
	}
}

// TestBackend_MarkReviewed_ResetsDecayClock verifies MarkReviewed stamps an
// engram_review_after override far enough in the future that a previously
// overdue fact no longer shows up in ObservationsNeedingReview.
func TestBackend_MarkReviewed_ResetsDecayClock(t *testing.T) {
	factPath := "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1"
	overdueCreated := time.Now().UTC().AddDate(0, -7, 0).Format(time.RFC3339)
	var patchedMD map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == factPath:
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "d", "created_at": overdueCreated,
				"metadata": map[string]any{metaRaw: "overdue decision", metaType: "decision"},
			}})
		case r.Method == "PATCH" && r.URL.Path == factPath:
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			patchedMD = body.Metadata
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "d", "created_at": overdueCreated, "metadata": body.Metadata,
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)
	id := b.idmap.IntFor(b.projID, "fact-1")

	if err := b.MarkReviewed(id); err != nil {
		t.Fatalf("MarkReviewed: %v", err)
	}
	raw, _ := patchedMD[metaReviewAfter].(string)
	if raw == "" {
		t.Fatal("expected engram_review_after to be stamped in the PATCH metadata")
	}
	parsed, err := parseFactTime(raw)
	if err != nil {
		t.Fatalf("parseFactTime(%q): %v", raw, err)
	}
	if !parsed.After(time.Now().UTC()) {
		t.Fatalf("engram_review_after=%v, want a future instant", parsed)
	}
	if patchedMD[metaRaw] != "overdue decision" {
		t.Fatalf("MarkReviewed dropped preserved metadata: %v", patchedMD)
	}
}

// TestBackend_MarkReviewed_UnknownIDErrors verifies the NOT_FOUND contract
// for an id with no fact mapping (mirrors GetObservation/PinObservation).
func TestBackend_MarkReviewed_UnknownIDErrors(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	if err := b.MarkReviewed(999); err == nil {
		t.Fatal("expected an error for an unmapped id")
	}
}
