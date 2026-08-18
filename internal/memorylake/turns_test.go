package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// turnServer answers the two calls AppendTurn makes and records what arrived.
func turnServer(t *testing.T, convPosts, msgPosts *int32, gotText *string, gotConvCustomID *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			atomic.AddInt32(convPosts, 1)
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			*gotConvCustomID = body.CustomID
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-1"}})

		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-1/messages":
			atomic.AddInt32(msgPosts, 1)
			var body struct {
				CustomID string `json:"custom_id"`
				Content  []struct {
					BlockType string `json:"block_type"`
					Text      string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.Content) > 0 {
				*gotText = body.Content[0].Text
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-" + body.CustomID}})

		default:
			t.Fatalf("unexpected request %s %s (AppendTurn must only ensure a conversation and append one message)", r.Method, r.URL.Path)
		}
	}))
}

func TestAppendTurnPostsOneMessageOnTheSessionConversation(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	content := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	id, err := b.AppendTurn("sess-42", content)
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if id == "" {
		t.Fatal("AppendTurn must return the MemoryLake message id")
	}
	if convPosts != 1 || msgPosts != 1 {
		t.Fatalf("convPosts=%d msgPosts=%d, want 1/1", convPosts, msgPosts)
	}
	if gotConvID != "sess-42" {
		t.Fatalf("conversation custom_id = %q, want the session id", gotConvID)
	}
	if gotText != content {
		t.Fatalf("posted text = %q, want the merged content verbatim", gotText)
	}
}

// TestAppendTurnIsIdempotentOnContent locks the re-run guarantee: the message
// custom_id is a content hash, so replaying the same turn maps to the same
// message id rather than creating a second one.
func TestAppendTurnIsIdempotentOnContent(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	content := "**User:**\nsame\n\n**Assistant:**\nsame"
	first, err := b.AppendTurn("sess-1", content)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.AppendTurn("sess-1", content)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same content must map to the same message id: %q vs %q", first, second)
	}
}

func TestAppendTurnWithoutSessionUsesDefaultConversation(t *testing.T) {
	var convPosts, msgPosts int32
	var gotText, gotConvID string
	srv := turnServer(t, &convPosts, &msgPosts, &gotText, &gotConvID)
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("", "**User:**\nx\n\n**Assistant:**\ny"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if gotConvID != defaultConversationCustomID {
		t.Fatalf("conversation custom_id = %q, want %q", gotConvID, defaultConversationCustomID)
	}
}

func TestAppendTurnRejectsEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("empty content must not reach the network (%s %s)", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("sess-1", "   \n\t "); err == nil {
		t.Fatal("empty content must be rejected")
	}
}

// TestAppendTurnRecoversFromConversationConflict covers the normal case for
// every turn after the first in a session: the create call rejects the
// duplicate custom_id and the existing conversation is fetched instead.
func TestAppendTurnRecoversFromConversationConflict(t *testing.T) {
	var msgPosts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			w.WriteHeader(http.StatusConflict)
			// Error shape copied from internal/memorylake/identity_test.go:
			// error_code is top-level, not nested under an "error" object.
			json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "custom_id already exists", "error_code": "CUSTOM_ID_CONFLICT",
			})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/memories/conversations/"):
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "conv-existing"}})
		case r.Method == "POST" && r.URL.Path == "/api/v3/conversations/conv-existing/messages":
			atomic.AddInt32(&msgPosts, 1)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"id": "msg-1"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	b := newTestBackend(t, srv.URL)

	if _, err := b.AppendTurn("sess-1", "**User:**\nx\n\n**Assistant:**\ny"); err != nil {
		t.Fatalf("AppendTurn must recover from CUSTOM_ID_CONFLICT: %v", err)
	}
	if msgPosts != 1 {
		t.Fatalf("msgPosts=%d, want 1 on the existing conversation", msgPosts)
	}
}
