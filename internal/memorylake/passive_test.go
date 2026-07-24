package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// passiveCaptureServer is a minimal AddObservation mock for the direct-write
// path: PassiveCapture's per-learning save (via AddObservation) posts each
// learning verbatim to the direct fact-add endpoint and returns the fact id.
// The returned counter tracks how many facts were posted.
func passiveCaptureServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var appendCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts" {
			atomic.AddInt32(&appendCount, 1)
			var body struct {
				Facts []string `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			text := ""
			if len(body.Facts) > 0 {
				text = body.Facts[0]
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{{"id": "fact-" + contentHash(text), "fact": text}},
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	return srv, &appendCount
}

const passiveCaptureContent = `Fixed the login bug.

## Key Learnings

1. Always validate tokens before checking expiry, not after.
2. Session cookies must be marked HttpOnly in production.
`

// TestBackend_PassiveCapture_ExtractsAndSaves verifies the parse-and-save
// path: two numbered learnings extracted, both saved as new facts.
func TestBackend_PassiveCapture_ExtractsAndSaves(t *testing.T) {
	srv, appendCount := passiveCaptureServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	result, err := b.PassiveCapture(store.PassiveCaptureParams{
		SessionID: "sess-1", Project: "proj", Content: passiveCaptureContent, Source: "session-end",
	})
	if err != nil {
		t.Fatalf("PassiveCapture: %v", err)
	}
	if result.Extracted != 2 {
		t.Fatalf("Extracted=%d, want 2", result.Extracted)
	}
	if result.Saved != 2 {
		t.Fatalf("Saved=%d, want 2", result.Saved)
	}
	if result.Duplicates != 0 {
		t.Fatalf("Duplicates=%d, want 0 (first capture)", result.Duplicates)
	}
	if got := atomic.LoadInt32(appendCount); got != 2 {
		t.Fatalf("append count=%d, want 2 (one message per learning)", got)
	}
}

// TestBackend_PassiveCapture_DuplicatesAreSkipped is the counting-with-dups
// regression the brief asks for: capturing the exact same content twice must
// report the second pass as all duplicates and must not re-append messages
// for learnings already saved.
func TestBackend_PassiveCapture_DuplicatesAreSkipped(t *testing.T) {
	srv, appendCount := passiveCaptureServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	first, err := b.PassiveCapture(store.PassiveCaptureParams{
		SessionID: "sess-1", Project: "proj", Content: passiveCaptureContent,
	})
	if err != nil {
		t.Fatalf("PassiveCapture (first): %v", err)
	}
	if first.Saved != 2 || first.Duplicates != 0 {
		t.Fatalf("first=%+v, want Saved=2 Duplicates=0", first)
	}

	second, err := b.PassiveCapture(store.PassiveCaptureParams{
		SessionID: "sess-1", Project: "proj", Content: passiveCaptureContent,
	})
	if err != nil {
		t.Fatalf("PassiveCapture (second): %v", err)
	}
	if second.Extracted != 2 {
		t.Fatalf("second.Extracted=%d, want 2 (still parsed, just deduped after)", second.Extracted)
	}
	if second.Saved != 0 {
		t.Fatalf("second.Saved=%d, want 0 (all duplicates)", second.Saved)
	}
	if second.Duplicates != 2 {
		t.Fatalf("second.Duplicates=%d, want 2", second.Duplicates)
	}
	if got := atomic.LoadInt32(appendCount); got != 2 {
		t.Fatalf("append count after both passes=%d, want still 2 (no re-append for duplicates)", got)
	}
}

// TestBackend_PassiveCapture_NoLearningsSection reports Extracted=0 and does
// not touch MemoryLake at all.
func TestBackend_PassiveCapture_NoLearningsSection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s (no learnings to save)", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	result, err := b.PassiveCapture(store.PassiveCaptureParams{SessionID: "sess-1", Project: "proj", Content: "just chatting, nothing structured here"})
	if err != nil {
		t.Fatalf("PassiveCapture: %v", err)
	}
	if result.Extracted != 0 || result.Saved != 0 || result.Duplicates != 0 {
		t.Fatalf("result=%+v, want all zero", result)
	}
}

// TestBackend_PassiveCapture_DifferentProjectsDoNotDedupeAgainstEachOther
// verifies the dedup key is scoped per project (mirroring store's
// `ifnull(project,”) = ?` match), not global.
func TestBackend_PassiveCapture_DifferentProjectsDoNotDedupeAgainstEachOther(t *testing.T) {
	srv, appendCount := passiveCaptureServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.PassiveCapture(store.PassiveCaptureParams{Project: "proj-a", Content: passiveCaptureContent}); err != nil {
		t.Fatalf("PassiveCapture (proj-a): %v", err)
	}
	second, err := b.PassiveCapture(store.PassiveCaptureParams{Project: "proj-b", Content: passiveCaptureContent})
	if err != nil {
		t.Fatalf("PassiveCapture (proj-b): %v", err)
	}
	if second.Saved != 2 || second.Duplicates != 0 {
		t.Fatalf("proj-b result=%+v, want Saved=2 Duplicates=0 (different project, not a duplicate)", second)
	}
	if got := atomic.LoadInt32(appendCount); got != 4 {
		t.Fatalf("append count=%d, want 4 (2 learnings x 2 projects)", got)
	}
}
