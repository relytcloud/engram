package memorylake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// AppendObservation writes an observation into MemoryLake as a conversation
// message rather than a fact directly: MemoryLake only extracts facts from
// conversation content, so engram's write path is always "ensure a
// conversation exists, then append the observation's content as a message to
// it" and lets MemoryLake's own extraction pipeline turn that message into a
// fact asynchronously (see BackfillFacts for the other half of that flow).
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
// AppendObservation returns the MemoryLake message id.
func (c *Client) AppendObservation(ws, projID, convCustomID, actorID string, p store.AddObservationParams) (string, error) {
	convID, err := c.ensureConversation(ws, projID, convCustomID, actorID)
	if err != nil {
		return "", err
	}

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
	if err := c.doJSON("POST", "/api/v3/conversations/"+convID+"/messages", body, &msg); err != nil {
		return "", err
	}
	return msg.ID, nil
}

// ensureConversation creates the MemoryLake conversation identified by
// custom_id within workspace ws, scoped to actorID and granted read-write
// access to project projID. MemoryLake treats POST .../conversations as
// idempotent on custom_id — calling this repeatedly with the same customID
// returns the same conversation id rather than creating duplicates, so no
// separate "does it already exist" lookup is needed here.
func (c *Client) ensureConversation(ws, projID, customID, actorID string) (string, error) {
	var conv struct {
		ID string `json:"id"`
	}
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
			return "", err
		}
		var existing struct {
			ID string `json:"id"`
		}
		getPath := "/api/v3/workspaces/" + ws + "/memories/conversations/" +
			url.PathEscape(customID) + "?by_custom_id=true"
		if gErr := c.doJSON("GET", getPath, nil, &existing); gErr != nil {
			return "", gErr
		}
		if existing.ID == "" {
			return "", err
		}
		return existing.ID, nil
	}
	return conv.ID, nil
}

// contentHash derives a stable idempotency key from s: the first 16 hex
// characters of its sha256 digest. Used as the custom_id for MemoryLake
// messages, so appending identical observation content twice always maps to
// the same message instead of creating a duplicate.
func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// listFacts fetches the facts MemoryLake currently has recorded for project
// projID within workspace ws.
//
// This intentionally reads only the first page (page_size=200) and is used
// exclusively by the write path (AddObservation's pre-append snapshot,
// BackfillFacts' poll loop): both need a fast, frequent read rather than an
// exhaustive one, and a project accumulating >200 unbackfilled/stale facts
// between polls is already outside the bounds this backend is designed for.
// Read-side aggregate methods (Stats, CountObservationsForProject, Timeline,
// FormatContext) need the *complete* fact list to be accurate and use
// listAllFacts instead. See spec §11.5.
func (c *Client) listFacts(ws, projID string) ([]Fact, error) {
	var out struct {
		Items []Fact `json:"items"`
	}
	path := "/api/v3/workspaces/" + ws + "/projects/" + projID + "/memories/facts?page_size=200"
	if err := c.doJSON("GET", path, nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
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
// Timeline, FormatContext) rather than the bounded, single-page listFacts
// used by the write path.
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
// with the update applied.
func (c *Client) patchFactMetadata(ws, projID, factID string, md map[string]any) (Fact, error) {
	var updated Fact
	path := "/api/v3/workspaces/" + ws + "/projects/" + projID + "/memories/facts/" + factID
	if err := c.doJSON("PATCH", path, map[string]any{"metadata": md}, &updated); err != nil {
		return Fact{}, err
	}
	return updated, nil
}

// BackfillFacts is the other half of the async write flow started by
// AppendObservation: MemoryLake extracts facts from conversation messages on
// its own schedule, so the fact engram's message eventually produces does not
// exist yet at the moment AppendObservation returns. BackfillFacts polls
// project projID's fact list every poll interval, looking for facts that
// have not yet been stamped with engram's own metadata (i.e. whose metadata
// lacks the engram_obs_id key — this is what marks a fact as "extracted by
// MemoryLake but not yet claimed by engram"), and PATCHes md onto each one it
// finds so engram can reconstruct the originating Observation from it later
// (see FactMetadata / ObservationFromFact).
//
// knownFactIDs is a snapshot of the project's fact ids taken *before* the
// message this backfill belongs to was appended (see AddObservation). Any
// fact whose id is in that snapshot pre-dates this write and must never be
// claimed by it: MemoryLake's extraction is asynchronous, so a previous save
// may have appended a message whose fact had not yet materialized (or whose
// own bounded backfill timed out), leaving an unmarked fact behind. Without
// this guard, the next save would "claim" that stale unmarked fact and PATCH
// *this* observation's engram_raw onto it, corrupting the earlier observation
// so mem_get/mem_search read back the wrong verbatim text. Restricting the
// scan to facts absent from the snapshot keeps each save associated only with
// facts that appeared after its own message was appended.
//
// BackfillFacts returns once a poll finds no additional un-backfilled facts
// and at least one fact has been backfilled so far (the set has
// "stabilized"), or once maxWait elapses — whichever comes first. Timing out
// is deliberately not treated as an error: MemoryLake's extraction is
// asynchronous and may simply not have produced a fact yet, so callers get
// back whatever was collected (possibly nothing) instead of an error, and may
// retry or proceed without a backfilled fact.
//
// poll and maxWait are caller-supplied rather than hardcoded so tests can
// drive this loop with millisecond-scale timings instead of waiting on
// production-scale polling intervals.
func (c *Client) BackfillFacts(ws, projID string, md map[string]any, knownFactIDs map[string]bool, poll, maxWait time.Duration) ([]Fact, error) {
	var backfilled []Fact
	seen := map[string]bool{}

	scanOnce := func() (foundNew bool, err error) {
		facts, err := c.listFacts(ws, projID)
		if err != nil {
			return false, err
		}
		for _, f := range facts {
			if seen[f.ID] {
				continue
			}
			if knownFactIDs[f.ID] {
				// Pre-existing before this write's message was appended — it
				// belongs to an earlier observation (possibly one whose own
				// backfill timed out) and must not be claimed here.
				continue
			}
			if _, alreadyBackfilled := f.Metadata[metaObsID]; alreadyBackfilled {
				// Backfilled already, by us in a previous run or another
				// caller — nothing to do, and it doesn't count as instability
				// for this scan.
				continue
			}
			updated, err := c.patchFactMetadata(ws, projID, f.ID, md)
			if err != nil {
				return false, err
			}
			seen[f.ID] = true
			backfilled = append(backfilled, updated)
			foundNew = true
		}
		return foundNew, nil
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	deadline := time.After(maxWait)

	for {
		select {
		case <-deadline:
			return backfilled, nil
		case <-ticker.C:
			foundNew, err := scanOnce()
			if err != nil {
				return backfilled, err
			}
			if !foundNew && len(backfilled) > 0 {
				return backfilled, nil
			}
		}
	}
}
