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
