package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// promptMessageServer answers conversation-ensure and message-append calls,
// counting how many distinct message posts arrive.
func promptMessageServer(t *testing.T, posts *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(posts, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
}

// TestBackend_AddPromptIfMissing_DedupesByContentHash is the core prompt
// idempotency test the brief asks for: a second AddPromptIfMissing call with
// the identical session_id+project+content must return the SAME id with
// inserted=false, and must not make a second MemoryLake round trip at all.
func TestBackend_AddPromptIfMissing_DedupesByContentHash(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	p := store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "please fix the bug"}

	id1, inserted1, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing (first): %v", err)
	}
	if !inserted1 {
		t.Fatal("first call should report inserted=true")
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts=%d, want 1 after first call", posts)
	}

	id2, inserted2, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing (second): %v", err)
	}
	if inserted2 {
		t.Fatal("second call with identical content should report inserted=false")
	}
	if id1 != id2 {
		t.Fatalf("id1=%d id2=%d, want equal on a dedup hit", id1, id2)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts=%d after second (dup) call, want still 1 (no network round trip on a hit)", posts)
	}
}

// TestBackend_AddPromptIfMissing_DifferentContentIsNotADuplicate verifies
// distinct content for the same session/project produces distinct ids and
// inserted=true both times.
func TestBackend_AddPromptIfMissing_DifferentContentIsNotADuplicate(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	id1, inserted1, err := b.AddPromptIfMissing(store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "prompt one"})
	if err != nil {
		t.Fatalf("AddPromptIfMissing: %v", err)
	}
	id2, inserted2, err := b.AddPromptIfMissing(store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "prompt two"})
	if err != nil {
		t.Fatalf("AddPromptIfMissing: %v", err)
	}
	if !inserted1 || !inserted2 {
		t.Fatalf("inserted1=%v inserted2=%v, want both true (distinct content)", inserted1, inserted2)
	}
	if id1 == id2 {
		t.Fatalf("id1=%d id2=%d, want distinct ids for distinct content", id1, id2)
	}
	if atomic.LoadInt32(&posts) != 2 {
		t.Fatalf("posts=%d, want 2 (one per distinct prompt)", posts)
	}
}

// TestBackend_AddPrompt_AlwaysPostsAMessage verifies plain AddPrompt (as
// opposed to AddPromptIfMissing) always appends to MemoryLake, even for
// repeat content — see appendPrompt's doc comment for why the returned id
// nonetheless matches on repeat identical content.
func TestBackend_AddPrompt_AlwaysPostsAMessage(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	p := store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "repeat me"}
	id1, err := b.AddPrompt(p)
	if err != nil {
		t.Fatalf("AddPrompt (first): %v", err)
	}
	id2, err := b.AddPrompt(p)
	if err != nil {
		t.Fatalf("AddPrompt (second): %v", err)
	}
	if id1 != id2 {
		t.Fatalf("id1=%d id2=%d, want equal (same dedup key on identical content)", id1, id2)
	}
	if atomic.LoadInt32(&posts) != 2 {
		t.Fatalf("posts=%d, want 2 (AddPrompt always appends, unlike AddPromptIfMissing)", posts)
	}
}

// TestBackend_AddPrompt_EmptySessionUsesDefaultConversation verifies a
// prompt with no session id still succeeds by falling back to the shared
// default conversation, mirroring AddObservation's convCustomID fallback.
func TestBackend_AddPrompt_EmptySessionUsesDefaultConversation(t *testing.T) {
	var gotConvCustomID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-default"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-default/messages":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AddPrompt(store.AddPromptParams{Content: "no session here"}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if gotConvCustomID != defaultConversationCustomID {
		t.Fatalf("conversation custom_id=%q, want %q", gotConvCustomID, defaultConversationCustomID)
	}
}
