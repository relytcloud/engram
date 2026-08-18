package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// convStore is a stateful mock of the conversation endpoints: it enforces the
// same custom_id semantics the real MemoryLake has — conversation creation
// rejects a duplicate custom_id (recoverable by GET), and a message whose
// custom_id was already posted resolves to the existing message instead of
// creating a second one.
type convStore struct {
	mu       sync.Mutex
	convByID map[string]string   // conversation custom_id -> conversation id
	msgs     map[string][]string // conversation id -> message texts, in order
	msgIDs   map[string]string   // message custom_id -> message id
	seq      int
}

func newConvStore() *convStore {
	return &convStore{
		convByID: map[string]string{},
		msgs:     map[string][]string{},
		msgIDs:   map[string]string{},
	}
}

func (c *convStore) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()

		ok := func(data any) {
			json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
		}

		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v3/workspaces/ws-1/memories/conversations":
			var body struct {
				CustomID string `json:"custom_id"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if _, exists := c.convByID[body.CustomID]; exists {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]any{
					"success":    false,
					"message":    "exists",
					"error_code": "CUSTOM_ID_CONFLICT",
				})
				return
			}
			c.seq++
			id := "conv-" + strconv.Itoa(c.seq)
			c.convByID[body.CustomID] = id
			ok(map[string]any{"id": id})

		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v3/workspaces/ws-1/memories/conversations/"):
			segments := strings.Split(r.URL.Path, "/")
			custom := segments[len(segments)-1]
			id, exists := c.convByID[custom]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]any{
					"success":    false,
					"message":    "no such conversation",
					"error_code": "NOT_FOUND",
				})
				return
			}
			ok(map[string]any{"id": id})

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			segments := strings.Split(r.URL.Path, "/")
			var convID string
			for i, seg := range segments {
				if seg == "conversations" && i+1 < len(segments) {
					convID = segments[i+1]
					break
				}
			}
			var body struct {
				CustomID string `json:"custom_id"`
				Content  []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if id, exists := c.msgIDs[body.CustomID]; exists {
				// MemoryLake's message idempotency: same custom_id, same message.
				ok(map[string]any{"id": id})
				return
			}
			c.seq++
			id := "msg-" + strconv.Itoa(c.seq)
			c.msgIDs[body.CustomID] = id
			text := ""
			if len(body.Content) > 0 {
				text = body.Content[0].Text
			}
			c.msgs[convID] = append(c.msgs[convID], text)
			ok(map[string]any{"id": id})

		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

// TestTurnSyncFlow_PromptSuppressedAndTurnAppendedOnce is the whole feature in
// one test: with suppression on, the prompt append makes no request, the turn
// lands as exactly one message on the session's conversation, and replaying
// the same turn does not add a second one.
func TestTurnSyncFlow_PromptSuppressedAndTurnAppendedOnce(t *testing.T) {
	cs := newConvStore()
	srv := httptest.NewServer(cs.handler(t))
	defer srv.Close()

	b := newTestBackend(t, srv.URL)
	b.SetSkipPromptAppend(true)

	// 1. The user's prompt is persisted through the normal path — and must not
	//    reach MemoryLake, because the merged turn carries it.
	if _, err := b.AddPrompt(store.AddPromptParams{
		SessionID: "sess-e2e", Project: "acme", Content: "fix the uploader",
	}); err != nil {
		t.Fatalf("AddPrompt: %v", err)
	}

	// 2. The turn arrives.
	content := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	if _, err := b.AppendTurn("sess-e2e", content); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	// 3. A replay of the same turn (a re-run, a manual backfill) is a no-op.
	if _, err := b.AppendTurn("sess-e2e", content); err != nil {
		t.Fatalf("AppendTurn (replay): %v", err)
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	convID, exists := cs.convByID["sess-e2e"]
	if !exists {
		t.Fatal("the turn must create a conversation keyed by the session id")
	}
	msgs := cs.msgs[convID]
	if len(msgs) != 1 {
		t.Fatalf("conversation holds %d messages, want exactly 1: %#v", len(msgs), msgs)
	}
	if msgs[0] != content {
		t.Fatalf("stored message = %q, want the merged turn verbatim", msgs[0])
	}
}
