package memorylake

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
//
// Option A thin adapter (Phase 3, see docs/superpowers/specs/2026-07-23-
// memorylake-thin-adapter-design.md): this type used to also own an IDMap
// (process-global int64↔fact-id registry) and a TopicIndex (project+scope+
// topic_key → fact-id cache backing a synchronous upsert path), both driving
// a synchronous snapshot→append→backfill write sequence in AddObservation.
// Both are retired: by-id methods take the MemoryLake fact id directly as
// their string sync_id (no id-mapping layer needed — a fact id is already
// globally unique), and AddObservation no longer waits on or claims
// extracted facts at all (mem0's own pipeline owns dedup/upsert/conflict
// merging asynchronously, off the request path — see AddObservation's doc
// comment).
type MemoryLakeBackend struct {
	client  *Client
	cfg     Config
	ws      string // resolved workspace id ("ws-...")
	projID  string // resolved MemoryLake project id
	actorID string // resolved MemoryLake actor id
	// sessions is the local sidecar recording session lifecycle fields
	// (project/directory/started_at/ended_at/summary) that have no confirmed
	// analogue on a MemoryLake conversation object — see SessionIndex's doc
	// comment for why this can't just be a GET on the conversation itself.
	sessions *SessionIndex

	// writeMu serializes AddObservation/AddPrompt/AddPromptIfMissing for this
	// backend instance. With the sync backfill retired, the write path itself
	// (ensureConversation + append) has no cross-goroutine hazard of its own —
	// this mutex remains mainly so the in-process prompt/passive dedup caches
	// below (a check-then-append-then-record sequence) are observed
	// atomically rather than racing two concurrent identical saves into a
	// double append. Because routing caches a single backend instance per
	// project, this mutex is naturally per-project: distinct projects hold
	// distinct instances and never contend. Read paths (Get/Search/Timeline/
	// ...) are deliberately NOT guarded — only the write path needs ordering.
	writeMu sync.Mutex

	// promptMu guards promptIDs: an in-process, non-persisted cache mapping
	// AddPromptIfMissing's dedup key (see prompts.go's promptDedupKey) to the
	// sync_id (MemoryLake message id) returned for it, so a second call with
	// byte-identical session_id+project+content within this backend's
	// lifetime skips the MemoryLake round trip entirely and reports
	// inserted=false. This replaces the retired process-global IDMap's
	// IntFor/IntIfExists, which served the same purpose across process
	// restarts (persisted to disk); this cache does not persist and resets
	// per process, which is acceptable since MemoryLake's own message
	// idempotency (content-hash custom_id, see AppendObservation) already
	// guarantees no duplicate message is created even on a cache miss — this
	// is purely a network-round-trip optimization, not a correctness
	// requirement.
	promptMu  sync.Mutex
	promptIDs map[string]string

	// passiveMu guards passiveSeen: the PassiveCapture analogue of promptIDs
	// above (see passive.go's passiveDedupKey) — an in-process, non-persisted
	// set of already-saved learning dedup keys for this backend's lifetime.
	passiveMu   sync.Mutex
	passiveSeen map[string]bool
}

// NewBackend constructs a MemoryLakeBackend for the given workspace reference
// (custom_id, name, or "ws-" id) and already-resolved MemoryLake project id. It
// resolves the workspace id and ensures a HUMAN actor exists (keyed by
// cfg.Actor, falling back to the machine hostname).
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
		sessions: sessions,
	}, nil
}

// ─── Observation CRUD (Tier A: core) ────────────────────────────────────────

// AddObservation appends the observation's content as a MemoryLake
// conversation message and returns immediately — it does not wait for, or
// attempt to claim, any fact MemoryLake's own extraction pipeline (mem0)
// eventually produces from that message.
//
// This is the Option A thin-adapter write path (spec §2/§4): mem0's own
// pipeline (vector+BM25 candidate recall, LLM ADD/UPDATE/NOOP decision,
// conflict-aware merge) already does everything the retired synchronous
// snapshot→append→backfill sequence existed to approximate locally —
// deduplication, topic_key-style upsert, and content management are its job
// now, not this adapter's. Because that decision runs asynchronously and
// off this request's path, AddObservation cannot report the resulting fact's
// real id: the sync_id it returns is a PENDING reference (the MemoryLake
// message id this call appended, or a content-hash fallback if MemoryLake's
// response carried no id) — not a fact id, and not guaranteed to resolve via
// GetObservation/UpdateObservation/DeleteObservation/PinObservation. A
// caller that needs the materialized fact (and its real fact-id sync_id)
// must re-find it later via Search once mem0 has processed the message.
func (b *MemoryLakeBackend) AddObservation(p store.AddObservationParams) (string, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	text := factText(p.Title, p.Content)
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("cannot save an observation with empty content")
	}
	created, err := b.client.AddFacts(b.ws, b.projID, []string{text})
	if err != nil {
		return "", err
	}
	if len(created) > 0 && strings.TrimSpace(created[0].ID) != "" {
		return created[0].ID, nil
	}
	// The endpoint accepted the write but echoed no id back (defensive — the
	// real API always returns one). Fall back to a stable, content-derived
	// reference rather than an empty sync_id.
	return contentHash(text), nil
}

// factText renders an observation's (title, content) into the single verbatim
// string stored as a MemoryLake fact via the direct fact-add endpoint. The
// title is preserved by prepending it to the content when it adds information
// (i.e. it is non-empty and not already contained in the content); otherwise
// whichever of content/title is present is used as-is.
func factText(title, content string) string {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	switch {
	case title == "":
		return content
	case content == "":
		return title
	case strings.Contains(content, title):
		return content
	default:
		return title + "\n\n" + content
	}
}

// metaString reads a string-valued metadata key from a MemoryLake fact's
// metadata map, returning "" if the key is absent or not a string. Used by
// review.go's decay computation.
func metaString(md map[string]any, key string) string {
	if v, ok := md[key].(string); ok {
		return v
	}
	return ""
}

// GetObservation fetches the MemoryLake fact identified by syncID (which, for
// this backend, simply *is* the MemoryLake fact id — see this type's doc
// comment) and decodes it into a store.Observation.
func (b *MemoryLakeBackend) GetObservation(syncID string) (*store.Observation, error) {
	f, err := b.getFact(syncID)
	if err != nil {
		return nil, err
	}
	obs := ObservationFromFact(f)
	obs.CreatedAt = f.CreatedAt
	obs.UpdatedAt = f.UpdatedAt
	return &obs, nil
}

// UpdateObservation merges the supplied fields into the fact's existing engram
// metadata and PATCHes the fact. This is an explicit, user-directed edit (see
// spec §5.5: "仍允许显式 PATCH, 但正确姿势是再 save 一次让 mem0 合并") — mem0 may
// still independently re-merge this fact later; Engram does not attempt to
// prevent that.
func (b *MemoryLakeBackend) UpdateObservation(syncID string, p store.UpdateObservationParams) (*store.Observation, error) {
	current, err := b.getFact(syncID)
	if err != nil {
		return nil, err
	}

	// Copy existing metadata so preserved keys (e.g. "pinned") survive the PATCH.
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
		// V3 fact update body field is `fact` (same field name used on read;
		// FactUpdateRequest has only `fact` + `metadata`, no `content`).
		fields["fact"] = *p.Content
	}

	updated, err := b.patchFact(syncID, fields)
	if err != nil {
		return nil, err
	}

	obs := ObservationFromFact(updated)
	obs.CreatedAt = updated.CreatedAt
	obs.UpdatedAt = updated.UpdatedAt
	return &obs, nil
}

// DeleteObservation soft-deletes by calling MemoryLake's forget endpoint.
// MemoryLake has no hard delete — forget marks the fact expired while retaining
// it for audit — so the hardDelete flag is intentionally ignored: a hard-delete
// request degrades to a forget.
func (b *MemoryLakeBackend) DeleteObservation(syncID string, hardDelete bool) error {
	_ = hardDelete // MemoryLake only supports soft delete (forget); see doc comment.
	return b.forgetFact(syncID)
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
func (b *MemoryLakeBackend) PinObservation(syncID string) error {
	return b.setPinned(syncID, true)
}

// UnpinObservation clears the pinned flag in the fact's metadata.
func (b *MemoryLakeBackend) UnpinObservation(syncID string) error {
	return b.setPinned(syncID, false)
}

// setPinned reads the fact, merges pinned=<v> into its metadata, and PATCHes it.
func (b *MemoryLakeBackend) setPinned(factID string, pinned bool) error {
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
// engram session id). SessionInfo is left nil. Every returned entry's
// (legacy, int64) ID field is left at zero — see this type's doc comment on
// why sync_id (a MemoryLake fact id string) is the only identifier this
// backend can offer; store.TimelineEntry has no sync_id field to carry it,
// so the timeline text these entries feed still displays content correctly
// but not a usable per-entry id. TODO(spec §6): richer timeline fidelity
// once fact metadata carries session linkage.
func (b *MemoryLakeBackend) Timeline(syncID string, before, after int) (*store.TimelineResult, error) {
	if before <= 0 {
		before = 5
	}
	if after <= 0 {
		after = 5
	}

	anchorFactID := syncID

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
		return store.TimelineEntry{
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
// considers, mirroring the local store's cfg.MaxContextResults ceiling (a
// fixed reasonable default here since this backend has no equivalent config
// field). The layered layout renders at most contextRecentObs of them.
const maxFormatContextRecent = 20

// contextPinnedTruncLen mirrors internal/store's FormatContext, which
// truncates a *pinned* observation's content to 300 runes (via its unexported
// truncate helper, spelled with this same name) before rendering the pinned
// lines — see internal/store/store.go's FormatContext. internal/store does not
// export that helper, so truncate below replicates it so MemoryLake-backed
// projects produce the same shape of context block as SQLite-backed ones
// (task-12 hardening brief I3). Recent-observation lines use firstSentence on
// both backends instead.
//
// The name is deliberately identical to internal/store's constant so a grep for
// contextPinnedTruncLen surfaces both copies of the rule at once.
const contextPinnedTruncLen = 300

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

// Layered mem_context contract (Phase 2 spec R3b) — a byte-for-byte mirror of
// internal/store's constants of the same names (see internal/store/store.go).
// internal/memorylake does import internal/store, but these are deliberately
// unexported there (presentation detail of the local backend, not part of its
// API), so they are duplicated here the same way truncate is. Keep in sync.
const (
	contextByteCap         = 1600
	contextRecentObs       = 3
	contextSummaryTruncLen = 160
)

// contextFooter closes every context block with the expand-on-demand pointer
// (identical string to internal/store's contextFooter).
const contextFooter = "Full bodies: mem_search / mem_get_observation.\n"

// firstSentence returns content up to the first sentence terminator, capped
// at contextSummaryTruncLen on a rune boundary. Deterministic — key entities in
// the opening sentence always survive (Phase 2 spec R3 rule).
//
// A '.' only terminates when it is followed by whitespace/end-of-string and is
// not flanked by digits, so decimals survive:
// "PostgreSQL 15.3 is required. More." → "PostgreSQL 15.3 is required."
// The other terminators (!?。！？ and \n) always terminate.
//
// Known tradeoff (ACCEPTED): abbreviations whose '.' is followed by a space
// still terminate — "e.g. use flags. Done." → "e.g." — because the guard is
// deliberately context-free (digit + whitespace rules only). We do NOT carry an
// abbreviation dictionary; the summary is a scanning aid, not prose.
//
// Canonical twin: internal/mcp.firstSentence (internal/mcp/search_index.go),
// whose rune cap is spelled searchSummaryMaxRunes (same value, 160);
// internal/store carries the third copy. internal/memorylake must not import
// internal/mcp (import cycle), so this duplication mirrors truncate above. Keep
// the three copies behaviourally identical.
func firstSentence(content string) string {
	c := strings.TrimSpace(content)
	runes := []rune(c)
	for i, r := range runes {
		if r == '!' || r == '?' || r == '\n' || r == '。' || r == '！' || r == '？' {
			c = string(runes[:i+1])
			break
		}
		if r != '.' {
			continue
		}
		// Must be followed by whitespace or end-of-string. This alone keeps
		// decimals intact ("15.3" — the '3' is not whitespace).
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		// Must not be flanked by digits, looking past the whitespace, so a
		// spaced decimal/enumeration ("15. 3") is not a sentence end either.
		if i > 0 && unicode.IsDigit(runes[i-1]) && unicode.IsDigit(nextNonSpace(runes[i+1:])) {
			continue
		}
		c = string(runes[:i+1])
		break
	}
	if utf8.RuneCountInString(c) > contextSummaryTruncLen {
		runes := []rune(c)
		c = string(runes[:contextSummaryTruncLen]) + "…"
	}
	return strings.TrimSpace(c)
}

// nextNonSpace returns the first non-whitespace rune of runes, or 0 when there
// is none (0 is not a digit, so callers treat "nothing follows" as not-a-digit).
func nextNonSpace(runes []rune) rune {
	for _, r := range runes {
		if !unicode.IsSpace(r) {
			return r
		}
	}
	return 0
}

// renderLayeredContext assembles the layered block and enforces
// contextByteCap by dropping the oldest observation line (this backend has no
// session lines — see FormatContext). Pinned lines are never dropped. Mirrors
// internal/store's renderLayeredContext.
func renderLayeredContext(pinnedLines, obsLines []string) string {
	for {
		out := joinLayeredContext(pinnedLines, obsLines)
		if len(out) <= contextByteCap {
			return out
		}
		if n := len(obsLines); n > 0 {
			obsLines = obsLines[:n-1]
			continue
		}
		return out
	}
}

func joinLayeredContext(pinnedLines, obsLines []string) string {
	var b strings.Builder
	b.WriteString("## Memory Context\n\n")
	writeContextSection(&b, "### Pinned\n", pinnedLines)
	writeContextSection(&b, "### Recent Observations\n", obsLines)
	b.WriteString(contextFooter)
	return b.String()
}

func writeContextSection(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	b.WriteString(heading)
	for _, line := range lines {
		b.WriteString(line)
	}
	b.WriteString("\n")
}

// FormatContext renders the layered context block of the project's facts,
// optionally filtered by scope: pinned facts (metadata["pinned"] == true)
// first, then up to contextRecentObs first-sentence summaries of the most
// recent unpinned facts, then the expand-on-demand footer — the same layout
// (and the same contextByteCap budget) as the local store's FormatContext.
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

	pinnedLines := make([]string, 0, len(pinned))
	for _, f := range pinned {
		o := ObservationFromFact(f)
		pinnedLines = append(pinnedLines, fmt.Sprintf("- [%s] **%s**: %s\n",
			o.Type, o.Title, truncate(o.Content, contextPinnedTruncLen)))
	}

	if len(recent) > contextRecentObs {
		recent = recent[:contextRecentObs]
	}
	obsLines := make([]string, 0, len(recent))
	for _, f := range recent {
		o := ObservationFromFact(f)
		obsLines = append(obsLines, fmt.Sprintf("- [%s] **%s**: %s\n",
			o.Type, o.Title, firstSentence(o.Content)))
	}

	return renderLayeredContext(pinnedLines, obsLines), nil
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
// id was never recorded), and — best-effort — also writes the summary as a
// verbatim MemoryLake fact (via the direct fact-add endpoint) so the session
// wrap-up is a durable, immediately-searchable memory rather than a raw
// conversation message subject to asynchronous mem0 extraction. A summary is
// memory-like, so unlike prompts (which stay on the conversation-append path
// to avoid polluting fact search) it is stored as a first-class fact. A
// failure writing it does not fail EndSession, since the authoritative
// lifecycle record is the SessionIndex sidecar, already persisted by the time
// the write is attempted.
func (b *MemoryLakeBackend) EndSession(id string, summary string) error {
	if _, ok := b.sessions.Get(id); !ok {
		return nil
	}
	endedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if err := b.sessions.End(id, endedAt, summary); err != nil {
		return err
	}
	if summary != "" {
		text := factText("Session summary", summary)
		if _, err := b.client.AddFacts(b.ws, b.projID, []string{text}); err != nil {
			fmt.Fprintf(os.Stderr, "[memorylake] EndSession: failed to write summary fact for session %s: %v\n", id, err)
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
