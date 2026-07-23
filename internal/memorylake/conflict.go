package memorylake

import (
	"fmt"
	"net/url"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// This file maps internal/mcp's relation/conflict surface
// (FindCandidates/GetRelationsForObservations/JudgeRelation/JudgeBySemantic)
// onto MemoryLake's V3 memory-conflict API
// (controller/v3/MemoryConflictController.java: list/get/resolve, all scoped
// by `?project_id=`).
//
// The two systems are NOT semantically isomorphic (task-14 brief):
//
//   - Engram's model is synchronous and save-time: FindCandidates runs an
//     FTS5 query the instant an observation is saved and inserts a "pending"
//     memory_relations row per candidate; a human/agent later calls
//     JudgeRelation with one of six verbs (related, compatible, scoped,
//     conflicts_with, supersedes, not_conflict) to record a verdict. Pairs
//     are directional (source_id/target_id) and every verdict is persisted
//     as its own row, judged or not.
//
//   - MemoryLake's model is asynchronous and server-side: some background
//     process (outside engram's control, timing unspecified) detects
//     conflicts between facts (or between a fact and a document, "m2d") and
//     exposes them as conflict objects with exactly one lifecycle action:
//     POST .../resolve, which picks one of four strategies (keep_fact,
//     trust_fact, trust_document, dismiss) and — as far as this client can
//     observe — permanently resolves the conflict. There is no "detect now"
//     call, no per-pair directionality, and no way to record "yes these are
//     related, but not a conflict" without either dismissing the conflict
//     record outright or leaving it untouched.
//
// Given that mismatch, this file is deliberately conservative: verbs/fields
// that map cleanly onto a resolve() call do so for real; verbs that don't
// are documented no-ops rather than best-guess resolve() calls that could
// misrepresent or destroy state MemoryLake can't undo. See the per-function
// doc comments below and task-14-report.md for the full unmappable list.
//
// IMPORTANT (verified from memorylake-backend source, not just the DTOs):
//   - MemoryConflictV3ServiceImpl.listConflicts (the method backing the GET
//     .../memories/conflicts list endpoint this file calls) is an
//     unimplemented stub that returns `null` — see that file's own TODO
//     comment ("cannot be adapted to the fact/conflict endpoint yet"). Every
//     list call this package makes will presently decode to zero conflicts
//     regardless of what MemoryLake has actually detected, until that stub
//     is filled in. get/resolve ARE fully implemented.
//   - MemoryConflictController#resolveConflict declares
//     ResponseWrapper<Void> and calls
//     `memoryConflictService.resolveConflict(...)` without capturing its
//     return value, then always replies `ResponseWrapper.success(null)` —
//     so MemoryConflictResolveResult's resolve_id/forgotten_fact_ids/
//     updated_fact_ids fields, though computed server-side, never reach an
//     API caller today. This file therefore treats resolve as a
//     success/failure-only call and never depends on its response body.

// ─── V3 conflict wire types ──────────────────────────────────────────────────
//
// These mirror dto/v3/memory/{MemoryConflictItem,MemoryConflictFactSnapshot}
// and dto/v3/common/LoadMoreResponse — only the fields this package actually
// consumes are declared. file_chunks (fact-vs-document excerpts, m2d only)
// is intentionally omitted: FindCandidates/JudgeRelation only need fact_ids
// and category to make their (best-effort) decisions.

// conflictFactSnapshot mirrors MemoryConflictFactSnapshot.
type conflictFactSnapshot struct {
	FactID   string `json:"fact_id"`
	FactText string `json:"fact_text"`
}

// conflictItem mirrors MemoryConflictItem.
type conflictItem struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Category      string                 `json:"category"` // "m2m" (fact-vs-fact) | "m2d" (fact-vs-document)
	ConflictType  string                 `json:"conflict_type"`
	FactIDs       []string               `json:"fact_ids"`
	FactSnapshots []conflictFactSnapshot `json:"fact_snapshots"`
	Resolved      bool                   `json:"resolved"`
	Stale         bool                   `json:"stale"`
	CreatedAt     string                 `json:"created_at"`
	UpdatedAt     string                 `json:"updated_at"`
}

// conflictListPage mirrors LoadMoreResponse<MemoryConflictItem>.
type conflictListPage struct {
	Items             []conflictItem `json:"items"`
	ContinuationToken string         `json:"continuation_token"`
}

// conflictResolveRequest mirrors MemoryConflictResolveRequest.
type conflictResolveRequest struct {
	Strategy   string `json:"strategy"`
	KeepFactID string `json:"keep_fact_id,omitempty"`
}

// Resolution strategies accepted by MemoryConflictResolveRequest.strategy.
const (
	resolveStrategyKeepFact      = "keep_fact"
	resolveStrategyTrustFact     = "trust_fact"
	resolveStrategyTrustDocument = "trust_document"
	resolveStrategyDismiss       = "dismiss"
)

// conflictCategoryFactToDoc is MemoryConflictItem.category's "m2d" value
// (fact-vs-document conflict, as opposed to "m2m" fact-vs-fact).
const conflictCategoryFactToDoc = "m2d"

// conflictListPageSize / maxConflictListPages bound listAllConflicts the same
// way ProjectExists/ListProjectNames bound their own project-list reads (see
// those TODO(pagination) comments): a long continuation-token chain must
// never turn a save-time or read-time call into an unbounded fan-out.
// pageSize matches the V3 endpoint's own @Max(100) validation.
const (
	conflictListPageSize = 100
	maxConflictListPages = 20
)

// ─── V3 conflict HTTP helpers ────────────────────────────────────────────────

// listConflictsPage issues one page of GET .../memories/conflicts.
func (b *MemoryLakeBackend) listConflictsPage(pageSize int, continuationToken string) (conflictListPage, error) {
	path := fmt.Sprintf("/api/v3/workspaces/%s/memories/conflicts?project_id=%s&page_size=%d",
		b.ws, url.QueryEscape(b.projID), pageSize)
	if continuationToken != "" {
		path += "&continuation_token=" + url.QueryEscape(continuationToken)
	}
	var page conflictListPage
	if err := b.client.doJSON("GET", path, nil, &page); err != nil {
		return conflictListPage{}, err
	}
	return page, nil
}

// listAllConflicts pages through up to maxConflictListPages pages of this
// backend's project conflicts (see the const's doc comment for the bound,
// and this file's header comment for why every call currently observes zero
// conflicts against the real MemoryLake backend).
func (b *MemoryLakeBackend) listAllConflicts() ([]conflictItem, error) {
	var all []conflictItem
	token := ""
	for i := 0; i < maxConflictListPages; i++ {
		page, err := b.listConflictsPage(conflictListPageSize, token)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.ContinuationToken == "" {
			break
		}
		token = page.ContinuationToken
	}
	return all, nil
}

// getConflictByID issues GET .../memories/conflicts/{conflictId}, scoped to
// this backend's project. A conflict id that belongs to a different
// MemoryLake project (or doesn't exist) is expected to 404 here — this is
// the structural cross-project guard for JudgeRelation (see its doc
// comment): unlike internal/store, this backend never needs to compare two
// independently-supplied project columns, because every conflict it can
// resolve is one MemoryLake itself already scoped to b.projID.
func (b *MemoryLakeBackend) getConflictByID(conflictID string) (conflictItem, error) {
	path := fmt.Sprintf("/api/v3/workspaces/%s/memories/conflicts/%s?project_id=%s",
		b.ws, conflictID, url.QueryEscape(b.projID))
	var c conflictItem
	if err := b.client.doJSON("GET", path, nil, &c); err != nil {
		return conflictItem{}, err
	}
	return c, nil
}

// resolveConflictAPI issues POST .../memories/conflicts/{conflictId}/resolve.
// See this file's header comment: the response body is unreliable by
// construction (the controller always replies with a null data payload), so
// this helper reports only success/failure and callers must not depend on a
// parsed response.
func (b *MemoryLakeBackend) resolveConflictAPI(conflictID, strategy, keepFactID string) error {
	path := fmt.Sprintf("/api/v3/workspaces/%s/memories/conflicts/%s/resolve?project_id=%s",
		b.ws, conflictID, url.QueryEscape(b.projID))
	body := conflictResolveRequest{Strategy: strategy, KeepFactID: keepFactID}
	return b.client.doJSON("POST", path, body, nil)
}

// ─── Relation verb vocabulary ────────────────────────────────────────────────

// validConflictRelationVerbs mirrors internal/store's own (unexported)
// validRelationVerbs set — duplicated here (not exported by internal/store)
// using only the locked, exported store.Relation* constants so this
// package's verb validation stays byte-for-byte in sync with the store's
// vocabulary without importing anything unexported.
var validConflictRelationVerbs = map[string]bool{
	store.RelationRelated:       true,
	store.RelationCompatible:    true,
	store.RelationScoped:        true,
	store.RelationConflictsWith: true,
	store.RelationSupersedes:    true,
	store.RelationNotConflict:   true,
}

func isValidConflictRelationVerb(v string) bool {
	return validConflictRelationVerbs[v]
}

// ─── FindCandidates ───────────────────────────────────────────────────────────

// FindCandidates maps a saved observation's int64 id to its MemoryLake fact
// id and looks for conflicts MemoryLake has already flagged as involving
// that fact, converting each other fact in an unresolved conflict into a
// store.Candidate.
//
// This differs from internal/store's FindCandidates in a fundamental way
// engram's callers must accept: the local store method *runs the detector*
// (an FTS5 query) synchronously at save time. This method does not — it can
// only ask MemoryLake "what have you already found", and MemoryLake's own
// conflict detection is an independent, asynchronously-scheduled process
// this backend does not trigger and cannot wait on. If MemoryLake hasn't
// gotten to this fact yet (or, as of this package's current server build,
// its list endpoint is an unimplemented stub — see this file's header
// comment), FindCandidates fail-safes to an empty, non-error result exactly
// as if no candidates existed — never an error, since a save must never fail
// because best-effort conflict surfacing came back empty.
//
// opts.Project/opts.Type/opts.BM25Floor/opts.SkipInsert have no MemoryLake
// analogue and are accepted-but-ignored for signature compatibility:
//   - opts.Project: this backend is already bound to a single MemoryLake
//     project (see CountObservationsForProject's doc comment).
//   - opts.Type: reserved/unenforced in the store's own Phase 1 too (see
//     CandidateOptions.Type's doc comment) — not worth inventing here.
//   - opts.BM25Floor: MemoryLake's conflict API has no relevance score to
//     floor-filter (see Candidate.Score below).
//   - opts.SkipInsert: the local store's SkipInsert controls whether
//     FindCandidates writes its own "pending" memory_relations rows; this
//     backend never writes anything to discover a candidate (the conflict
//     already exists server-side, detected by MemoryLake, not by this call),
//     so there is nothing to skip.
//
// opts.Scope, by contrast, IS honored: candidates whose engram_scope
// metadata doesn't match are filtered out client-side, mirroring the local
// store's scope-filtered FTS5 query.
// SkipsCandidateGeneration reports true unconditionally: it satisfies
// internal/mcp's candidateOptOut duck-typed interface (see that type's doc
// comment) so mem_save never calls FindCandidates or surfaces
// judgment_required/candidates for a MemoryLake-backed project. FindCandidates
// below is still implemented for direct callers/tests, but the mem_save path
// no longer reaches it.
func (b *MemoryLakeBackend) SkipsCandidateGeneration() bool {
	return true
}

func (b *MemoryLakeBackend) FindCandidates(savedSyncID string, opts store.CandidateOptions) ([]store.Candidate, error) {
	// Under Option A′ (spec §3), sync_id for this backend simply *is* the
	// MemoryLake fact id — no id-mapping lookup needed. A savedSyncID that is
	// actually a still-pending message reference (see AddObservation's doc
	// comment) rather than a materialized fact id just fails to match any
	// fact_ids below and falls through to the empty, non-error result, same
	// as before.
	factID := savedSyncID

	limit := opts.Limit
	if limit <= 0 {
		limit = 3
	}

	conflicts, err := b.listAllConflicts()
	if err != nil {
		// Mirrors internal/store's own contract for this method: detection
		// failure must never fail the originating save.
		return nil, nil
	}

	var candidates []store.Candidate
	for _, c := range conflicts {
		if c.Resolved {
			// Already resolved — no fresh judgment surface needed.
			continue
		}
		if !containsString(c.FactIDs, factID) {
			continue
		}
		for _, otherID := range c.FactIDs {
			if otherID == factID {
				continue
			}
			fact, ferr := b.getFact(otherID)
			if ferr != nil {
				// Best-effort: skip a candidate we can't fetch rather than
				// fabricating its title/type/topic_key.
				continue
			}
			obs := ObservationFromFact(fact)
			if opts.Scope != "" && obs.Scope != opts.Scope {
				continue
			}
			candidates = append(candidates, store.Candidate{
				// ID (the legacy int64 field): left at zero. There is no
				// MemoryLake analogue any more (that was the retired IDMap's
				// job) — SyncID (a real fact id) is the handle callers use.
				SyncID:   otherID,
				Title:    obs.Title,
				Type:     obs.Type,
				TopicKey: obs.TopicKey,
				// Score: MemoryLake's conflict API carries no relevance/BM25-
				// style score for a candidate pair — left at the zero value
				// rather than inventing one. Unmappable field; see task-14
				// report.
				Score: 0,
				// JudgmentID: the local store's JudgmentID is the sync_id of
				// a freshly-inserted pending memory_relations row. This
				// backend has no such row to insert, so JudgmentID is
				// instead the MemoryLake conflict id itself — the one
				// identifier JudgeRelation needs to later resolve this exact
				// conflict (see JudgeRelation's doc comment).
				JudgmentID: c.ID,
			})
			if len(candidates) >= limit {
				return candidates, nil
			}
		}
	}
	return candidates, nil
}

// ─── GetRelationsForObservations ─────────────────────────────────────────────

// GetRelationsForObservations lists this project's conflicts and, for every
// requested sync_id (a MemoryLake fact id — see FindCandidates' JudgmentID
// doc comment for why this backend's "sync_id" is a fact id, not a
// memory_relations row id) that participates in a conflict, synthesizes a
// store.Relation per other fact in that conflict.
//
// Two deliberate simplifications versus the local store's version, both
// consequences of MemoryLake's conflict objects being genuinely symmetric
// (a conflict just lists fact_ids, with no source/target roles):
//
//   - Every synthesized relation is placed in the requested observation's
//     AsSource slice, never AsTarget — MemoryLake has no directionality to
//     recover, so there is no principled way to decide which participant
//     "is" the source. AsTarget is always left empty for MemoryLake-derived
//     relations.
//   - SourceTitle/TargetTitle are left empty (rather than issuing a GET per
//     fact per relation, which would make this call's cost scale with
//     conflicts × facts-per-conflict × requested ids) and
//     SourceMissing/TargetMissing always false (this backend does not
//     additionally check whether a referenced fact has since expired).
//     Documented gaps, not fabricated data.
//
// Relation/JudgmentStatus mapping: an unresolved conflict becomes
// RelationConflictsWith + JudgmentStatusPending (the one thing we ARE sure
// of — MemoryLake flagged these facts as genuinely conflicting, and nothing
// has decided how to reconcile them yet). A resolved conflict becomes
// JudgmentStatusJudged, but Relation STAYS RelationConflictsWith: neither
// MemoryConflictItem (list/get) nor MemoryConflictResolveResult (resolve —
// and see this file's header comment on why that response is unreliable
// even if it did carry a strategy) tells this client which of
// supersedes/not_conflict/etc. was actually applied, so guessing one would
// misrepresent history. This is the clearest unmappable gap in this file;
// see task-14 report.
//
// Confidence/MarkedByActor/MarkedByKind/MarkedByModel/SessionID have no
// MemoryLake analogue exposed on a conflict object and are left nil/empty.
func (b *MemoryLakeBackend) GetRelationsForObservations(syncIDs []string) (map[string]store.ObservationRelations, error) {
	result := map[string]store.ObservationRelations{}
	if len(syncIDs) == 0 {
		return result, nil
	}
	want := make(map[string]bool, len(syncIDs))
	for _, id := range syncIDs {
		want[id] = true
	}

	conflicts, err := b.listAllConflicts()
	if err != nil {
		// Fail-safe: unable to enumerate conflicts — report no relations
		// rather than erroring the caller (mirrors FindCandidates' contract).
		return result, nil
	}

	for _, c := range conflicts {
		for _, srcID := range c.FactIDs {
			if !want[srcID] {
				continue
			}
			for _, tgtID := range c.FactIDs {
				if tgtID == srcID {
					continue
				}
				entry := result[srcID]
				entry.AsSource = append(entry.AsSource, b.relationFromConflict(c, srcID, tgtID))
				result[srcID] = entry
			}
		}
	}
	return result, nil
}

// relationFromConflict builds the store.Relation synthesized for the
// (srcID, tgtID) pair inside conflict c. See GetRelationsForObservations'
// doc comment for the mapping rationale and its documented gaps.
func (b *MemoryLakeBackend) relationFromConflict(c conflictItem, srcID, tgtID string) store.Relation {
	status := store.JudgmentStatusPending
	if c.Resolved {
		status = store.JudgmentStatusJudged
	}
	var reason *string
	if c.Description != "" {
		reason = &c.Description
	}
	// SourceIntID/TargetIntID (legacy int64 annotation fields, json:"-"): left
	// at zero. There is no MemoryLake analogue any more (that was the retired
	// IDMap's job) — SourceID/TargetID (real fact ids) are what callers use.
	return store.Relation{
		SyncID:         c.ID,
		SourceID:       srcID,
		TargetID:       tgtID,
		Relation:       store.RelationConflictsWith,
		Reason:         reason,
		JudgmentStatus: status,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

// ─── JudgeRelation ────────────────────────────────────────────────────────────

// JudgeRelation records a verdict against a MemoryLake conflict. p.JudgmentID
// is expected to be a MemoryLake conflict id (see FindCandidates' doc
// comment) — this backend has no memory_relations table of its own to look
// one up in, so unlike internal/store's JudgeRelation there is no separate
// existence check beyond fetching the conflict itself.
//
// Cross-project guard: a MemoryLakeBackend instance is permanently bound to
// one MemoryLake project (b.projID), and getConflictByID always scopes its
// request with that project id — a conflict id belonging to a different
// project simply 404s here (propagated as this call's error) exactly the
// way internal/store's ErrCrossProjectRelation rejects a cross-project pair.
// There is no separate sentinel to check because this backend never has two
// independently-supplied project values to compare in the first place; see
// getConflictByID's doc comment.
//
// Verb → MemoryLake resolve mapping (task-14 brief §"要做"):
//
//   - supersedes: the ONE verb with an unambiguous MemoryLake analogue —
//     resolve with strategy=keep_fact (m2m) or trust_fact (m2d), keeping
//     whichever fact this call determines is the newer one (see
//     pickSupersedingFact's doc comment for how, and its documented
//     limitation: JudgeRelationParams carries no source/target fact id, only
//     the conflict id, so "which fact is new" must be inferred rather than
//     known).
//   - not_conflict: resolve with strategy=dismiss — the detector was wrong,
//     so telling MemoryLake to dismiss the conflict record is the direct
//     analogue.
//   - conflicts_with: a documented no-op. MemoryLake's conflict already
//     defaults to unresolved; conflicts_with means exactly "yes, still
//     conflicting, not yet decided" — leaving the conflict untouched IS the
//     correct action, not a fallback. No HTTP call is made.
//   - related / compatible / scoped: documented no-ops. MemoryLake's four
//     resolve strategies (keep_fact/trust_fact/trust_document/dismiss) each
//     permanently resolve the conflict one way or another; none of them mean
//     "these overlap but there's no real contradiction, and I'm not asking
//     you to discard anything" (which is closer to dismiss, but "dismiss" is
//     already committed above as not_conflict's mapping and calling it again
//     for a materially different verdict would blur the two). Per the
//     brief's own guidance ("MemoryLake无对应 → 客户端记录(或no-op)"), this
//     backend does NOT call resolve for these three verbs; it returns a
//     client-side-only *store.Relation reflecting the requested verdict, and
//     nothing is persisted in MemoryLake. A subsequent
//     GetRelationsForObservations call will still see the conflict as
//     unresolved (conflicts_with/pending) until something calls resolve on
//     it. This is the second major unmappable gap in this file; see
//     task-14 report.
func (b *MemoryLakeBackend) JudgeRelation(p store.JudgeRelationParams) (*store.Relation, error) {
	if !isValidConflictRelationVerb(p.Relation) {
		return nil, fmt.Errorf("memorylake JudgeRelation: invalid relation verb %q — must be one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict", p.Relation)
	}

	conflict, err := b.getConflictByID(p.JudgmentID)
	if err != nil {
		return nil, fmt.Errorf("memorylake JudgeRelation: fetch conflict %q: %w", p.JudgmentID, err)
	}

	switch p.Relation {
	case store.RelationConflictsWith, store.RelationRelated, store.RelationCompatible, store.RelationScoped:
		return b.clientSideVerdictRelation(conflict, p), nil

	case store.RelationSupersedes:
		keepFactID, err := b.pickSupersedingFact(conflict)
		if err != nil {
			return nil, fmt.Errorf("memorylake JudgeRelation: determine superseding fact for conflict %q: %w", p.JudgmentID, err)
		}
		strategy := resolveStrategyKeepFact
		if conflict.Category == conflictCategoryFactToDoc {
			strategy = resolveStrategyTrustFact
		}
		if err := b.resolveConflictAPI(conflict.ID, strategy, keepFactID); err != nil {
			return nil, fmt.Errorf("memorylake JudgeRelation: resolve conflict %q: %w", p.JudgmentID, err)
		}
		return b.judgedVerdictRelation(conflict, p, keepFactID, otherFactID(conflict.FactIDs, keepFactID)), nil

	case store.RelationNotConflict:
		if err := b.resolveConflictAPI(conflict.ID, resolveStrategyDismiss, ""); err != nil {
			return nil, fmt.Errorf("memorylake JudgeRelation: resolve conflict %q: %w", p.JudgmentID, err)
		}
		src, tgt := firstTwoFactIDs(conflict.FactIDs)
		return b.judgedVerdictRelation(conflict, p, src, tgt), nil

	default:
		// Unreachable: isValidConflictRelationVerb already rejected anything
		// outside the six-verb vocabulary above.
		return nil, fmt.Errorf("memorylake JudgeRelation: unsupported relation verb %q", p.Relation)
	}
}

// pickSupersedingFact determines which fact in conflict c a "supersedes"
// verdict should keep, when JudgeRelation's only input is the conflict id
// (JudgeRelationParams carries no source/target fact id — unlike
// JudgeBySemanticParams, see JudgeBySemantic's doc comment). Heuristic,
// documented as best-effort: fetch each involved fact and keep whichever has
// the latest updated_at (falling back to created_at) — the fact a save just
// touched is the one most likely to be "the new one" a supersedes verdict
// means to keep. Ties break on FactIDs order (first wins).
//
// If every per-fact fetch fails, this falls back to the first fact id rather
// than erroring the whole judgment: the caller already confirmed intent to
// supersede something in this conflict, and a defensible (if arbitrary)
// choice is more useful than blocking the verdict entirely on a read-path
// failure. If the conflict has zero fact ids, that IS an error (nothing to
// supersede) — returned rather than fabricating a fact id that was never in
// the conflict object.
func (b *MemoryLakeBackend) pickSupersedingFact(c conflictItem) (string, error) {
	if len(c.FactIDs) == 0 {
		return "", fmt.Errorf("conflict %q has no fact_ids", c.ID)
	}
	if len(c.FactIDs) == 1 {
		return c.FactIDs[0], nil
	}

	best := ""
	bestTime := ""
	for _, id := range c.FactIDs {
		f, err := b.getFact(id)
		if err != nil {
			continue
		}
		ts := f.UpdatedAt
		if ts == "" {
			ts = f.CreatedAt
		}
		if best == "" || ts > bestTime {
			best, bestTime = id, ts
		}
	}
	if best == "" {
		return c.FactIDs[0], nil
	}
	return best, nil
}

// clientSideVerdictRelation builds the *store.Relation returned for a verb
// that intentionally makes no MemoryLake API call (conflicts_with / related
// / compatible / scoped — see JudgeRelation's doc comment). Nothing here is
// persisted anywhere; it exists only to satisfy this call's return contract.
func (b *MemoryLakeBackend) clientSideVerdictRelation(c conflictItem, p store.JudgeRelationParams) *store.Relation {
	status := store.JudgmentStatusPending
	if p.Relation != store.RelationConflictsWith {
		// related/compatible/scoped are genuine verdicts, just ones with no
		// MemoryLake-side effect — mark them judged locally even though
		// nothing was written server-side.
		status = store.JudgmentStatusJudged
	}
	src, tgt := firstTwoFactIDs(c.FactIDs)
	return &store.Relation{
		SyncID:         c.ID,
		SourceID:       src,
		TargetID:       tgt,
		Relation:       p.Relation,
		Reason:         p.Reason,
		Evidence:       p.Evidence,
		Confidence:     p.Confidence,
		JudgmentStatus: status,
		MarkedByActor:  nonEmptyPtr(p.MarkedByActor),
		MarkedByKind:   nonEmptyPtr(p.MarkedByKind),
		MarkedByModel:  nonEmptyPtr(p.MarkedByModel),
		SessionID:      nonEmptyPtr(p.SessionID),
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

// judgedVerdictRelation builds the *store.Relation returned after a verb
// that DID call resolveConflictAPI successfully (supersedes/not_conflict).
// src/tgt is this file's best-effort, possibly-lossy pairwise representative
// of conflict.FactIDs (see firstTwoFactIDs/otherFactID) — a conflict
// involving more than two facts collapses to a single representative pair,
// documented rather than silently dropped.
func (b *MemoryLakeBackend) judgedVerdictRelation(c conflictItem, p store.JudgeRelationParams, src, tgt string) *store.Relation {
	return &store.Relation{
		SyncID:         c.ID,
		SourceID:       src,
		TargetID:       tgt,
		Relation:       p.Relation,
		Reason:         p.Reason,
		Evidence:       p.Evidence,
		Confidence:     p.Confidence,
		JudgmentStatus: store.JudgmentStatusJudged,
		MarkedByActor:  nonEmptyPtr(p.MarkedByActor),
		MarkedByKind:   nonEmptyPtr(p.MarkedByKind),
		MarkedByModel:  nonEmptyPtr(p.MarkedByModel),
		SessionID:      nonEmptyPtr(p.SessionID),
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

// ─── JudgeBySemantic ──────────────────────────────────────────────────────────

// JudgeBySemantic maps an engram-side semantic verdict (typically produced
// by internal/store's ScanProject worker pool comparing FTS5 candidates via
// an LLM) onto MemoryLake's conflict resolve endpoint.
//
// Unlike JudgeRelation, this method IS given both fact ids directly
// (p.SourceID/p.TargetID — sync_ids, i.e. MemoryLake fact ids, per
// FindCandidates' JudgmentID doc comment), so for a supersedes verdict there
// is no ambiguity about which fact to keep: p.SourceID supersedes
// p.TargetID by definition (JudgeBySemanticParams' own doc comment).
//
// The deepest unmappable gap in this whole file lives here: engram's FTS5 +
// LLM candidate detection and MemoryLake's own conflict detector are two
// entirely independent pipelines. A pair engram's semantic scan flagged may
// have no MemoryLake conflict object at all — MemoryLake's detector may not
// have run yet, may never flag this exact pair, or may already have resolved
// it. There is no MemoryLake endpoint to *create* a conflict directly, only
// to resolve one that already exists, so when findConflictForPair finds no
// matching unresolved conflict, this method fail-safes to a benign empty
// result ("", nil) — mirroring not_conflict's own no-op contract — rather
// than fabricating a MemoryLake-side effect that cannot actually happen.
// This means: a semantic verdict computed by engram's own scan can silently
// have zero effect on MemoryLake state. Documented, not papered over — see
// task-14 report.
//
// Cross-project guard: findConflictForPair only ever lists THIS backend's
// project's conflicts (via listAllConflicts, itself scoped by b.projID), so
// a foreign-project fact id pair simply never matches any conflict here —
// the guard is structural, the same way it is for JudgeRelation/
// getConflictByID.
func (b *MemoryLakeBackend) JudgeBySemantic(p store.JudgeBySemanticParams) (string, error) {
	if p.SourceID == "" {
		return "", fmt.Errorf("memorylake JudgeBySemantic: SourceID is required")
	}
	if p.TargetID == "" {
		return "", fmt.Errorf("memorylake JudgeBySemantic: TargetID is required")
	}
	if !isValidConflictRelationVerb(p.Relation) {
		return "", fmt.Errorf("memorylake JudgeBySemantic: invalid relation verb %q — must be one of: related, compatible, scoped, conflicts_with, supersedes, not_conflict", p.Relation)
	}
	if p.Confidence < 0.0 || p.Confidence > 1.0 {
		return "", fmt.Errorf("memorylake JudgeBySemantic: confidence %v is out of range [0.0, 1.0]", p.Confidence)
	}

	// not_conflict is a no-op, mirroring internal/store's JudgeBySemantic
	// exactly: no row/conflict is touched, no error.
	if p.Relation == store.RelationNotConflict {
		return "", nil
	}

	conflict, ok, err := b.findConflictForPair(p.SourceID, p.TargetID)
	if err != nil {
		// Fail-safe: enumeration failure degrades to a benign no-op rather
		// than failing the caller (mirrors FindCandidates' contract).
		return "", nil
	}
	if !ok {
		// See doc comment above: no MemoryLake conflict exists for this
		// engram-detected pair (or it's already resolved) — nothing to
		// resolve, no error.
		return "", nil
	}

	switch p.Relation {
	case store.RelationConflictsWith, store.RelationRelated, store.RelationCompatible, store.RelationScoped:
		// Same documented no-op as JudgeRelation: no MemoryLake resolve
		// strategy corresponds to these verbs. The conflict id is still
		// returned (mirroring the local store returning the relation's
		// sync_id even though this call is not writing anything to
		// MemoryLake) so callers have a stable identifier to reference.
		return conflict.ID, nil

	case store.RelationSupersedes:
		strategy := resolveStrategyKeepFact
		if conflict.Category == conflictCategoryFactToDoc {
			strategy = resolveStrategyTrustFact
		}
		if err := b.resolveConflictAPI(conflict.ID, strategy, p.SourceID); err != nil {
			return "", fmt.Errorf("memorylake JudgeBySemantic: resolve conflict %q: %w", conflict.ID, err)
		}
		return conflict.ID, nil

	default:
		// Unreachable: isValidConflictRelationVerb already rejected anything
		// outside the six-verb vocabulary, and not_conflict returned above.
		return "", fmt.Errorf("memorylake JudgeBySemantic: unsupported relation verb %q", p.Relation)
	}
}

// findConflictForPair lists this project's conflicts and returns the first
// UNRESOLVED one whose fact_ids include both sourceID and targetID. Already-
// resolved conflicts are treated as "no actionable conflict" (ok=false) —
// calling resolve again would only error against MemoryLake, and there is
// nothing left for this verdict to do.
func (b *MemoryLakeBackend) findConflictForPair(sourceID, targetID string) (conflictItem, bool, error) {
	conflicts, err := b.listAllConflicts()
	if err != nil {
		return conflictItem{}, false, err
	}
	for _, c := range conflicts {
		if c.Resolved {
			continue
		}
		if containsString(c.FactIDs, sourceID) && containsString(c.FactIDs, targetID) {
			return c, true, nil
		}
	}
	return conflictItem{}, false, nil
}

// ─── small helpers ────────────────────────────────────────────────────────────

// containsString reports whether s appears in ss.
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// firstTwoFactIDs returns the first two elements of ids (or fewer, zero-
// valued, if ids is shorter) — the best-effort pairwise representative used
// when a store.Relation needs a single SourceID/TargetID but the underlying
// MemoryLake conflict may involve more than two facts (documented lossy
// collapse; see judgedVerdictRelation's doc comment).
func firstTwoFactIDs(ids []string) (string, string) {
	var a, b string
	if len(ids) > 0 {
		a = ids[0]
	}
	if len(ids) > 1 {
		b = ids[1]
	}
	return a, b
}

// otherFactID returns the first element of ids that isn't except, or "" if
// none exists. Used to pick a representative TargetID for a supersedes
// verdict once keepFactID (the SourceID) is known.
func otherFactID(ids []string, except string) string {
	for _, id := range ids {
		if id != except {
			return id
		}
	}
	return ""
}

// nonEmptyPtr returns nil for an empty string, or a pointer to s otherwise —
// used to build store.Relation's optional *string fields from
// JudgeRelationParams' plain string fields without ever storing an
// intentionally-empty override.
func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
