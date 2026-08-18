package memorylake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// promptMessageServer answers conversation-ensure and message-append calls,
// counting how many distinct message posts arrive. The returned message id is
// derived from the posted custom_id (itself a content hash, see
// AppendObservation) so byte-identical content always maps to the same
// message id and distinct content maps to distinct ids — mirroring
// MemoryLake's real idempotency contract.
func promptMessageServer(t *testing.T, posts *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(posts, 1)
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-" + body.CustomID}})
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
		t.Fatalf("id1=%q id2=%q, want equal on a dedup hit", id1, id2)
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
		t.Fatalf("id1=%q id2=%q, want distinct ids for distinct content", id1, id2)
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
		t.Fatalf("id1=%q id2=%q, want equal (same dedup key on identical content)", id1, id2)
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

// TestBackend_SkipPromptAppend_MakesNoRequest is the regression lock for the
// "not affecting existing behavior" half of per-turn conversation sync: with
// the flag set, prompt persistence must not touch the network at all (the
// merged turn message already carries the user's text), yet must still hand
// callers a stable, non-empty id so no call site needs a special case.
func TestBackend_SkipPromptAppend_MakesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no request may be made while prompt append is suppressed (%s %s)", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)
	b.SetSkipPromptAppend(true)

	p := store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "please fix the bug"}

	id, err := b.AddPrompt(p)
	if err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if id == "" {
		t.Fatal("AddPrompt must still return a stable non-empty id")
	}

	id2, inserted, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing: %v", err)
	}
	if !inserted {
		t.Fatal("first AddPromptIfMissing must still report inserted=true")
	}
	if id2 != id {
		t.Fatalf("suppressed ids must be stable for identical content: %q vs %q", id2, id)
	}

	_, inserted2, err := b.AddPromptIfMissing(p)
	if err != nil {
		t.Fatalf("AddPromptIfMissing (repeat): %v", err)
	}
	if inserted2 {
		t.Fatal("repeat AddPromptIfMissing must still report inserted=false")
	}
}

// TestBackend_SkipPromptAppend_DefaultsOff proves the flag is opt-in: a backend
// nobody configured behaves exactly as before.
func TestBackend_SkipPromptAppend_DefaultsOff(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AddPrompt(store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: "hello"}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts=%d, want 1 — suppression must default to off", posts)
	}
}

// TestBackend_SkipPromptAppend_TogglesOnALiveBackend is the hot-reload
// contract: the routing layer flips this flag on a backend that is already
// constructed and already serving, so a toggle must take effect on the very
// next call rather than at the next process start. Without it, enabling
// per-turn sync leaves prompts being appended twice (once standalone, once
// inside the merged turn) until the user kills engram serve.
func TestBackend_SkipPromptAppend_TogglesOnALiveBackend(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	p := func(content string) store.AddPromptParams {
		return store.AddPromptParams{SessionID: "sess-1", Project: "proj", Content: content}
	}

	// Off: a real append happens.
	if _, err := b.AddPrompt(p("first")); err != nil {
		t.Fatalf("AddPrompt (suppression off): %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("posts=%d, want 1 while suppression is off", got)
	}

	// Flip it on mid-life — no reconstruction.
	b.SetSkipPromptAppend(true)
	if _, err := b.AddPrompt(p("second")); err != nil {
		t.Fatalf("AddPrompt (suppression on): %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Fatalf("posts=%d, want still 1 — the toggle must take effect immediately", got)
	}

	// And back off again, which is the disable direction.
	b.SetSkipPromptAppend(false)
	if _, err := b.AddPrompt(p("third")); err != nil {
		t.Fatalf("AddPrompt (suppression off again): %v", err)
	}
	if got := atomic.LoadInt32(&posts); got != 2 {
		t.Fatalf("posts=%d, want 2 — turning suppression back off must resume appends", got)
	}
}

// TestBackend_SkipPromptAppend_ConcurrentToggleAndRead must be run under -race.
// The routing selector now writes this flag on every resolve while handlers
// read it concurrently, so the field cannot be a plain bool.
func TestBackend_SkipPromptAppend_ConcurrentToggleAndRead(t *testing.T) {
	var posts int32
	srv := promptMessageServer(t, &posts)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				b.SetSkipPromptAppend(n%2 == 0)
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				_, _ = b.AddPrompt(store.AddPromptParams{
					SessionID: "sess-race", Project: "proj",
					Content:   fmt.Sprintf("content-%d-%d", i, n),
				})
			}
		}(i)
	}
	wg.Wait()
}
