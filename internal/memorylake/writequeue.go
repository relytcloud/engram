package memorylake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// AppendObservation writes an observation into MemoryLake as a conversation
// message rather than a fact directly: MemoryLake only extracts facts from
// conversation content, so engram's write path is always "ensure a
// conversation exists, then append the observation's content as a message to
// it" and lets MemoryLake's own extraction pipeline turn that message into a
// fact asynchronously, out of band. engram does not poll or backfill that
// extraction result back onto the observation it just appended — mem_save
// returns right after this append (see mcp.go's handleSave), and any
// resulting fact is only observed later via the normal read paths (Search,
// Timeline, FormatContext, ...).
//
// convCustomID identifies the conversation (callers typically pass something
// stable per engram session, e.g. the session ID) — AppendObservation ensures
// it exists (creating it on first use, resolving to the existing one on
// every call after) before appending to it. The appended message's own
// custom_id is derived from a sha256 hash of p.Content, so calling
// AppendObservation again with byte-identical content is idempotent:
// MemoryLake resolves the duplicate custom_id to the message it already has
// instead of creating a second one.
//
// Appending is not just "POST to the conversation": a message has to extend
// the conversation's current head by naming it as parent_message_id, and
// MemoryLake rejects an append that names the wrong parent (or names none once
// a head exists) with a 409. Ensuring the conversation therefore also reads its
// head, and a 409 is recovered from by re-reading the head and retrying — see
// maxAppendHeadRetries.
//
// AppendObservation returns the MemoryLake message id.
func (c *Client) AppendObservation(ws, projID, convCustomID, actorID string, p store.AddObservationParams) (string, error) {
	conv, err := c.ensureConversation(ws, projID, convCustomID, actorID)
	if err != nil {
		return "", err
	}

	head := conv.CurrentMessageID
	for attempt := 0; ; attempt++ {
		var msg struct {
			ID string `json:"id"`
		}
		body := map[string]any{
			"custom_id": contentHash(p.Content),
			"actor_id":  actorID,
			"content": []map[string]any{
				{"block_type": "TEXT", "text": p.Content},
			},
		}
		// Appends must extend the conversation's current head, so the head we
		// just read goes back as parent_message_id. It is omitted only for the
		// very first entry of a conversation, which has no head to extend —
		// sending a parent there is itself rejected.
		if head != "" {
			body["parent_message_id"] = head
		}

		err := c.doJSON("POST", "/api/v3/conversations/"+conv.ID+"/messages", body, &msg)
		if err == nil {
			return msg.ID, nil
		}

		apiErr, ok := err.(*APIError)
		if !ok || apiErr.HTTP != http.StatusConflict || attempt >= maxAppendHeadRetries {
			return "", err
		}

		// Head drift: engram writes one session's conversation from up to three
		// separate processes (`engram serve` for prompts, `engram mcp` for
		// observations, `engram turn` for turns), so another writer can extend
		// the conversation between our read and this append. MemoryLake answers
		// that with a 409 and the documented recovery is to re-read the head and
		// extend that instead. Re-reading also covers the case where the head we
		// started from was unknown (empty) but the conversation did have one.
		refreshed, gErr := c.getConversationByCustomID(ws, convCustomID)
		if gErr != nil || refreshed.CurrentMessageID == "" || refreshed.CurrentMessageID == head {
			// Nothing new to extend — report the original append failure rather
			// than masking it with a lookup error.
			return "", err
		}
		head = refreshed.CurrentMessageID
	}
}

// maxAppendHeadRetries bounds how many times AppendObservation re-reads the
// conversation head and retries after a 409. One retry absorbs a single
// concurrent writer, which is the realistic case for a per-session
// conversation; a caller losing the race repeatedly gets the error instead of
// looping against a server that keeps moving the head.
const maxAppendHeadRetries = 1

// AddFacts writes each string in facts verbatim into project projID via
// MemoryLake's direct fact-add endpoint (POST .../memories/facts, shipped
// 2026-07-23), bypassing the conversation → mem0-extraction pipeline entirely:
// every text becomes a new fact stored as-is, synchronously, and the created
// facts (with their real fact ids) are returned. Unlike AppendObservation this
// is deliberately NOT deduplicated or decided server-side — posting the same
// text twice creates two facts — so callers that need idempotency must dedup
// before calling (see MemoryLakeBackend.MigrateObservations). A nil/empty
// facts slice is a no-op.
func (c *Client) AddFacts(ws, projID string, facts []string) ([]Fact, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	var out struct {
		Facts []Fact `json:"facts"`
	}
	path := "/api/v3/workspaces/" + ws + "/projects/" + projID + "/memories/facts"
	if err := c.doJSON("POST", path, map[string]any{"facts": facts}, &out); err != nil {
		return nil, err
	}
	return out.Facts, nil
}

// ensureConversation creates the MemoryLake conversation identified by
// custom_id within workspace ws, scoped to actorID and granted read-write
// access to project projID, and returns it along with its current head (see
// conversationRef).
//
// POST .../conversations is NOT idempotent on custom_id — it rejects a
// duplicate rather than returning the existing row — so the conflict is
// recovered from explicitly below. That recovery is also what supplies the
// head: a freshly created conversation has none, while an existing one
// generally does, and an append has to name it.
func (c *Client) ensureConversation(ws, projID, customID, actorID string) (conversationRef, error) {
	var conv conversationRef
	body := map[string]any{
		"custom_id":      customID,
		"kind":           "DIRECT",
		"actor_ids":      []string{actorID},
		"rw_project_ids": []string{projID},
	}
	if err := c.doJSON("POST", "/api/v3/workspaces/"+ws+"/memories/conversations", body, &conv); err != nil {
		// A conversation with this custom_id already exists (409): the create
		// endpoint is NOT idempotent — it rejects duplicate custom_ids rather
		// than returning the existing row. This is the normal case for every
		// mem_save after the first within a session. Recover by fetching the
		// existing conversation by custom_id.
		if apiErr, ok := err.(*APIError); !ok || apiErr.Code != "CUSTOM_ID_CONFLICT" {
			return conversationRef{}, err
		}
		existing, gErr := c.getConversationByCustomID(ws, customID)
		if gErr != nil {
			return conversationRef{}, gErr
		}
		if existing.ID == "" {
			return conversationRef{}, err
		}
		return existing, nil
	}
	return conv, nil
}

// conversationRef is the subset of MemoryLake's conversation object this
// package reads. CurrentMessageID is the conversation's head — the id of its
// latest message, which an append has to name as its parent_message_id. It is
// empty for a conversation that has no messages yet, and it is the reason a
// plain "ensure the conversation exists" is not enough to append to one.
type conversationRef struct {
	ID               string `json:"id"`
	CurrentMessageID string `json:"current_message_id"`
}

// getConversationByCustomID resolves a conversation by its caller-defined
// custom_id (the engram session id), returning its id and current head.
// Used both to recover from ensureConversation's create conflict and to
// re-read a head that moved under a concurrent writer.
func (c *Client) getConversationByCustomID(ws, customID string) (conversationRef, error) {
	var conv conversationRef
	path := "/api/v3/workspaces/" + ws + "/memories/conversations/" +
		url.PathEscape(customID) + "?by_custom_id=true"
	if err := c.doJSON("GET", path, nil, &conv); err != nil {
		return conversationRef{}, err
	}
	return conv, nil
}

// contentHash derives a stable idempotency key from s: the first 16 hex
// characters of its sha256 digest. Used as the custom_id for MemoryLake
// messages, so appending identical observation content twice always maps to
// the same message instead of creating a duplicate.
func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// maxListAllFactsPages bounds how many pages listAllFacts will follow via
// continuation_token before giving up. At the fixed page_size=200 this caps a
// single call at 200,000 facts — generously beyond any project this backend
// is designed for (see spec §11.5) — while guaranteeing the loop always
// terminates even against a misbehaving/malicious server that never stops
// returning a continuation_token. Mirrors BackfillFacts' bounded-wait
// posture: hitting the cap is not treated as an error, it just stops early
// and returns whatever was collected so far (task-12 hardening brief I2).
const maxListAllFactsPages = 1000

// maxPaginationPages is maxListAllFactsPages' counterpart for every other
// cursor-paginated MemoryLake list endpoint this package follows to
// exhaustion (workspaces, projects, actors — see identity.go and
// backend.go's ProjectExists/ListProjectNames). Kept as its own named
// constant rather than reusing maxListAllFactsPages so the two concerns
// (facts vs. everything else) can be tuned independently even though they
// currently share the same value and the same task-12 rationale: a page cap
// that is both generous for real usage and guaranteed to terminate against a
// misbehaving/malicious server.
const maxPaginationPages = 1000

// paginatedPage is the response shape every MemoryLake cursor-paginated list
// endpoint wraps its items in: `data.items` plus `data.continuation_token`
// (empty/absent once there are no more pages).
type paginatedPage[T any] struct {
	Items             []T    `json:"items"`
	ContinuationToken string `json:"continuation_token"`
}

// listAllPages is the shared pagination loop behind every "fetch the entire
// list" call in this package (facts, workspaces, projects, actors): it
// follows continuation_token across pages, calling pathForPage with the
// token for the next page to fetch ("" for the first) until the server
// stops returning one or maxPages is reached — whichever comes first.
// Hitting maxPages is deliberately not an error (mirrors listAllFacts' prior
// standalone behavior, task-12 hardening brief I2): it just stops early and
// returns whatever was collected so far, with what identifying the call site
// in the log line so operators can tell which list ran away.
func listAllPages[T any](c *Client, maxPages int, what string, pathForPage func(token string) string) ([]T, error) {
	var all []T
	token := ""
	for page := 0; page < maxPages; page++ {
		var out paginatedPage[T]
		if err := c.doJSON("GET", pathForPage(token), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if out.ContinuationToken == "" {
			return all, nil
		}
		token = out.ContinuationToken
		if page == maxPages-1 {
			log.Printf("[memorylake] %s: reached page cap=%d; server keeps returning a continuation_token, stopping with %d items collected so far",
				what, maxPages, len(all))
		}
	}
	return all, nil
}

// listAllFacts fetches every fact MemoryLake has recorded for project projID
// within workspace ws, following continuation_token across pages until the
// server stops returning one or maxListAllFactsPages is reached (see its doc
// comment) — whichever comes first. Used by read/aggregate paths that must
// count or enumerate the whole project (Stats, CountObservationsForProject,
// Timeline, FormatContext).
func (c *Client) listAllFacts(ws, projID string) ([]Fact, error) {
	what := fmt.Sprintf("listAllFacts for project %s (ws %s)", projID, ws)
	return listAllPages[Fact](c, maxListAllFactsPages, what, func(token string) string {
		path := "/api/v3/workspaces/" + ws + "/projects/" + projID + "/memories/facts?page_size=200"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		return path
	})
}

// patchFactMetadata overwrites the metadata of fact factID (within project
// projID, workspace ws) with md, returning the Fact MemoryLake echoes back
// with the update applied. Used by PinObservation/UnpinObservation/
// MarkReviewed — the handful of explicit, Engram-only metadata fields that
// survive the Option A thin-adapter cut (see mapper.go's doc comment).
func (c *Client) patchFactMetadata(ws, projID, factID string, md map[string]any) (Fact, error) {
	var updated Fact
	path := "/api/v3/workspaces/" + ws + "/projects/" + projID + "/memories/facts/" + factID
	if err := c.doJSON("PATCH", path, map[string]any{"metadata": md}, &updated); err != nil {
		return Fact{}, err
	}
	return updated, nil
}
