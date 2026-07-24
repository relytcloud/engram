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

// TestMigrateObservations_AppendsEachAndCounts drives the happy path: every
// observation is appended (one conversation ensure + one message post per
// distinct content), and the result counts them all as migrated with no
// failures.
func TestMigrateObservations_AppendsEachAndCounts(t *testing.T) {
	var msgPosts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/memories/conversations"):
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"conv-1"}}`))
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/v3/conversations/") && strings.HasSuffix(r.URL.Path, "/messages"):
			atomic.AddInt32(&msgPosts, 1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"msg-x"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	obs := []store.Observation{
		{SessionID: "s1", Type: "decision", Title: "Use Postgres", Content: "Postgres 15 for users.", Project: strptr("acme"), Scope: "project", TopicKey: strptr("db/choice")},
		{SessionID: "s1", Type: "convention", Title: "Topic keys", Content: "slash-separated kebab.", Project: strptr("acme"), Scope: "project"},
		{SessionID: "s2", Type: "bugfix", Title: "Fix N+1", Content: "Batch the query.", ToolName: strptr("editor"), Scope: "global"},
	}

	res := b.MigrateObservations(obs)

	if res.Total != 3 || res.Migrated != 3 || res.Failed != 0 || res.FirstErr != nil {
		t.Fatalf("result = %+v, want Total=3 Migrated=3 Failed=0 err=nil", res)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 3 {
		t.Fatalf("message posts = %d, want 3 (one per observation)", got)
	}
}

// TestMigrateObservations_CountsFailuresAndContinues verifies a per-observation
// append failure is counted and skipped (not fatal), the run continues, and the
// first error is retained.
func TestMigrateObservations_CountsFailuresAndContinues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/memories/conversations"):
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"conv-1"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			// "boom" content fails; everything else succeeds.
			var body struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) > 0 && strings.Contains(body.Content[0].Text, "boom") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"success":false,"error_code":"BOOM","message":"nope"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"msg-ok"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	obs := []store.Observation{
		{SessionID: "s1", Content: "ok one", Scope: "global"},
		{SessionID: "s1", Content: "boom two", Scope: "global"},
		{SessionID: "s1", Content: "ok three", Scope: "global"},
	}

	res := b.MigrateObservations(obs)

	if res.Total != 3 || res.Migrated != 2 || res.Failed != 1 {
		t.Fatalf("result = %+v, want Total=3 Migrated=2 Failed=1", res)
	}
	if res.FirstErr == nil {
		t.Fatal("FirstErr should be set when a failure occurred")
	}
}
