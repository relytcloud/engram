package memorylake

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// maxObservationLength mirrors internal/store's default MaxObservationLength
// (store.NewStore's cfg default is 50000). MemoryLake-backed projects must
// enforce the same content-length ceiling so mem_* handlers behave identically
// regardless of which backend a project uses.
const maxObservationLength = 50000

// defaultConversationCustomID is used as the MemoryLake conversation key for an
// AddObservation whose params carry no SessionID. MemoryLake requires every
// message to belong to a conversation; a stable fallback keeps such writes from
// scattering across one-off conversations.
const defaultConversationCustomID = "engram-default"

// MemoryLakeBackend is the opt-in, per-project alternate to the local SQLite
// store. It implements the same call surface as internal/mcp.MemoryBackend
// (see backend_test.go's memoryBackend mirror) by translating engram's
// observation model onto MemoryLake's V3 fact/conversation APIs.
//
// Engram's SQLite store remains the source of truth for projects that have not
// opted in; this backend is only constructed for projects explicitly enabled
// for MemoryLake.
type MemoryLakeBackend struct {
	client  *Client
	cfg     Config
	ws      string // resolved workspace id ("ws-...")
	projID  string // resolved MemoryLake project id
	actorID string // resolved MemoryLake actor id
	idmap   *IDMap
	topics  *TopicIndex
	// sessions is the local sidecar recording session lifecycle fields
	// (project/directory/started_at/ended_at/summary) that have no confirmed
	// analogue on a MemoryLake conversation object — see SessionIndex's doc
	// comment for why this can't just be a GET on the conversation itself.
	sessions *SessionIndex
	poll     time.Duration
	maxWait  time.Duration

	// writeMu serializes the write path (AddObservation) for this backend
	// instance. AddObservation is a snapshot→append→backfill sequence:
	// BackfillFacts claims any fact that is both absent from the pre-append
	// snapshot and not yet stamped with engram metadata. Two concurrent
	// AddObservation calls on the same project could each snapshot before the
	// other appends, so each would see the other's freshly-extracted fact as
	// "new and unmarked" and race to claim it — one save stamping its own
	// engram_raw onto the fact that belongs to the other (data corruption on
	// read-back). Serializing the whole sequence per backend closes that
	// window. Because routing caches a single backend instance per project,
	// this mutex is naturally per-project: distinct projects hold distinct
	// instances and never contend. Read paths (Get/Search/Timeline/...) are
	// deliberately NOT guarded — only the write path needs ordering.
	writeMu sync.Mutex
}

// NewBackend constructs a MemoryLakeBackend for the given workspace reference
// (custom_id, name, or "ws-" id) and already-resolved MemoryLake project id.
// It resolves the workspace id, ensures a HUMAN actor exists (keyed by
// cfg.Actor, falling back to the machine hostname), and loads the per-project
// IDMap from ~/.engram/memorylake-idmap-<projID>.json.
func NewBackend(cfg Config, ws, projID string) (*MemoryLakeBackend, error) {
	client := NewClient(cfg)

	wsID, err := client.ResolveWorkspaceID(ws)
	if err != nil {
		return nil, err
	}

	actorCustomID := cfg.Actor
	if actorCustomID == "" {
		if h, herr := os.Hostname(); herr == nil && h != "" {
			actorCustomID = h
		} else {
			actorCustomID = "engram"
		}
	}
	actorID, err := client.EnsureActor(wsID, actorCustomID, actorCustomID)
	if err != nil {
		return nil, err
	}

	idmap, err := LoadIDMap(idmapPath(projID))
	if err != nil {
		return nil, err
	}

	topics, err := LoadTopicIndex(topicIndexPath(projID))
	if err != nil {
		return nil, err
	}

	sessions, err := LoadSessionIndex(sessionIndexPath(projID))
	if err != nil {
		return nil, err
	}

	return &MemoryLakeBackend{
		client:   client,
		cfg:      cfg,
		ws:       wsID,
		projID:   projID,
		actorID:  actorID,
		idmap:    idmap,
		topics:   topics,
		sessions: sessions,
		poll:     time.Duration(cfg.ExtractPollMS) * time.Millisecond,
		maxWait:  time.Duration(cfg.ExtractMaxWaitMS) * time.Millisecond,
	}, nil
}

// idmapPath returns the per-project IDMap location:
// ~/.engram/memorylake-idmap-<projID>.json.
func idmapPath(projID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake-idmap-"+projID+".json")
}

// ─── Observation CRUD (Tier A: core) ────────────────────────────────────────

// AddObservation writes an observation to MemoryLake and returns a stable
// int64 id.
//
// Because `engram mcp` is a short-lived subprocess, a fire-and-forget goroutine
// would be killed when the process exits — so the extraction/backfill step runs
// synchronously here (bounded by cfg.ExtractMaxWaitMS). The flow:
//
//  1. AppendObservation posts the content as a conversation message.
//  2. BackfillFacts polls (bounded) for the fact MemoryLake extracts from that
//     message and stamps engram's metadata onto it.
//  3. Every backfilled fact id is registered in the IDMap. The first fact's
//     int64 id is returned.
//
// If extraction produces no fact before the deadline, AddObservation does NOT
// error: it returns a provisional int64 keyed off the MemoryLake message id
// (the fact will materialize later; a subsequent AddObservation with identical
// content is idempotent on the message custom_id). Callers treat this as a
// successful, pending save.
func (b *MemoryLakeBackend) AddObservation(p store.AddObservationParams) (int64, error) {
	// Serialize the snapshot→append→backfill sequence against concurrent
	// AddObservation calls on this same project (see writeMu's doc comment):
	// without it, two concurrent saves can each claim the other's extracted
	// fact and overwrite its engram_raw. Only the write path is locked; reads
	// stay concurrent.
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	// topic_key upsert: same project+scope+topic_key updates the existing
	// fact in place (mirrors internal/store's topic_key match-and-UPDATE
	// path) instead of appending a new conversation message. The local
	// TopicIndex is what makes "the existing fact" findable at all — see
	// topicindex.go's doc comment for why MemoryLake itself can't answer this
	// query (metadata isn't filterable server-side).
	//
	// A TopicIndex hit is NOT trusted blindly: the index is only ever this
	// backend's own eventually-consistent cache, and internal/store's
	// equivalent match query filters `deleted_at IS NULL` — a hit here must
	// clear the same bar. Before PATCHing, fetch the fact and reject the hit
	// (falling through to the normal append+backfill path below, exactly as
	// if the index had never had an entry) when either:
	//   - the fact has since been forgotten (Expired == true) — without this
	//     check, upserting into a forgotten fact would silently "revive" it:
	//     Search/Timeline/Stats/FormatContext all exclude expired facts, so
	//     the save would report success while its content stays permanently
	//     invisible (task-12 hardening brief C1).
	//   - the fact's own metadata no longer agrees with the scope/topic_key
	//     that produced this lookup (normalized on both sides) — this catches
	//     index drift from a fact whose identity moved out from under it
	//     (e.g. UpdateObservation reassigning scope/topic_key; see C2) even if
	//     the index entry itself was never explicitly purged.
	// Either way, AddObservation self-heals: falling through re-establishes a
	// fresh fact and topics.Put below overwrites the stale entry with it.
	if p.TopicKey != "" {
		if factID, ok := b.topics.Lookup(p.Project, p.Scope, p.TopicKey); ok {
			current, err := b.getFact(factID)
			if err != nil {
				return 0, err
			}
			if isValidTopicKeyHit(current, p) {
				return b.upsertTopicKeyFact(current, p)
			}
			// Stale/expired index entry: treat as a miss and fall through.
		}
	}

	convCustomID := p.SessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}

	// Stable engram-side observation id stamped into fact metadata so a fact
	// can be traced back to the message that produced it.
	obsID := contentHash(p.Content)

	// Snapshot the project's existing fact ids *before* appending this message
	// so BackfillFacts only claims facts that appear afterward. MemoryLake
	// extraction is asynchronous and bounded here by maxWait, so earlier saves
	// can leave unmarked facts behind; without this snapshot, this save would
	// claim one of those stale facts and overwrite its engram_raw with the
	// wrong observation's text (data corruption on read-back). See BackfillFacts.
	known, err := b.client.listFacts(b.ws, b.projID)
	if err != nil {
		return 0, err
	}
	knownFactIDs := make(map[string]bool, len(known))
	for _, f := range known {
		knownFactIDs[f.ID] = true
	}

	msgID, err := b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, p)
	if err != nil {
		return 0, err
	}

	md := FactMetadata(p, obsID, p.Content)
	facts, err := b.client.BackfillFacts(b.ws, b.projID, md, knownFactIDs, b.poll, b.maxWait)
	if err != nil {
		return 0, err
	}

	if len(facts) > 0 {
		var first int64
		for i, f := range facts {
			id := b.idmap.IntFor(f.ID)
			if i == 0 {
				first = id
			}
		}
		// Record the first backfilled fact as this topic_key's fact so the
		// next save with the same project+scope+topic_key upserts it instead
		// of appending another message.
		if p.TopicKey != "" {
			if err := b.topics.Put(p.Project, p.Scope, p.TopicKey, facts[0].ID); err != nil {
				return 0, err
			}
		}
		return first, nil
	}

	// Extraction pending: return a provisional id keyed off the message.
	provisional := msgID
	if provisional == "" {
		provisional = obsID
	}
	return b.idmap.IntFor(provisional), nil
}

// isValidTopicKeyHit reports whether fact f is actually eligible to be
// upserted as the project+scope+topic_key match for p — i.e. whether a
// TopicIndex hit resolving to f should be trusted (see AddObservation's C1
// hardening comment). A hit is valid only when:
//   - f has not been forgotten (soft-deleted): f.Expired == false, mirroring
//     internal/store's `deleted_at IS NULL` filter on its own topic_key match
//     query.
//   - f's own stamped scope/topic_key metadata (normalized) still agrees with
//     the scope/topic_key this lookup was made for (also normalized) — a
//     mismatch means the index entry has drifted from the fact's actual
//     current identity (e.g. after UpdateObservation reassigned it).
//
// Any other difference (title, type, content) is exactly what an upsert is
// *for* and does not affect validity.
func isValidTopicKeyHit(f Fact, p store.AddObservationParams) bool {
	if f.Expired {
		return false
	}
	if normalizeIndexScope(metaString(f.Metadata, metaScope)) != normalizeIndexScope(p.Scope) {
		return false
	}
	return normalizeIndexTopicKey(metaString(f.Metadata, metaTopicKey)) == normalizeIndexTopicKey(p.TopicKey)
}

// metaString reads a string-valued metadata key from a MemoryLake fact's
// metadata map, returning "" if the key is absent or not a string. Local to
// backend.go (not mapper.go, which task-12's hardening scope leaves
// untouched) — used only by isValidTopicKeyHit above.
func metaString(md map[string]any, key string) string {
	if v, ok := md[key].(string); ok {
		return v
	}
	return ""
}

// upsertTopicKeyFact implements the topic_key upsert hit path: PATCH the
// already-known (and already fetched + validated, see isValidTopicKeyHit)
// fact in place (new content + refreshed metadata) instead of appending a new
// conversation message and waiting on extraction. This mirrors
// internal/store's topic_key match: on a hit, the store's SQL UPDATEs
// type/title/content/topic_key and bumps revision_count on the existing row
// rather than inserting a new one (see store.go's AddObservation). scope is
// intentionally left untouched here for the same reason the store's SQL never
// assigns it on that branch: scope is part of what identified this fact as
// the match (via the TopicIndex key) in the first place, so it cannot differ
// from what's already stored. Any other metadata keys already on the fact
// (e.g. "pinned") are preserved verbatim.
//
// current is the fact AddObservation already fetched (and validated) for this
// hit — reusing it here avoids a second, redundant GET.
func (b *MemoryLakeBackend) upsertTopicKeyFact(current Fact, p store.AddObservationParams) (int64, error) {
	md := map[string]any{}
	for k, v := range current.Metadata {
		md[k] = v
	}
	md[metaRaw] = p.Content
	md[metaTitle] = p.Title
	md[metaType] = p.Type
	md[metaTopicKey] = p.TopicKey
	md[metaObsID] = contentHash(p.Content)
	md[metaRev] = revisionFromMetadata(current.Metadata) + 1

	if _, err := b.patchFact(current.ID, map[string]any{"fact": p.Content, "metadata": md}); err != nil {
		return 0, err
	}
	return b.idmap.IntFor(current.ID), nil
}

// GetObservation resolves the int64 id through the IDMap to a MemoryLake fact
// id, fetches the fact, and decodes it back into a store.Observation (Content
// preferring the verbatim engram_raw metadata over MemoryLake's paraphrase).
func (b *MemoryLakeBackend) GetObservation(id int64) (*store.Observation, error) {
	factID, ok := b.idmap.FactFor(id)
	if !ok {
		return nil, &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}
	f, err := b.getFact(factID)
	if err != nil {
		return nil, err
	}
	obs := ObservationFromFact(f)
	obs.ID = id
	obs.CreatedAt = f.CreatedAt
	obs.UpdatedAt = f.UpdatedAt
	return &obs, nil
}

// UpdateObservation merges the supplied fields into the fact's existing engram
// metadata and PATCHes the fact. When Content changes, the fact's own text is
// updated too (V3 FactUpdateRequest carries `content`) and engram_raw is kept in
// sync so future reads return the verbatim edited text.
func (b *MemoryLakeBackend) UpdateObservation(id int64, p store.UpdateObservationParams) (*store.Observation, error) {
	factID, ok := b.idmap.FactFor(id)
	if !ok {
		return nil, &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}

	current, err := b.getFact(factID)
	if err != nil {
		return nil, err
	}

	// Copy existing metadata so preserved keys (e.g. engram_raw for unchanged
	// content, engram_obs_id) survive the PATCH.
	md := map[string]any{}
	for k, v := range current.Metadata {
		md[k] = v
	}
	if p.Title != nil {
		md[metaTitle] = *p.Title
	}
	if p.Type != nil {
		md[metaType] = *p.Type
	}
	if p.Scope != nil {
		md[metaScope] = *p.Scope
	}
	if p.TopicKey != nil {
		md[metaTopicKey] = *p.TopicKey
	}

	fields := map[string]any{"metadata": md}
	if p.Content != nil {
		md[metaRaw] = *p.Content
		// V3 fact update body field is `fact` (same field name used on read;
		// FactUpdateRequest has only `fact` + `metadata`, no `content`).
		fields["fact"] = *p.Content
	}

	updated, err := b.patchFact(factID, fields)
	if err != nil {
		return nil, err
	}

	// If this update changed the fact's scope and/or topic_key, its
	// project+scope+topic_key identity may no longer match whatever
	// project+scope+topic_key the TopicIndex filed it under (that project
	// isn't itself recoverable from fact metadata — see RemoveByFactID's doc
	// comment — so the exact old key can't be recomputed here). Purge any
	// entry pointing at this fact so a later save using the OLD
	// scope/topic_key can never upsert-PATCH into this now-reassigned fact; a
	// save using the NEW scope/topic_key simply misses and re-establishes its
	// own entry via the normal append path (task-12 hardening brief C2). This
	// purge is best-effort for the same reason as DeleteObservation's: the
	// PATCH above already succeeded, and isValidTopicKeyHit's scope/topic_key
	// comparison (C1) independently self-heals a stale pointer even if this
	// purge fails to persist.
	if p.Scope != nil || p.TopicKey != nil {
		if _, rmErr := b.topics.RemoveByFactID(factID); rmErr != nil {
			log.Printf("[memorylake] UpdateObservation: failed to purge stale TopicIndex entry for fact %s: %v", factID, rmErr)
		}
	}

	obs := ObservationFromFact(updated)
	obs.ID = id
	obs.CreatedAt = updated.CreatedAt
	obs.UpdatedAt = updated.UpdatedAt
	return &obs, nil
}

// DeleteObservation soft-deletes by calling MemoryLake's forget endpoint.
// MemoryLake has no hard delete — forget marks the fact expired while retaining
// it for audit — so the hardDelete flag is intentionally ignored: a hard-delete
// request degrades to a forget. An id with no fact mapping is a no-op (nil).
//
// Once the fact is forgotten, any TopicIndex entry pointing at it is purged
// (see RemoveByFactID) so a later save with the same project+scope+topic_key
// can never upsert-PATCH a forgotten fact back into visibility. isValidTopicKeyHit's
// Expired check (C1) already prevents that outcome on its own even if this
// purge never ran; removing the pointer here just means most later saves
// avoid paying for an extra GET+append round trip (task-12 hardening brief
// C2). The purge is best-effort: it runs after forgetFact has already
// succeeded, and a failure to persist it does not resurface the C1 data-loss
// bug (self-healed on the next hit), so it is logged rather than returned as
// this call's error.
func (b *MemoryLakeBackend) DeleteObservation(id int64, hardDelete bool) error {
	_ = hardDelete // MemoryLake only supports soft delete (forget); see doc comment.
	factID, ok := b.idmap.FactFor(id)
	if !ok {
		return nil
	}
	if err := b.forgetFact(factID); err != nil {
		return err
	}
	if _, err := b.topics.RemoveByFactID(factID); err != nil {
		log.Printf("[memorylake] DeleteObservation: failed to purge stale TopicIndex entry for fact %s: %v", factID, err)
	}
	return nil
}

// Search delegates to SearchFacts (Task 8), which handles semantic search plus
// the topic_key fuzzy fast-path and client-side type/scope filtering.
func (b *MemoryLakeBackend) Search(query string, opts store.SearchOptions) ([]store.SearchResult, error) {
	return b.client.SearchFacts(b.ws, b.projID, b.actorID, query, opts)
}

// MaxObservationLength returns the same content ceiling as the local store so
// truncation behavior is backend-independent.
func (b *MemoryLakeBackend) MaxObservationLength() int {
	return maxObservationLength
}

// PinObservation sets the pinned flag in the fact's metadata.
func (b *MemoryLakeBackend) PinObservation(id int64) error {
	return b.setPinned(id, true)
}

// UnpinObservation clears the pinned flag in the fact's metadata.
func (b *MemoryLakeBackend) UnpinObservation(id int64) error {
	return b.setPinned(id, false)
}

// setPinned reads the fact, merges pinned=<v> into its metadata, and PATCHes it.
func (b *MemoryLakeBackend) setPinned(id int64, pinned bool) error {
	factID, ok := b.idmap.FactFor(id)
	if !ok {
		return &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}
	current, err := b.getFact(factID)
	if err != nil {
		return err
	}
	md := map[string]any{}
	for k, v := range current.Metadata {
		md[k] = v
	}
	md["pinned"] = pinned
	_, err = b.client.patchFactMetadata(b.ws, b.projID, factID, md)
	return err
}

// ─── Tier B: first-cut implementations (see task-9 report for TODOs) ─────────

// Timeline lists the project's facts, orders them by created_at, and returns
// the N facts before/after the anchor observation.
//
// First-cut: session grouping and prompts are not modeled (facts carry no
// engram session id). SessionInfo is left nil. TODO(spec §6): richer timeline
// fidelity once fact metadata carries session linkage.
func (b *MemoryLakeBackend) Timeline(observationID int64, before, after int) (*store.TimelineResult, error) {
	if before <= 0 {
		before = 5
	}
	if after <= 0 {
		after = 5
	}

	anchorFactID, ok := b.idmap.FactFor(observationID)
	if !ok {
		return nil, &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}

	allFacts, err := b.client.listAllFacts(b.ws, b.projID)
	if err != nil {
		return nil, err
	}

	// Exclude expired (soft-deleted) facts from the timeline window, mirroring
	// the local store's `deleted_at IS NULL` filter on before/after — except
	// the anchor itself, which must stay locatable even if it has since
	// expired (the caller explicitly asked for this observation's timeline).
	facts := make([]Fact, 0, len(allFacts))
	for _, f := range allFacts {
		if f.ID == anchorFactID || !f.Expired {
			facts = append(facts, f)
		}
	}
	// Chronological order (created_at ascending); fall back to id for stability.
	sortFactsByCreatedAsc(facts)

	anchorIdx := -1
	for i, f := range facts {
		if f.ID == anchorFactID {
			anchorIdx = i
			break
		}
	}
	if anchorIdx < 0 {
		return nil, &APIError{Code: "NOT_FOUND", Message: "anchor fact not found in project fact list"}
	}

	toEntry := func(f Fact) store.TimelineEntry {
		o := ObservationFromFact(f)
		o.ID = b.idmap.IntFor(f.ID)
		return store.TimelineEntry{
			ID:            o.ID,
			Type:          o.Type,
			Title:         o.Title,
			Content:       o.Content,
			Scope:         o.Scope,
			TopicKey:      o.TopicKey,
			RevisionCount: o.RevisionCount,
			CreatedAt:     f.CreatedAt,
			UpdatedAt:     f.UpdatedAt,
		}
	}

	res := &store.TimelineResult{}
	focus := ObservationFromFact(facts[anchorIdx])
	focus.ID = observationID
	focus.CreatedAt = facts[anchorIdx].CreatedAt
	focus.UpdatedAt = facts[anchorIdx].UpdatedAt
	res.Focus = focus

	start := anchorIdx - before
	if start < 0 {
		start = 0
	}
	for i := start; i < anchorIdx; i++ {
		res.Before = append(res.Before, toEntry(facts[i]))
	}
	end := anchorIdx + after
	if end >= len(facts) {
		end = len(facts) - 1
	}
	for i := anchorIdx + 1; i <= end; i++ {
		res.After = append(res.After, toEntry(facts[i]))
	}
	res.TotalInRange = len(res.Before) + 1 + len(res.After)
	return res, nil
}

// maxFormatContextRecent bounds how many non-pinned facts FormatContext
// includes, mirroring the local store's cfg.MaxContextResults ceiling (a
// fixed reasonable default here since this backend has no equivalent config
// field).
const maxFormatContextRecent = 20

// formatContextContentTruncateLen mirrors internal/store's FormatContext,
// which truncates every observation's content to 300 runes (via its
// unexported truncate helper) before rendering pinned/recent lines — see
// internal/store/store.go's FormatContext. internal/store does not export
// that helper, so truncate below replicates it so MemoryLake-backed projects
// produce the same shape of context block as SQLite-backed ones (task-12
// hardening brief I3).
const formatContextContentTruncateLen = 300

// truncate mirrors internal/store's unexported truncate(s, max): a rune-safe
// cut to at most max runes, with a literal "..." appended when s was longer.
// Copied here (not exported by internal/store) — see
// internal/store/store.go:truncate. Keep in sync if that rule ever changes.
func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// FormatContext renders a human-readable text block of the project's facts,
// optionally filtered by scope, with pinned facts (metadata["pinned"] == true)
// listed ahead of the most recent unpinned ones — the same priority order as
// the local store's FormatContext (pinned section before recent-observations
// section).
//
// project is accepted for signature compatibility but ignored: a backend
// instance is already bound to a single MemoryLake project (see
// CountObservationsForProject).
//
// First-cut: no session/prompt sections (see CreateSession/AddPrompt doc
// comments — MemoryLake has no session or prompt tracking in this backend).
// TODO(spec §6): fold in sessions/prompts if/when they gain a MemoryLake
// analogue.
func (b *MemoryLakeBackend) FormatContext(project, scope string) (string, error) {
	_ = project
	allFacts, err := b.client.listAllFacts(b.ws, b.projID)
	if err != nil {
		return "", err
	}

	active := make([]Fact, 0, len(allFacts))
	for _, f := range allFacts {
		if !f.Expired {
			active = append(active, f)
		}
	}
	sortFactsByCreatedAsc(active)

	var pinned, recent []Fact
	for _, f := range active {
		if scope != "" && ObservationFromFact(f).Scope != scope {
			continue
		}
		if isPinned(f) {
			pinned = append(pinned, f)
		} else {
			recent = append(recent, f)
		}
	}
	// Most-recent-first within each group.
	reverseFacts(pinned)
	reverseFacts(recent)
	if len(recent) > maxFormatContextRecent {
		recent = recent[:maxFormatContextRecent]
	}

	if len(pinned) == 0 && len(recent) == 0 {
		return "", nil
	}

	var out strings.Builder
	out.WriteString("## Memory Context (MemoryLake)\n\n")

	if len(pinned) > 0 {
		out.WriteString("### Pinned\n")
		for _, f := range pinned {
			o := ObservationFromFact(f)
			fmt.Fprintf(&out, "- [%s] **%s**: %s\n", o.Type, o.Title, truncate(o.Content, formatContextContentTruncateLen))
		}
		out.WriteString("\n")
	}

	if len(recent) > 0 {
		out.WriteString("### Recent Observations\n")
		for _, f := range recent {
			o := ObservationFromFact(f)
			fmt.Fprintf(&out, "- [%s] **%s**: %s\n", o.Type, o.Title, truncate(o.Content, formatContextContentTruncateLen))
		}
		out.WriteString("\n")
	}

	return out.String(), nil
}

// isPinned reports whether a fact's metadata carries the pinned flag set by
// PinObservation.
func isPinned(f Fact) bool {
	p, _ := f.Metadata["pinned"].(bool)
	return p
}

// reverseFacts reverses facts in place.
func reverseFacts(facts []Fact) {
	for i, j := 0, len(facts)-1; i < j; i, j = i+1, j-1 {
		facts[i], facts[j] = facts[j], facts[i]
	}
}

// Stats reports observation counts from the project's full fact list
// (excluding expired/forgotten facts, mirroring the store's `deleted_at IS
// NULL` exclusion), plus the workspace's project names.
//
// First-cut: sessions and prompts have no MemoryLake-tracked analogue in this
// backend (see CreateSession/AddPrompt doc comments), so those counts stay 0
// rather than an invented value. If listing project names fails, Stats still
// succeeds with Projects left nil — mirroring the local store's Stats, which
// likewise treats project-listing failure as non-fatal.
func (b *MemoryLakeBackend) Stats() (*store.Stats, error) {
	facts, err := b.client.listAllFacts(b.ws, b.projID)
	if err != nil {
		return nil, err
	}
	stats := &store.Stats{TotalObservations: countActive(facts)}
	if names, err := b.ListProjectNames(); err == nil {
		stats.Projects = names
	}
	return stats, nil
}

// countActive counts facts that are not expired (MemoryLake's soft-delete
// flag, the analogue of the store's deleted_at IS NULL exclusion).
func countActive(facts []Fact) int {
	n := 0
	for _, f := range facts {
		if !f.Expired {
			n++
		}
	}
	return n
}

// CountObservationsForProject counts non-expired facts in the backend's
// project. The name argument is accepted for signature compatibility but
// ignored: a backend instance is bound to a single MemoryLake project.
func (b *MemoryLakeBackend) CountObservationsForProject(name string) (int, error) {
	facts, err := b.client.listAllFacts(b.ws, b.projID)
	if err != nil {
		return 0, err
	}
	return countActive(facts), nil
}

// ProjectExists checks name against the workspace's project list (matched by
// custom_id or display name), the same way EnsureProject resolves a project
// reference. Unlike the local store (which recognizes a project from any of
// observations/sessions/prompts/enrollment), MemoryLake's only durable
// project registry is this list, so that's the sole source of truth here.
// Uses listAllProjects (see identity.go) to follow continuation_token
// across pages rather than only the first, so a workspace with many
// projects still gets a correct answer.
func (b *MemoryLakeBackend) ProjectExists(name string) (bool, error) {
	items, err := b.client.listAllProjects(b.ws)
	if err != nil {
		return false, err
	}
	for _, p := range items {
		if p.CustomID == name || p.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ListProjectNames returns the custom_id (falling back to display name when
// custom_id is empty) of every project in the workspace, following
// continuation_token across pages via listAllProjects (see identity.go and
// ProjectExists above).
func (b *MemoryLakeBackend) ListProjectNames() ([]string, error) {
	items, err := b.client.listAllProjects(b.ws)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, p := range items {
		if p.CustomID != "" {
			names = append(names, p.CustomID)
		} else {
			names = append(names, p.Name)
		}
	}
	return names, nil
}

// ─── Tier B: sessions ─────────────────────────────────────────────────────────
//
// Session lifecycle (project/directory/started_at/ended_at/summary) is
// tracked in the local SessionIndex sidecar rather than on the MemoryLake
// conversation object itself — see SessionIndex's doc comment for why.
// CreateSession is the one operation that also performs a real, tested
// MemoryLake write (ensuring the session's conversation exists); the other
// four operations below read/write the sidecar only.

// CreateSession ensures a MemoryLake conversation keyed by the session id
// (the one part of "session ↔ conversation" backed by a real, tested
// MemoryLake write) and records the session's lifecycle fields in the local
// SessionIndex sidecar (see its doc comment for why those fields can't live
// on the conversation object itself).
func (b *MemoryLakeBackend) CreateSession(id, project, directory string) error {
	project, _ = store.NormalizeProject(project)
	if _, err := b.client.ensureConversation(b.ws, b.projID, id, b.actorID); err != nil {
		return err
	}
	startedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	return b.sessions.Create(id, project, directory, startedAt)
}

// GetSession reads the session's lifecycle fields from the local
// SessionIndex sidecar. Returns a NOT_FOUND APIError (rather than nil,nil)
// when id was never recorded, mirroring internal/store's GetSession
// returning a non-nil error on sql.ErrNoRows — callers only branch on
// err != nil (see internal/mcp's resolveSaveWriteProject), not on a specific
// sentinel, so any descriptive error satisfies that contract.
func (b *MemoryLakeBackend) GetSession(id string) (*store.Session, error) {
	rec, ok := b.sessions.Get(id)
	if !ok {
		return nil, &APIError{Code: "NOT_FOUND", Message: "no session recorded for id " + id}
	}
	return &store.Session{
		ID:        id,
		Project:   rec.Project,
		Directory: rec.Directory,
		StartedAt: rec.StartedAt,
		EndedAt:   rec.EndedAt,
		Summary:   rec.Summary,
	}, nil
}

// EndSession records the session as ended with the given summary in the
// local SessionIndex sidecar (a no-op, mirroring store.go's EndSession, when
// id was never recorded), and — best-effort — also appends the summary as a
// conversation message so MemoryLake retains a durable trace of the session
// end. MemoryLake's conversation object has no confirmed "end"/"summary"
// field to PATCH (see SessionIndex's doc comment), so a message is the only
// tested write surface available for this; a failure appending it does not
// fail EndSession, since the authoritative lifecycle record is the
// SessionIndex sidecar, already persisted by the time the append is
// attempted.
func (b *MemoryLakeBackend) EndSession(id string, summary string) error {
	if _, ok := b.sessions.Get(id); !ok {
		return nil
	}
	endedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := b.sessions.End(id, endedAt, summary); err != nil {
		return err
	}
	if summary != "" {
		if _, err := b.client.AppendObservation(b.ws, b.projID, id, b.actorID, store.AddObservationParams{
			SessionID: id,
			Type:      "session_summary",
			Title:     "Session summary",
			Content:   summary,
		}); err != nil {
			log.Printf("[memorylake] EndSession: failed to append summary message for session %s: %v", id, err)
		}
	}
	return nil
}

// MostRecentActiveSession resolves the most recently started, not-yet-ended
// session for project from the local SessionIndex sidecar. See
// SessionIndex.MostRecentActive's doc comment for the selection rules
// mirrored from internal/store.
func (b *MemoryLakeBackend) MostRecentActiveSession(project string) (string, bool, error) {
	project, _ = store.NormalizeProject(project)
	if project == "" {
		return "", false, nil
	}
	id, ok := b.sessions.MostRecentActive(project)
	return id, ok, nil
}

// RecentSessions lists up to limit recent sessions for project from the
// local SessionIndex sidecar. See SessionIndex.Recent's doc comment for the
// ordering and for why ObservationCount is always 0 in this backend.
func (b *MemoryLakeBackend) RecentSessions(project string, limit int) ([]store.SessionSummary, error) {
	project, _ = store.NormalizeProject(project)
	if limit <= 0 {
		limit = 5 // mirrors store.RecentSessions' own default, store.go:2123.
	}
	return b.sessions.Recent(project, limit), nil
}

// AddPrompt, AddPromptIfMissing (see prompts.go) and PassiveCapture (see
// passive.go) and ObservationsNeedingReview/MarkReviewed (see review.go) are
// implemented in their own files.

// ─── Tier B: projects ─────────────────────────────────────────────────────────

// MergeProjects is unsupported: MemoryLake owns project identity and engram
// must not silently migrate facts across MemoryLake projects.
func (b *MemoryLakeBackend) MergeProjects(sources []string, canonical string) (*store.MergeResult, error) {
	return nil, errors.New("MemoryLake backend does not support merging projects")
}

// FindCandidates, GetRelationsForObservations, JudgeRelation and
// JudgeBySemantic (the relation/conflict surface) are implemented in
// conflict.go, mapped onto the V3 memory-conflict API (task 14).

// ─── Fact HTTP helpers ───────────────────────────────────────────────────────

// getFact fetches a single fact by id.
func (b *MemoryLakeBackend) getFact(factID string) (Fact, error) {
	var f Fact
	path := "/api/v3/workspaces/" + b.ws + "/projects/" + b.projID + "/memories/facts/" + factID
	if err := b.client.doJSON("GET", path, nil, &f); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// patchFact PATCHes arbitrary fields (metadata and/or fact) onto a fact and
// returns the updated fact MemoryLake echoes back.
func (b *MemoryLakeBackend) patchFact(factID string, fields map[string]any) (Fact, error) {
	var f Fact
	path := "/api/v3/workspaces/" + b.ws + "/projects/" + b.projID + "/memories/facts/" + factID
	if err := b.client.doJSON("PATCH", path, fields, &f); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// forgetFact soft-deletes (forgets) a fact.
func (b *MemoryLakeBackend) forgetFact(factID string) error {
	path := "/api/v3/workspaces/" + b.ws + "/projects/" + b.projID + "/memories/facts/" + factID + "/forget"
	return b.client.doJSON("POST", path, nil, nil)
}

// sortFactsByCreatedAsc orders facts by created_at ascending, falling back to
// id for stable ordering when timestamps tie or are empty.
func sortFactsByCreatedAsc(facts []Fact) {
	sort.SliceStable(facts, func(i, j int) bool {
		if facts[i].CreatedAt != facts[j].CreatedAt {
			return facts[i].CreatedAt < facts[j].CreatedAt
		}
		return facts[i].ID < facts[j].ID
	})
}
