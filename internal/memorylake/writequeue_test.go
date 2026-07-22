package memorylake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// TestBackfillFacts_PollsUntilNewFactAppearsThenBackfills is the scenario
// from the task-7 brief: GET .../facts is empty for the first two polls,
// then on the third returns a single newly-extracted fact. BackfillFacts
// must keep polling (on the caller-supplied interval, not a hardcoded one),
// notice the fact once it appears, PATCH engram's metadata onto it exactly
// once, and return it without waiting the full maxWait.
func TestBackfillFacts_PollsUntilNewFactAppearsThenBackfills(t *testing.T) {
	var getCount int32
	var patchCount int32
	var lastPatchMetadata map[string]any

	fact := Fact{
		ID:        "fact-1",
		Fact:      "user prefers dark mode",
		Metadata:  map[string]any{},
		CreatedAt: "2026-07-22T00:00:00Z",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts":
			n := atomic.AddInt32(&getCount, 1)
			var items []Fact
			if n >= 3 {
				items = []Fact{fact}
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    map[string]any{"items": items},
			})
		case r.Method == "PATCH" && r.URL.Path == "/api/v3/workspaces/ws-1/projects/proj-1/memories/facts/fact-1":
			atomic.AddInt32(&patchCount, 1)
			var body struct {
				Metadata map[string]any `json:"metadata"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			lastPatchMetadata = body.Metadata
			// Real MemoryLake would persist the patched metadata; simulate
			// that so the next GET reflects it (and BackfillFacts sees the
			// fact as already-backfilled, letting the poll loop stabilize).
			fact.Metadata = body.Metadata
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"data":    fact,
			})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	md := map[string]any{
		metaRaw:   "user prefers dark mode, verbatim",
		metaObsID: "42",
	}

	facts, err := c.BackfillFacts("ws-1", "proj-1", md, 5*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("BackfillFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	if facts[0].ID != "fact-1" {
		t.Fatalf("fact id=%q, want fact-1", facts[0].ID)
	}
	if got := atomic.LoadInt32(&getCount); got < 3 {
		t.Fatalf("getCount=%d, want at least 3 (2 empty polls + 1 with the fact)", got)
	}
	if got := atomic.LoadInt32(&patchCount); got != 1 {
		t.Fatalf("patchCount=%d, want exactly 1 (must not re-PATCH once backfilled)", got)
	}
	if lastPatchMetadata[metaRaw] != "user prefers dark mode, verbatim" {
		t.Fatalf("PATCH metadata missing engram_raw: %+v", lastPatchMetadata)
	}
	if lastPatchMetadata[metaObsID] != "42" {
		t.Fatalf("PATCH metadata missing engram_obs_id: %+v", lastPatchMetadata)
	}
}

// TestBackfillFacts_TimesOutReturningPartialResults verifies the timeout
// path: if no fact ever shows up, BackfillFacts must give up after maxWait
// and return an empty (not nil-error) result — a timeout is not a failure,
// since MemoryLake's extraction may simply not have run yet.
func TestBackfillFacts_TimesOutReturningPartialResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"items": []Fact{}},
		})
	}))
	defer srv.Close()

	c := NewClient(Config{BaseURL: srv.URL, APIKey: "sk-test", TimeoutMS: 5000})

	start := time.Now()
	facts, err := c.BackfillFacts("ws-1", "proj-1", map[string]any{}, 5*time.Millisecond, 30*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("BackfillFacts: %v, want nil (timeout is not an error)", err)
	}
	if len(facts) != 0 {
		t.Fatalf("got %d facts, want 0 (nothing ever appeared)", len(facts))
	}
	if elapsed < 30*time.Millisecond {
		t.Fatalf("returned after %v, want >= maxWait (30ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("returned after %v, want reasonably close to maxWait (30ms)", elapsed)
	}
}
