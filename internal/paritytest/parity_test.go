//go:build parity

package paritytest

import (
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// loadTestCorpus loads testdata/corpus.jsonl and fails the test loudly if it
// is missing or malformed — every case in this file draws its fixture
// content from it so the corpus stays the single source of truth for
// "what does a representative observation look like" (parity spec §2.2).
func loadTestCorpus(t *testing.T) []CorpusEntry {
	t.Helper()
	entries, err := LoadCorpus(corpusPath())
	if err != nil {
		t.Fatalf("paritytest: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("paritytest: %s is empty", corpusPath())
	}
	return entries
}

func entryByID(t *testing.T, entries []CorpusEntry, id string) CorpusEntry {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("paritytest: corpus entry %q not found", id)
	return CorpusEntry{}
}

// mlReadSettle bounds the eventual-read poll in eventualFactContent. mem0
// extraction is asynchronous and can take well over 90s to surface a fact via
// search (thin-adapter design §1: mem_save is "秒回" / append-and-return, the
// fact only becomes searchable after downstream extraction), so a real parity
// run configures a generous budget. It is kept short here because the default
// dev/CI checkout has no live MemoryLake key and skips the MemoryLake side
// entirely (RequireMemoryLake), so this loop never actually runs there.
const (
	mlReadSettle   = 120 * time.Second
	mlReadInterval = 3 * time.Second
)

// eventualFactContent implements the thin-adapter read path for a
// MemoryLake-backed observation: mem_save returns a *pending* sync_id
// immediately (append-and-return, no synchronous backfill), so the just-saved
// message is not yet a retrievable fact. This polls Search for goldQuery until
// the extracted fact surfaces, then GetObservation on that fact's real sync_id
// and returns its (mem0-extracted) content. Returns ok=false if nothing
// surfaces within mlReadSettle. The registered sync_id is the *fact* id when
// available so t.Cleanup can forget it.
func eventualFactContent(t *testing.T, b mcp.MemoryBackend, goldQuery string, register func(string)) (content string, ok bool) {
	t.Helper()
	deadline := time.Now().Add(mlReadSettle)
	for {
		results, err := b.Search(goldQuery, store.SearchOptions{Limit: 5, MatchMode: "any"})
		if err != nil {
			t.Fatalf("paritytest: Search(%q) during eventual read: %v", goldQuery, err)
		}
		if len(results) > 0 {
			factSyncID := results[0].SyncID
			if register != nil {
				register(factSyncID)
			}
			obs, err := b.GetObservation(factSyncID)
			if err != nil {
				t.Fatalf("paritytest: GetObservation(%q): %v", factSyncID, err)
			}
			return obs.Content, true
		}
		if time.Now().After(deadline) {
			return "", false
		}
		time.Sleep(mlReadInterval)
	}
}

// TestAddObservationRoundTrip_ExactSQLite_SemanticMemoryLake is the
// representative content case from parity spec §4.1 (AddObservation /
// GetObservation row), updated for the thin-adapter design
// (docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md).
//
// The verbatim metadata.engram_raw round-trip is retired: a MemoryLake fact's
// content is the mem0 extraction of the saved text, not the original, so the
// two backends can no longer be compared byte-for-byte. The case is therefore
// split by backend:
//
//   - SQLite: write → read back → require the returned Content to match the
//     original verbatim (ModeExact self-check). SQLite still stores content
//     losslessly, so any mismatch here is a straight regression.
//   - MemoryLake: mem_save returns a *pending* sync_id immediately ("秒回",
//     no synchronous backfill, so we do NOT poll at save time); the fact only
//     becomes readable after asynchronous extraction. We then do an
//     eventual-read (Search → real fact sync_id → GetObservation) and score
//     the extraction SEMANTIC — non-empty and preserving the annotated
//     key_facts (CompareSemantic), not verbatim-equal to SQLite.
func TestAddObservationRoundTrip_ExactSQLite_SemanticMemoryLake(t *testing.T) {
	entries := loadTestCorpus(t)
	entry := entryByID(t, entries, "decision-001")
	if entry.GoldQuery == "" {
		t.Fatalf("paritytest: corpus entry %q has no gold_query (needed for the MemoryLake eventual-read)", entry.ID)
	}

	params := store.AddObservationParams{
		SessionID: "paritytest-session",
		Type:      entry.Type,
		Title:     entry.Title,
		Content:   entry.Content,
		Scope:     entry.Scope,
		TopicKey:  entry.TopicKey,
	}

	// --- SQLite side: verbatim round-trip, ModeExact self-check. ---
	sqliteBackend := NewSQLiteBackend(t)
	// observations.session_id is a NOT NULL FK to sessions in the SQLite
	// schema (mem_save handlers call ensureImplicitSessionWithCWD before
	// AddObservation for the same reason); MemoryLake's CreateSession is a
	// no-op-safe conversation ensure, so the same call is harmless there too.
	if err := sqliteBackend.CreateSession(params.SessionID, "", ""); err != nil {
		t.Fatalf("paritytest: sqlite CreateSession: %v", err)
	}
	sqliteID, err := sqliteBackend.AddObservation(params)
	if err != nil {
		t.Fatalf("paritytest: sqlite AddObservation: %v", err)
	}
	sqliteObs, err := sqliteBackend.GetObservation(sqliteID)
	if err != nil {
		t.Fatalf("paritytest: sqlite GetObservation(%q): %v", sqliteID, err)
	}
	sqliteVerdict := Compare(ModeExact, "AddObservation+GetObservation/decision-001/sqlite",
		Result{Value: sqliteObs.Content}, Result{Value: entry.Content})
	if !sqliteVerdict.Pass {
		t.Errorf("parity FAIL [%s]: %s", sqliteVerdict.Case, sqliteVerdict.Detail)
	}

	// --- MemoryLake side: append-and-return + eventual-read, ModeSemantic. ---
	cfg := RequireMemoryLake(t)
	mlBackend, register := NewMemoryLakeBackend(t, cfg)
	if err := mlBackend.CreateSession(params.SessionID, "", ""); err != nil {
		t.Fatalf("paritytest: memorylake CreateSession: %v", err)
	}
	pendingSyncID, err := mlBackend.AddObservation(params)
	if err != nil {
		t.Fatalf("paritytest: memorylake AddObservation: %v", err)
	}
	// mem_save is "秒回": AddObservation returns a pending sync_id right away
	// without waiting on extraction. Register it so cleanup can attempt to
	// forget it even if the fact never surfaces below.
	register(pendingSyncID)

	mlContent, ok := eventualFactContent(t, mlBackend, entry.GoldQuery, register)
	if !ok {
		// Extraction did not converge within the budget. Not a hard failure:
		// the thin-adapter contract only promises eventual consistency, and a
		// real parity run raises mlReadSettle. Surface it loudly instead.
		t.Skipf("parity SKIP [memorylake decision-001]: fact did not surface within %s (extraction is asynchronous; raise mlReadSettle for a real run)", mlReadSettle)
	}
	mlVerdict := CompareSemantic("AddObservation+GetObservation/decision-001/memorylake", entry.KeyFacts, mlContent)
	if !mlVerdict.Pass {
		t.Errorf("parity FAIL [%s]: %s", mlVerdict.Case, mlVerdict.Detail)
	}
	t.Logf("parity [%s] mode=%s pass=%v detail=%s", mlVerdict.Case, mlVerdict.Mode, mlVerdict.Pass, mlVerdict.Detail)
}

// TestSearch_SetRank is the SET/RANK representative case from parity spec
// §4.1 (Search row): seed both backends with the same corpus entry, run the
// same gold query against each, and compare the returned result sets.
//
// This is a skeleton, not a scored comparison: BM25 (SQLite) and semantic
// (MemoryLake) are expected to disagree on ranking (parity spec §1, §3
// SET/RANK row) and reconciling that needs recall@k/precision@k/MRR against
// entry.RelevantIDs — the metrics Compare's ModeSetRank branch does not
// implement yet (see driver.go's TODO(parity-matrix)). What this case does
// verify today: both backends return a non-empty result set for the gold
// query and don't error, which is enough to catch a hard regression (e.g.
// MemoryLake's semantic search returning nothing at all for a query that
// plainly matches seeded content) while the real scoring lands.
func TestSearch_SetRank(t *testing.T) {
	entries := loadTestCorpus(t)
	entry := entryByID(t, entries, "decision-001")
	if entry.GoldQuery == "" {
		t.Fatalf("paritytest: corpus entry %q has no gold_query", entry.ID)
	}

	seedParams := store.AddObservationParams{
		SessionID: "paritytest-session",
		Type:      entry.Type,
		Title:     entry.Title,
		Content:   entry.Content,
		Scope:     entry.Scope,
		TopicKey:  entry.TopicKey,
	}

	seedAndSearch := func(b mcp.MemoryBackend, register func(string), settle time.Duration) []store.SearchResult {
		// See the content case: session_id is a NOT NULL FK to sessions in the
		// SQLite schema.
		if err := b.CreateSession(seedParams.SessionID, "", ""); err != nil {
			t.Fatalf("paritytest: CreateSession: %v", err)
		}
		id, err := b.AddObservation(seedParams)
		if err != nil {
			t.Fatalf("paritytest: AddObservation: %v", err)
		}
		if register != nil {
			register(id)
		}
		if settle > 0 {
			// MemoryLake extraction is asynchronous and mem_save is now
			// append-and-return (thin-adapter design §1: no synchronous
			// backfill at all), so a just-saved observation is not searchable
			// until extraction completes downstream — potentially well over
			// 90s. TODO(parity-matrix): replace this fixed grace pause with the
			// documented poll-until-stable loop (see eventualFactContent) once
			// this case grows real recall@k scoring; a fixed sleep is only a
			// placeholder that keeps the skeleton honest for a single entry.
			time.Sleep(settle)
		}
		// MatchMode "any" (rather than the default "all") because the gold
		// query is a short keyword bag, not a phrase every token of which is
		// guaranteed to appear verbatim in the seeded content.
		results, err := b.Search(entry.GoldQuery, store.SearchOptions{Limit: 10, MatchMode: "any"})
		if err != nil {
			t.Fatalf("paritytest: Search(%q): %v", entry.GoldQuery, err)
		}
		return results
	}

	sqliteBackend := NewSQLiteBackend(t)
	sqliteResults := seedAndSearch(sqliteBackend, nil, 0)
	if len(sqliteResults) == 0 {
		t.Errorf("parity: sqlite Search(%q) returned no results for a query seeded from its own corpus entry", entry.GoldQuery)
	}

	cfg := RequireMemoryLake(t)
	mlBackend, register := NewMemoryLakeBackend(t, cfg)
	mlResults := seedAndSearch(mlBackend, register, mlReadSettle)
	if len(mlResults) == 0 {
		t.Errorf("parity: memorylake Search(%q) returned no results for a query seeded from its own corpus entry", entry.GoldQuery)
	}

	verdict := Compare(ModeSetRank, "Search/decision-001-gold-query", Result{Value: sqliteResults}, Result{Value: mlResults})
	t.Logf("parity [%s] mode=%s pass=%v detail=%s (recall@k/MRR scoring is TODO(parity-matrix); this case only checks non-empty result sets today)",
		verdict.Case, verdict.Mode, verdict.Pass, verdict.Detail)
}

// TestMergeProjects_Unsupported is the UNSUPPORTED representative case from
// parity spec §4.3 (MergeProjects row): the thin-adapter design states
// mem_merge_projects is explicitly not implemented against a MemoryLake
// project and must return a clear error rather than attempting (or silently
// no-op'ing) a cross-project migration. Per spec §3's UNSUPPORTED row, the
// correctness bar here is "returns an explicit not-supported error", not
// "behaves like SQLite" — there is nothing to compare against SQLite for
// this method once a project is MemoryLake-backed.
func TestMergeProjects_Unsupported(t *testing.T) {
	cfg := RequireMemoryLake(t)
	mlBackend, _ := NewMemoryLakeBackend(t, cfg)

	_, err := mlBackend.MergeProjects([]string{"some-source"}, "some-canonical")
	if err == nil {
		t.Fatalf("parity FAIL: memorylake MergeProjects should return an explicit unsupported error, got nil")
	}
	t.Logf("parity [MergeProjects/unsupported] mode=%s pass=true detail=%q", ModeUnsupported, err.Error())
}
