package memorylake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// factStore is a minimal stateful mock of the MemoryLake fact endpoints used by
// the delete flow: direct fact-add (POST), keyset list (GET), single-fact read
// (GET .../{id}), and forget (POST .../{id}/forget → mark expired). It is the
// smallest server that lets a test drive save → delete → read back end to end.
type factStore struct {
	mu        sync.Mutex
	order     []string
	byID      map[string]*mockFact
	forgotten []string // ids that received a /forget call, in order
	seq       int
}

type mockFact struct {
	id      string
	text    string
	expired bool
}

func newFactStore() *factStore { return &factStore{byID: map[string]*mockFact{}} }

func (fs *factStore) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	const base = "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts"
	return func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == "POST" && p == base: // direct fact-add
			var body struct {
				Facts []string `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			created := []map[string]any{}
			for _, text := range body.Facts {
				id := fmt.Sprintf("fact-%d", fs.seq)
				fs.seq++
				fs.byID[id] = &mockFact{id: id, text: text}
				fs.order = append(fs.order, id)
				created = append(created, map[string]any{"id": id, "fact": text})
			}
			writeData(w, map[string]any{"facts": created})

		case r.Method == "GET" && p == base: // keyset list (listAllFacts)
			items := []map[string]any{}
			for _, id := range fs.order {
				f := fs.byID[id]
				items = append(items, map[string]any{"id": f.id, "fact": f.text, "expired": f.expired})
			}
			writeData(w, map[string]any{"items": items, "continuation_token": ""})

		case r.Method == "POST" && strings.HasPrefix(p, base+"/") && strings.HasSuffix(p, "/forget"): // forget
			id := strings.TrimSuffix(strings.TrimPrefix(p, base+"/"), "/forget")
			f, ok := fs.byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"success":false,"error_code":"NOT_FOUND","message":"fact not found"}`))
				return
			}
			f.expired = true
			fs.forgotten = append(fs.forgotten, id)
			writeData(w, nil)

		case r.Method == "GET" && strings.HasPrefix(p, base+"/"): // single-fact read (getFact)
			id := strings.TrimPrefix(p, base+"/")
			f, ok := fs.byID[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"success":false,"error_code":"NOT_FOUND","message":"fact not found"}`))
				return
			}
			writeData(w, map[string]any{"id": f.id, "fact": f.text, "expired": f.expired})

		default:
			t.Fatalf("unexpected request %s %s", r.Method, p)
		}
	}
}

func writeData(w http.ResponseWriter, data any) {
	json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok", "data": data})
}

// TestE2E_SaveThenDelete_SoftDeletesAndDropsFromActive drives the full delete
// flow against the stateful mock: save an observation (real fact id back), it
// counts as active, then mem_delete forgets it (POST .../forget) and it drops
// out of the active count — MemoryLake's soft-delete semantics.
func TestE2E_SaveThenDelete_SoftDeletesAndDropsFromActive(t *testing.T) {
	fs := newFactStore()
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	// Save.
	id, err := b.AddObservation(store.AddObservationParams{Title: "Temp note", Content: "delete me", Scope: "global"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}
	if id == "" {
		t.Fatal("AddObservation returned an empty sync_id")
	}

	// Active before delete.
	if n, err := b.CountObservationsForProject("proj"); err != nil || n != 1 {
		t.Fatalf("active count before delete = %d (err=%v), want 1", n, err)
	}
	// Readable before delete.
	if obs, err := b.GetObservation(id); err != nil || obs == nil {
		t.Fatalf("GetObservation before delete: obs=%v err=%v", obs, err)
	}

	// Delete (soft — hard_delete=false).
	if err := b.DeleteObservation(id, false); err != nil {
		t.Fatalf("DeleteObservation: %v", err)
	}

	// The forget endpoint was hit with exactly this id.
	if len(fs.forgotten) != 1 || fs.forgotten[0] != id {
		t.Fatalf("forgotten=%v, want exactly [%s]", fs.forgotten, id)
	}
	// No longer counted as active.
	if n, err := b.CountObservationsForProject("proj"); err != nil || n != 0 {
		t.Fatalf("active count after delete = %d (err=%v), want 0", n, err)
	}
}

// TestE2E_HardDeleteDegradesToForget verifies that hard_delete=true does not
// reach any different endpoint — MemoryLake has no hard delete, so it degrades
// to the same forget (soft delete) as hard_delete=false.
func TestE2E_HardDeleteDegradesToForget(t *testing.T) {
	fs := newFactStore()
	srv := httptest.NewServer(fs.handler(t))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	id, err := b.AddObservation(store.AddObservationParams{Content: "hard delete me", Scope: "global"})
	if err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	// hard_delete=true must still only forget (the mock t.Fatalf's on any
	// endpoint other than add/list/read/forget, so reaching a hypothetical
	// hard-delete route would fail the test).
	if err := b.DeleteObservation(id, true); err != nil {
		t.Fatalf("DeleteObservation(hard=true): %v", err)
	}
	if len(fs.forgotten) != 1 || fs.forgotten[0] != id {
		t.Fatalf("forgotten=%v, want exactly [%s] (hard delete must degrade to forget)", fs.forgotten, id)
	}
	if n, _ := b.CountObservationsForProject("proj"); n != 0 {
		t.Fatalf("active count after hard delete = %d, want 0 (soft-deleted)", n)
	}
}
