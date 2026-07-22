package memorylake

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
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
	poll    time.Duration
	maxWait time.Duration
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

	return &MemoryLakeBackend{
		client:  client,
		cfg:     cfg,
		ws:      wsID,
		projID:  projID,
		actorID: actorID,
		idmap:   idmap,
		poll:    time.Duration(cfg.ExtractPollMS) * time.Millisecond,
		maxWait: time.Duration(cfg.ExtractMaxWaitMS) * time.Millisecond,
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
	convCustomID := p.SessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}

	// Stable engram-side observation id stamped into fact metadata so a fact
	// can be traced back to the message that produced it.
	obsID := contentHash(p.Content)

	msgID, err := b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, p)
	if err != nil {
		return 0, err
	}

	md := FactMetadata(p, obsID, p.Content)
	facts, err := b.client.BackfillFacts(b.ws, b.projID, md, b.poll, b.maxWait)
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
		return first, nil
	}

	// Extraction pending: return a provisional id keyed off the message.
	provisional := msgID
	if provisional == "" {
		provisional = obsID
	}
	return b.idmap.IntFor(provisional), nil
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
		// V3 fact update body field is `content` (asymmetric with the `fact`
		// read field); send it so MemoryLake's extracted text updates too.
		fields["content"] = *p.Content
	}

	updated, err := b.patchFact(factID, fields)
	if err != nil {
		return nil, err
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
func (b *MemoryLakeBackend) DeleteObservation(id int64, hardDelete bool) error {
	_ = hardDelete // MemoryLake only supports soft delete (forget); see doc comment.
	factID, ok := b.idmap.FactFor(id)
	if !ok {
		return nil
	}
	return b.forgetFact(factID)
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
// First-cut: session grouping, prompts, and DeletedAt filtering are not
// modeled (facts carry no engram session id, and forgotten facts are excluded
// server-side). SessionInfo is left nil. TODO(spec §6): richer timeline
// fidelity once fact metadata carries session linkage.
func (b *MemoryLakeBackend) Timeline(observationID int64, before, after int) (*store.TimelineResult, error) {
	anchorFactID, ok := b.idmap.FactFor(observationID)
	if !ok {
		return nil, &APIError{Code: "NOT_FOUND", Message: "no MemoryLake fact mapped for observation id"}
	}

	facts, err := b.client.listFacts(b.ws, b.projID)
	if err != nil {
		return nil, err
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
			ID:        o.ID,
			Type:      o.Type,
			Title:     o.Title,
			Content:   o.Content,
			Scope:     o.Scope,
			TopicKey:  o.TopicKey,
			CreatedAt: f.CreatedAt,
			UpdatedAt: f.UpdatedAt,
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

// FormatContext renders a lightweight text block of the project's facts,
// optionally filtered by scope.
//
// First-cut: no session/recency grouping like the store's FormatContext.
// TODO(spec §6): match the store's richer context formatting.
func (b *MemoryLakeBackend) FormatContext(project, scope string) (string, error) {
	facts, err := b.client.listFacts(b.ws, b.projID)
	if err != nil {
		return "", err
	}
	var out []byte
	out = append(out, "## Memory Context\n\n"...)
	for _, f := range facts {
		o := ObservationFromFact(f)
		if scope != "" && o.Scope != scope {
			continue
		}
		out = append(out, "- "...)
		if o.Title != "" {
			out = append(out, o.Title...)
			out = append(out, ": "...)
		}
		out = append(out, o.Content...)
		out = append(out, '\n')
	}
	return string(out), nil
}

// Stats reports observation count from the fact list.
//
// First-cut: sessions and prompts are not tracked distinctly on MemoryLake, so
// their counts are 0 and Projects is left empty. TODO(spec §6): populate from a
// statistics endpoint / project listing.
func (b *MemoryLakeBackend) Stats() (*store.Stats, error) {
	facts, err := b.client.listFacts(b.ws, b.projID)
	if err != nil {
		return nil, err
	}
	return &store.Stats{TotalObservations: len(facts)}, nil
}

// CountObservationsForProject counts facts in the backend's project. The name
// argument is accepted for signature compatibility but ignored: a backend
// instance is bound to a single MemoryLake project.
func (b *MemoryLakeBackend) CountObservationsForProject(name string) (int, error) {
	facts, err := b.client.listFacts(b.ws, b.projID)
	if err != nil {
		return 0, err
	}
	return len(facts), nil
}

// ProjectExists reports true: the backend is only constructed for a project
// that was resolved/created at enablement time, so the bound project exists.
// The name argument is accepted for signature compatibility.
//
// First-cut: does not cross-check name against the workspace project list.
// TODO(spec §6): verify against GET .../projects when name != bound project.
func (b *MemoryLakeBackend) ProjectExists(name string) (bool, error) {
	return true, nil
}

// ListProjectNames returns no names in the first cut.
//
// First-cut: engram's human-readable project name is not recoverable from the
// backend's resolved project id alone. TODO(spec §6): list workspace projects
// and map custom_id → engram project name.
func (b *MemoryLakeBackend) ListProjectNames() ([]string, error) {
	return nil, nil
}

// ─── Tier B: sessions (first-cut) ────────────────────────────────────────────

// CreateSession ensures a MemoryLake conversation keyed by the session id.
// This is the one session operation with a real MemoryLake analogue
// (session ↔ conversation), so it is implemented rather than stubbed.
func (b *MemoryLakeBackend) CreateSession(id, project, directory string) error {
	_, err := b.client.ensureConversation(b.ws, b.projID, id, b.actorID)
	return err
}

// GetSession is a first-cut no-op returning "not found".
// TODO(spec §6): fetch the conversation and map it to store.Session.
func (b *MemoryLakeBackend) GetSession(id string) (*store.Session, error) {
	return nil, nil
}

// EndSession is a first-cut no-op: MemoryLake conversations have no "end".
// TODO(spec §6): record the summary as a conversation message or metadata.
func (b *MemoryLakeBackend) EndSession(id string, summary string) error {
	return nil
}

// MostRecentActiveSession is a first-cut no-op returning "no active session".
// TODO(spec §6): derive from recent conversations.
func (b *MemoryLakeBackend) MostRecentActiveSession(project string) (string, bool, error) {
	return "", false, nil
}

// RecentSessions is a first-cut no-op returning no sessions.
// TODO(spec §6): list recent conversations and map to store.SessionSummary.
func (b *MemoryLakeBackend) RecentSessions(project string, limit int) ([]store.SessionSummary, error) {
	return nil, nil
}

// ─── Tier B: prompts / passive capture (first-cut) ──────────────────────────

// AddPrompt is a first-cut no-op returning id 0.
// TODO(spec §6): persist prompts (e.g. as conversation messages) if needed.
func (b *MemoryLakeBackend) AddPrompt(p store.AddPromptParams) (int64, error) {
	return 0, nil
}

// AddPromptIfMissing is a first-cut no-op returning id 0, inserted=false.
// TODO(spec §6): mirror AddPrompt once prompt persistence is designed.
func (b *MemoryLakeBackend) AddPromptIfMissing(p store.AddPromptParams) (int64, bool, error) {
	return 0, false, nil
}

// PassiveCapture is a first-cut no-op reporting nothing captured.
// TODO(spec §6): reuse store.ExtractLearnings + AddObservation to extract and
// save structured learnings from p.Content.
func (b *MemoryLakeBackend) PassiveCapture(p store.PassiveCaptureParams) (*store.PassiveCaptureResult, error) {
	return &store.PassiveCaptureResult{}, nil
}

// ─── Tier B: review (first-cut) ──────────────────────────────────────────────

// ObservationsNeedingReview is a first-cut no-op returning no observations.
// TODO(spec §6): client-side decay by type + fact expiration_date.
func (b *MemoryLakeBackend) ObservationsNeedingReview(project string, limit int) ([]store.Observation, error) {
	return nil, nil
}

// MarkReviewed is a first-cut no-op.
// TODO(spec §6): clear/refresh the client-side review marker for the fact.
func (b *MemoryLakeBackend) MarkReviewed(id int64) error {
	return nil
}

// ─── Tier B: projects / relations (first-cut) ────────────────────────────────

// MergeProjects is unsupported: MemoryLake owns project identity and engram
// must not silently migrate facts across MemoryLake projects.
func (b *MemoryLakeBackend) MergeProjects(sources []string, canonical string) (*store.MergeResult, error) {
	return nil, errors.New("MemoryLake backend does not support merging projects")
}

// FindCandidates is a first-cut no-op returning no candidates.
// TODO(spec §6.1): approximate via the V3 conflicts API.
func (b *MemoryLakeBackend) FindCandidates(savedID int64, opts store.CandidateOptions) ([]store.Candidate, error) {
	return nil, nil
}

// GetRelationsForObservations is a first-cut no-op returning an empty map.
// TODO(spec §6.1): map from the V3 conflicts API.
func (b *MemoryLakeBackend) GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error) {
	return map[string]store.ObservationRelations{}, nil
}

// JudgeRelation is a first-cut no-op returning no relation.
// TODO(spec §6.1): map to the V3 conflict resolve endpoint.
func (b *MemoryLakeBackend) JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error) {
	return nil, nil
}

// JudgeBySemantic is a first-cut no-op returning an empty verdict.
// TODO(spec §6.1): map to the V3 conflict resolve endpoint.
func (b *MemoryLakeBackend) JudgeBySemantic(p store.JudgeBySemanticParams) (string, error) {
	return "", nil
}

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

// patchFact PATCHes arbitrary fields (metadata and/or content) onto a fact and
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
