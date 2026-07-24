package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBackend_Session_OpenReadEnd is the end-to-end "open, read, end" cycle
// the task brief asks for: CreateSession ensures a MemoryLake conversation
// (a real, tested write), GetSession reads back what CreateSession recorded,
// and EndSession marks it ended with a summary that a subsequent GetSession
// reflects.
func TestBackend_Session_OpenReadEnd(t *testing.T) {
	var convCreated, summaryWritten bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			convCreated = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-sess-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			summaryWritten = true
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"facts": []map[string]any{{"id": "fact-summary", "fact": "Session summary"}},
			}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)

	if err := b.CreateSession("sess-1", "myproj", "/home/me/myproj"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !convCreated {
		t.Fatal("expected CreateSession to ensure a MemoryLake conversation")
	}

	sess, err := b.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.ID != "sess-1" || sess.Project != "myproj" || sess.Directory != "/home/me/myproj" {
		t.Fatalf("sess=%+v, unexpected fields", sess)
	}
	if sess.EndedAt != nil || sess.Summary != nil {
		t.Fatalf("sess=%+v, expected EndedAt/Summary unset before EndSession", sess)
	}

	if err := b.EndSession("sess-1", "wrapped up the feature"); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if !summaryWritten {
		t.Fatal("expected EndSession to write the summary as a verbatim MemoryLake fact")
	}

	ended, err := b.GetSession("sess-1")
	if err != nil {
		t.Fatalf("GetSession (after end): %v", err)
	}
	if ended.EndedAt == nil {
		t.Fatal("expected EndedAt to be set after EndSession")
	}
	if ended.Summary == nil || *ended.Summary != "wrapped up the feature" {
		t.Fatalf("Summary=%v, want %q", ended.Summary, "wrapped up the feature")
	}
}

// TestBackend_GetSession_UnknownIDErrors verifies GetSession surfaces a
// non-nil error (not a nil *store.Session) for a session id that was never
// created, matching the contract internal/mcp's resolveSaveWriteProject
// relies on (branches on err != nil).
func TestBackend_GetSession_UnknownIDErrors(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	sess, err := b.GetSession("never-created")
	if err == nil {
		t.Fatal("expected an error for an unknown session id")
	}
	if sess != nil {
		t.Fatalf("expected nil session on error, got %+v", sess)
	}
}

// TestBackend_EndSession_UnknownIDIsNoOp mirrors internal/store's EndSession
// treating an end for an unrecorded session as a no-op rather than an error,
// and must not touch MemoryLake at all (no summary message appended) since
// there is no known conversation/session to attach it to.
func TestBackend_EndSession_UnknownIDIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s (EndSession on an unknown id must not call MemoryLake)", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	if err := b.EndSession("never-created", "summary"); err != nil {
		t.Fatalf("EndSession(unknown): %v, want nil (no-op)", err)
	}
}

// conversationEnsureServer answers every conversation-ensure/message-append
// call generically, for tests that only care about session bookkeeping.
func conversationEnsureServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-" + r.URL.Path}})
		case r.Method == "POST" && r.URL.Path != "":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func TestBackend_MostRecentActiveSession_PicksLatestUnended(t *testing.T) {
	srv := conversationEnsureServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if err := b.CreateSession("sess-old", "proj", "/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := b.CreateSession("sess-new", "proj", "/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Force a deterministic ordering independent of wall-clock granularity by
	// directly seeding started_at via the sidecar the same way CreateSession
	// does, then re-verify through the public API.
	b.sessions.Sessions["sess-old"].StartedAt = "2026-07-20 00:00:00"
	b.sessions.Sessions["sess-new"].StartedAt = "2026-07-21 00:00:00"

	id, ok, err := b.MostRecentActiveSession("proj")
	if err != nil {
		t.Fatalf("MostRecentActiveSession: %v", err)
	}
	if !ok || id != "sess-new" {
		t.Fatalf("MostRecentActiveSession=(%q,%v), want (sess-new,true)", id, ok)
	}
}

func TestBackend_MostRecentActiveSession_NoneActive(t *testing.T) {
	b := newTestBackend(t, "http://127.0.0.1:0")
	id, ok, err := b.MostRecentActiveSession("empty-project")
	if err != nil {
		t.Fatalf("MostRecentActiveSession: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("MostRecentActiveSession=(%q,%v), want (\"\",false)", id, ok)
	}
}

func TestBackend_RecentSessions_OrdersMostRecentFirst(t *testing.T) {
	srv := conversationEnsureServer(t)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if err := b.CreateSession("sess-a", "proj", "/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := b.CreateSession("sess-b", "proj", "/dir"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	b.sessions.Sessions["sess-a"].StartedAt = "2026-07-20 00:00:00"
	b.sessions.Sessions["sess-b"].StartedAt = "2026-07-21 00:00:00"

	got, err := b.RecentSessions("proj", 5)
	if err != nil {
		t.Fatalf("RecentSessions: %v", err)
	}
	if len(got) != 2 || got[0].ID != "sess-b" || got[1].ID != "sess-a" {
		t.Fatalf("RecentSessions=%+v, want [sess-b, sess-a]", got)
	}
}
