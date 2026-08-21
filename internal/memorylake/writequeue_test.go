package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TestAppendObservation_EnsuresConversationAndAppendsMessage drives the
// happy-path flow: POST .../memories/conversations to ensure the
// conversation exists, then POST .../conversations/{id}/messages to append
// the observation content, returning the message id.
func TestAppendObservation_EnsuresConversationAndAppendsMessage(t *testing.T) {
	var convPosts, msgPosts int32
	var lastConvBody map[string]any
	var lastMsgBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			atomic.AddInt32(&convPosts, 1)
			json.NewDecoder(r.Body).Decode(&lastConvBody)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "conv-1"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(&msgPosts, 1)
			json.NewDecoder(r.Body).Decode(&lastMsgBody)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "msg-1"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	p := store.AddObservationParams{Content: "user prefers dark mode"}
	msgID, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1", p)
	if err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if msgID != "msg-1" {
		t.Fatalf("msgID=%q, want msg-1", msgID)
	}
	if got := atomic.LoadInt32(&convPosts); got != 1 {
		t.Fatalf("convPosts=%d, want 1", got)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 1 {
		t.Fatalf("msgPosts=%d, want 1", got)
	}

	if lastConvBody["custom_id"] != "session-abc" {
		t.Fatalf("conv custom_id=%v, want session-abc", lastConvBody["custom_id"])
	}
	if lastConvBody["kind"] != "DIRECT" {
		t.Fatalf("conv kind=%v, want DIRECT", lastConvBody["kind"])
	}
	if actorIDs, _ := lastConvBody["actor_ids"].([]any); len(actorIDs) != 1 || actorIDs[0] != "actor-1" {
		t.Fatalf("conv actor_ids=%v, want [actor-1]", lastConvBody["actor_ids"])
	}
	if rwProj, _ := lastConvBody["rw_project_ids"].([]any); len(rwProj) != 1 || rwProj[0] != "proj-1" {
		t.Fatalf("conv rw_project_ids=%v, want [proj-1]", lastConvBody["rw_project_ids"])
	}

	wantHash := contentHash(p.Content)
	if lastMsgBody["custom_id"] != wantHash {
		t.Fatalf("msg custom_id=%v, want %v (sha256-derived)", lastMsgBody["custom_id"], wantHash)
	}
	if lastMsgBody["actor_id"] != "actor-1" {
		t.Fatalf("msg actor_id=%v, want actor-1", lastMsgBody["actor_id"])
	}
	content, _ := lastMsgBody["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("msg content len=%d, want 1", len(content))
	}
	block, _ := content[0].(map[string]any)
	if block["block_type"] != "TEXT" || block["text"] != p.Content {
		t.Fatalf("msg content block=%+v, want TEXT/%q", block, p.Content)
	}
}

// TestAppendObservation_SameContentIsIdempotentViaCustomID verifies the
// idempotency contract: the message custom_id is derived purely from
// content, so calling AppendObservation twice with byte-identical content
// sends the same custom_id both times. MemoryLake is documented to resolve a
// repeated custom_id to the message it already has, which this mock
// simulates by always returning the same message id.
func TestAppendObservation_SameContentIsIdempotentViaCustomID(t *testing.T) {
	var customIDs []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "conv-1"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			customIDs = append(customIDs, body.CustomID)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "msg-1"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	p := store.AddObservationParams{Content: "same content twice"}

	id1, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1", p)
	if err != nil {
		t.Fatalf("first AppendObservation: %v", err)
	}
	id2, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1", p)
	if err != nil {
		t.Fatalf("second AppendObservation: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("ids differ: %q vs %q, want equal (idempotent)", id1, id2)
	}
	if len(customIDs) != 2 || customIDs[0] != customIDs[1] {
		t.Fatalf("custom_ids=%v, want two identical values", customIDs)
	}
}

// TestPatchFactMetadata_OverwritesMetadata verifies the PATCH-metadata helper
// still used by PinObservation/UnpinObservation/MarkReviewed (the surviving
// explicit-write paths under the Option A thin adapter — see backend.go's
// doc comment on what AddObservation itself no longer does).
func TestPatchFactMetadata_OverwritesMetadata(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1" {
			json.NewDecoder(r.Body).Decode(&gotBody)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
				"id": "fact-1", "fact": "f", "metadata": gotBody["metadata"],
			}})
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	updated, err := c.patchFactMetadata("ws-1", "proj-1", "fact-1", map[string]any{"pinned": true})
	if err != nil {
		t.Fatalf("patchFactMetadata: %v", err)
	}
	if updated.Metadata["pinned"] != true {
		t.Fatalf("updated.Metadata=%v, want pinned=true", updated.Metadata)
	}
	md, _ := gotBody["metadata"].(map[string]any)
	if md["pinned"] != true {
		t.Fatalf("PATCH body metadata=%v, want pinned=true", md)
	}
	if _, hasFact := gotBody["fact"]; hasFact {
		t.Fatalf("patchFactMetadata must only send metadata, not fact: %v", gotBody)
	}
}

// TestAppendObservation_SendsHeadAsParentMessageID covers the normal case for
// every append after a session's first: the conversation already exists, so
// ensureConversation recovers via GET, and the head it reports must be sent
// back as parent_message_id. Without it MemoryLake rejects the append with a
// 409 ("already has a head entry"), which is what silently capped every
// conversation at one message.
func TestAppendObservation_SendsHeadAsParentMessageID(t *testing.T) {
	var lastMsgBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"success":    false,
				"error_code": "CUSTOM_ID_CONFLICT",
				"message":    "conversation with this custom_id already exists",
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations/session-abc":
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "conv-1", "current_message_id": "msg-7"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			json.NewDecoder(r.Body).Decode(&lastMsgBody)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "msg-8"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	msgID, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1",
		store.AddObservationParams{Content: "second turn"})
	if err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if msgID != "msg-8" {
		t.Fatalf("msgID=%q, want msg-8", msgID)
	}
	if lastMsgBody["parent_message_id"] != "msg-7" {
		t.Fatalf("parent_message_id=%v, want msg-7", lastMsgBody["parent_message_id"])
	}
}

// TestAppendObservation_OmitsParentMessageIDForFirstMessage pins the other half
// of the contract: parent_message_id must be omitted for the very first entry
// of a conversation. Sending an empty or bogus parent there is itself a 409
// ("not the current head"), so a fresh conversation must not carry the field
// at all.
func TestAppendObservation_OmitsParentMessageIDForFirstMessage(t *testing.T) {
	var lastMsgBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			// Fresh conversation: created here, so it has no head yet.
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "conv-1"},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			json.NewDecoder(r.Body).Decode(&lastMsgBody)
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "msg-1"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	if _, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1",
		store.AddObservationParams{Content: "first turn"}); err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if _, ok := lastMsgBody["parent_message_id"]; ok {
		t.Fatalf("parent_message_id must be absent for the first message, got %v",
			lastMsgBody["parent_message_id"])
	}
}

// TestAppendObservation_RetriesWithRefreshedHeadOn409 covers head drift: a
// concurrent writer (engram serve, engram mcp and engram turn are separate
// processes writing the same session's conversation) can advance the head
// between our read and our append. MemoryLake answers that with a 409, and the
// documented recovery is to re-read the head and extend it instead.
func TestAppendObservation_RetriesWithRefreshedHeadOn409(t *testing.T) {
	var gets, msgPosts int32
	var parents []any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "error_code": "CUSTOM_ID_CONFLICT", "message": "exists",
			})
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations/session-abc":
			// First read reports msg-7; by the time we append, another writer
			// has moved the head to msg-9.
			head := "msg-7"
			if atomic.AddInt32(&gets, 1) > 1 {
				head = "msg-9"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "conv-1", "current_message_id": head},
			})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			parents = append(parents, body["parent_message_id"])
			if atomic.AddInt32(&msgPosts, 1) == 1 {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{
					"success": false, "error_code": "CUSTOM_ID_CONFLICT",
					"message": "`parent_message_id` 'msg-7' is not the current head",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"id": "msg-10"},
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})
	msgID, err := c.AppendObservation("ws-1", "proj-1", "session-abc", "actor-1",
		store.AddObservationParams{Content: "concurrent turn"})
	if err != nil {
		t.Fatalf("AppendObservation: %v", err)
	}
	if msgID != "msg-10" {
		t.Fatalf("msgID=%q, want msg-10", msgID)
	}
	if got := atomic.LoadInt32(&msgPosts); got != 2 {
		t.Fatalf("msgPosts=%d, want 2 (one 409, one retry)", got)
	}
	if len(parents) != 2 || parents[0] != "msg-7" || parents[1] != "msg-9" {
		t.Fatalf("parent_message_id sequence=%v, want [msg-7 msg-9]", parents)
	}
}
