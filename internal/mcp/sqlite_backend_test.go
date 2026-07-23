package mcp

import (
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TestSQLiteBackend_AddObservationReturnsNonEmptySyncID covers the adapter's
// AddObservation: it must delegate to *store.Store.AddObservation and return
// the resulting row's sync_id (a non-empty string), not the int64 primary
// key — that translation is exactly what makes sqliteBackend satisfy
// MemoryBackend's by-id-via-sync_id contract (see backend.go).
func TestSQLiteBackend_AddObservationReturnsNonEmptySyncID(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-adapter", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	b := newSQLiteBackend(s)

	syncID, err := b.AddObservation(store.AddObservationParams{
		SessionID: "s-adapter",
		Type:      "decision",
		Title:     "Adapter test",
		Content:   "sqliteBackend.AddObservation must return a sync_id",
		Project:   "engram",
		Scope:     "project",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if syncID == "" {
		t.Fatal("expected AddObservation to return a non-empty sync_id")
	}

	// Confirm it's a real sync_id (not a stringified int64 primary key) by
	// cross-checking against the store's own row.
	obs, err := s.GetObservationBySyncID(syncID)
	if err != nil {
		t.Fatalf("GetObservationBySyncID(%q): %v", syncID, err)
	}
	if obs.Title != "Adapter test" {
		t.Fatalf("expected the sync_id to resolve back to the saved row, got title=%q", obs.Title)
	}
}

// TestSQLiteBackend_GetObservationBySyncIDRoundTrips covers the read half:
// GetObservation(syncID) must return the exact row AddObservation just
// created, keyed purely by the sync_id string handed back.
func TestSQLiteBackend_GetObservationBySyncIDRoundTrips(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-adapter-get", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	b := newSQLiteBackend(s)

	syncID, err := b.AddObservation(store.AddObservationParams{
		SessionID: "s-adapter-get",
		Type:      "note",
		Title:     "Round trip",
		Content:   "content",
		Project:   "engram",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	obs, err := b.GetObservation(syncID)
	if err != nil {
		t.Fatalf("GetObservation(%q): %v", syncID, err)
	}
	if obs.SyncID != syncID {
		t.Fatalf("obs.SyncID=%q, want %q", obs.SyncID, syncID)
	}
	if obs.Title != "Round trip" {
		t.Fatalf("obs.Title=%q, want %q", obs.Title, "Round trip")
	}
}

// TestSQLiteBackend_DeleteObservationBySyncIDTakesEffect confirms
// DeleteObservation(syncID) reaches the same row a subsequent GetObservation
// call can no longer see (soft delete filters it out).
func TestSQLiteBackend_DeleteObservationBySyncIDTakesEffect(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-adapter-delete", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	b := newSQLiteBackend(s)

	syncID, err := b.AddObservation(store.AddObservationParams{
		SessionID: "s-adapter-delete",
		Type:      "note",
		Title:     "To delete",
		Content:   "content",
		Project:   "engram",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if err := b.DeleteObservation(syncID, false); err != nil {
		t.Fatalf("DeleteObservation(%q): %v", syncID, err)
	}

	if _, err := b.GetObservation(syncID); err == nil {
		t.Fatal("expected GetObservation to fail for a soft-deleted sync_id")
	}
}

// TestSQLiteBackend_PinObservationBySyncIDTakesEffect confirms
// PinObservation(syncID) sets the pinned flag visible through a subsequent
// GetObservation(syncID).
func TestSQLiteBackend_PinObservationBySyncIDTakesEffect(t *testing.T) {
	s := newMCPTestStore(t)
	if err := s.CreateSession("s-adapter-pin", "engram", "/tmp/engram"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	b := newSQLiteBackend(s)

	syncID, err := b.AddObservation(store.AddObservationParams{
		SessionID: "s-adapter-pin",
		Type:      "note",
		Title:     "To pin",
		Content:   "content",
		Project:   "engram",
	})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	if err := b.PinObservation(syncID); err != nil {
		t.Fatalf("PinObservation(%q): %v", syncID, err)
	}

	obs, err := b.GetObservation(syncID)
	if err != nil {
		t.Fatalf("GetObservation(%q): %v", syncID, err)
	}
	if !obs.Pinned {
		t.Fatal("expected observation to be pinned")
	}

	if err := b.UnpinObservation(syncID); err != nil {
		t.Fatalf("UnpinObservation(%q): %v", syncID, err)
	}
	obs, err = b.GetObservation(syncID)
	if err != nil {
		t.Fatalf("GetObservation(%q) after unpin: %v", syncID, err)
	}
	if obs.Pinned {
		t.Fatal("expected observation to be unpinned")
	}
}

// TestSQLiteBackend_UnknownSyncIDIsNotFound is the negative case shared by
// every by-id method: a sync_id that was never assigned to any row must
// surface as a not-found error, never a panic or a silent zero value.
func TestSQLiteBackend_UnknownSyncIDIsNotFound(t *testing.T) {
	s := newMCPTestStore(t)
	b := newSQLiteBackend(s)

	const unknown = "obs-never-existed"

	if _, err := b.GetObservation(unknown); err == nil {
		t.Error("expected GetObservation to fail for an unknown sync_id")
	}
	if _, err := b.UpdateObservation(unknown, store.UpdateObservationParams{}); err == nil {
		t.Error("expected UpdateObservation to fail for an unknown sync_id")
	}
	if err := b.DeleteObservation(unknown, false); err == nil {
		t.Error("expected DeleteObservation to fail for an unknown sync_id")
	}
	if err := b.PinObservation(unknown); err == nil {
		t.Error("expected PinObservation to fail for an unknown sync_id")
	}
	if err := b.UnpinObservation(unknown); err == nil {
		t.Error("expected UnpinObservation to fail for an unknown sync_id")
	}
	if err := b.MarkReviewed(unknown); err == nil {
		t.Error("expected MarkReviewed to fail for an unknown sync_id")
	}
	if _, err := b.Timeline(unknown, 5, 5); err == nil {
		t.Error("expected Timeline to fail for an unknown sync_id")
	}
	if _, err := b.FindCandidates(unknown, store.CandidateOptions{}); err == nil {
		t.Error("expected FindCandidates to fail for an unknown sync_id")
	}
}

// TestSQLiteBackend_ImplementsMemoryBackend is a compile-time-only assertion
// duplicated here (also present in backend_test.go) so this file is a
// self-contained record of the adapter's contract.
func TestSQLiteBackend_ImplementsMemoryBackend(t *testing.T) {
	var _ MemoryBackend = (*sqliteBackend)(nil)
}
