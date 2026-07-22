package memorylake

import (
	"crypto/sha256"
	"encoding/hex"
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
		return "", err
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
// TODO(pagination): GET .../facts may be cursor-paginated (continuation_token)
// for projects with many facts. First cut only reads the first page (fixed
// page_size=200), matching the same first-cut limitation already accepted in
// ResolveWorkspaceID/EnsureProject. See spec §11.5.
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
func (c *Client) BackfillFacts(ws, projID string, md map[string]any, poll, maxWait time.Duration) ([]Fact, error) {
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
