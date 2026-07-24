package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

func strptr(s string) *string { return &s }

// factsHandler is a minimal mock of the two endpoints MigrateObservations uses:
// GET .../memories/facts (existing-fact listing, for the idempotency guard) and
// POST .../memories/facts (the direct verbatim fact-add). `existing` seeds the
// GET response; posted texts are appended to `got`.
func factsHandler(t *testing.T, existing []string, got *[]string, postCalls *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(strings.Split(r.URL.Path, "?")[0], "/memories/facts"):
			items := make([]map[string]any, 0, len(existing))
			for i, e := range existing {
				items = append(items, map[string]any{"id": "f" + string(rune('0'+i)), "fact": e})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"items": items, "continuation_token": ""},
			})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/memories/facts"):
			atomic.AddInt32(postCalls, 1)
			var body struct {
				Facts []string `json:"facts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			created := make([]map[string]any, 0, len(body.Facts))
			for i, f := range body.Facts {
				*got = append(*got, f)
				created = append(created, map[string]any{"id": "new" + string(rune('0'+i)), "fact": f})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"facts": created},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}
}

// TestMigrateObservations_DirectWriteVerbatim verifies the happy path writes
// each observation as a verbatim fact via POST, preserving the title.
func TestMigrateObservations_DirectWriteVerbatim(t *testing.T) {
	var got []string
	var postCalls int32
	srv := httptest.NewServer(factsHandler(t, nil, &got, &postCalls))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	obs := []store.Observation{
		{Title: "Use Postgres", Content: "Postgres 15 for users.", Project: strptr("acme"), Scope: "project"},
		{Title: "", Content: "Content only, no title.", Scope: "global"},
	}

	res := b.MigrateObservations(obs)

	if res.Total != 2 || res.Migrated != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("result = %+v, want Total=2 Migrated=2 Skipped=0 Failed=0", res)
	}
	if len(got) != 2 {
		t.Fatalf("posted %d facts, want 2: %v", len(got), got)
	}
	// Title is preserved (prepended) when distinct from content.
	if got[0] != "Use Postgres\n\nPostgres 15 for users." {
		t.Fatalf("fact[0] = %q, want title+content", got[0])
	}
	if got[1] != "Content only, no title." {
		t.Fatalf("fact[1] = %q, want content only", got[1])
	}
}

// TestMigrateObservations_SkipsAlreadyPresent verifies the idempotency guard:
// an observation whose rendered text already exists as a fact is skipped, not
// re-posted.
func TestMigrateObservations_SkipsAlreadyPresent(t *testing.T) {
	var got []string
	var postCalls int32
	// "Dup\n\nalready there." already exists server-side.
	srv := httptest.NewServer(factsHandler(t, []string{"Dup\n\nalready there."}, &got, &postCalls))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	obs := []store.Observation{
		{Title: "Dup", Content: "already there.", Scope: "global"},   // already present → skip
		{Title: "Fresh", Content: "brand new fact.", Scope: "global"}, // new → write
		{Title: "", Content: "", Scope: "global"},                     // empty → skip
	}

	res := b.MigrateObservations(obs)

	if res.Migrated != 1 || res.Skipped != 2 {
		t.Fatalf("result = %+v, want Migrated=1 Skipped=2", res)
	}
	if len(got) != 1 || got[0] != "Fresh\n\nbrand new fact." {
		t.Fatalf("posted = %v, want only the fresh fact", got)
	}
}

// TestMigrateObservations_CountsPostFailure verifies a failing POST batch is
// counted as failed (not fatal) and FirstErr is retained.
func TestMigrateObservations_CountsPostFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			_, _ = w.Write([]byte(`{"success":true,"data":{"items":[],"continuation_token":""}}`))
		case r.Method == "POST":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false,"error_code":"BOOM","message":"nope"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	obs := []store.Observation{
		{Title: "A", Content: "one", Scope: "global"},
		{Title: "B", Content: "two", Scope: "global"},
	}

	res := b.MigrateObservations(obs)

	if res.Migrated != 0 || res.Failed != 2 || res.FirstErr == nil {
		t.Fatalf("result = %+v, want Migrated=0 Failed=2 err!=nil", res)
	}
}
