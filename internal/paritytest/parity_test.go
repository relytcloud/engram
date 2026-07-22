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

// TestAddObservationGetObservation_Exact is the EXACT representative case
// from parity spec §4.1 (AddObservation / GetObservation row): write one
// observation to each backend, read it back, and require the returned
// Content to match the original verbatim. Per spec §4.2 of the design doc,
// MemoryLake's fact text is an LLM paraphrase but its metadata.engram_raw is
// the verbatim original — backend.GetObservation is documented to prefer
// engram_raw, so this case is exactly the guarantee that preference exists
// to uphold. Any mismatch here is a MemoryLake defect (parity spec §3,
// EXACT row), not a "different but equally valid" result.
func TestAddObservationGetObservation_Exact(t *testing.T) {
	entries := loadTestCorpus(t)
	entry := entryByID(t, entries, "decision-001")

	params := store.AddObservationParams{
		SessionID: "paritytest-session",
		Type:      entry.Type,
		Title:     entry.Title,
		Content:   entry.Content,
		Scope:     entry.Scope,
		TopicKey:  entry.TopicKey,
	}

	readBack := func(b mcp.MemoryBackend) (Result, int64) {
		// observations.session_id is a NOT NULL FK to sessions in the
		// SQLite schema (mem_save handlers call ensureImplicitSessionWithCWD
		// before AddObservation for the same reason); MemoryLake's
		// CreateSession is a no-op-safe conversation ensure, so this call is
		// harmless there too.
		if err := b.CreateSession(params.SessionID, "", ""); err != nil {
			t.Fatalf("paritytest: CreateSession: %v", err)
		}
		id, err := b.AddObservation(params)
		if err != nil {
			t.Fatalf("paritytest: AddObservation: %v", err)
		}
		obs, err := b.GetObservation(id)
		if err != nil {
			t.Fatalf("paritytest: GetObservation(%d): %v", id, err)
		}
		return Result{Value: obs.Content}, id
	}

	sqliteBackend := NewSQLiteBackend(t)
	sqliteResult, _ := readBack(sqliteBackend)

	cfg := RequireMemoryLake(t)
	mlBackend, register := NewMemoryLakeBackend(t, cfg)
	mlResult, mlID := readBack(mlBackend)
	register(mlID)

	verdict := Compare(ModeExact, "AddObservation+GetObservation/decision-001", sqliteResult, mlResult)
	if !verdict.Pass {
		t.Errorf("parity FAIL [%s]: %s", verdict.Case, verdict.Detail)
	}
}

// TestSearch_SetRank is the SET/RANK representative case from parity spec
// §4.1 (Search row): seed both backends with the same handful of corpus
// entries, run the same gold query against each, and compare the returned
// result sets.
//
// This is a skeleton, not a scored comparison: BM25 (SQLite) and semantic +
// fuzzy (MemoryLake) are expected to disagree on ranking (parity spec §1,
// §3 SET/RANK row) and reconciling that needs recall@k/precision@k/MRR
// against entry.RelevantIDs — the metrics Compare's ModeSetRank branch does
// not implement yet (see driver.go's TODO(parity-matrix)). What this case
// does verify today: both backends return a non-empty result set for the
// gold query and don't error, which is enough to catch a hard regression
// (e.g. MemoryLake's semantic search returning nothing at all for a query
// that plainly matches seeded content) while the real scoring lands.
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

	seedAndSearch := func(b mcp.MemoryBackend, register func(int64), settle time.Duration) []store.SearchResult {
		// See TestAddObservationGetObservation_Exact: session_id is a NOT
		// NULL FK to sessions in the SQLite schema.
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
			// MemoryLake extraction is asynchronous (~12s per the design doc
			// §1.2); AddObservation already waits for it (bounded by
			// ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS), but the search index
			// itself may take a beat longer to reflect a just-created fact,
			// so this case gives it one extra short grace pause before
			// searching (parity spec §2.1, "异步收敛"). TODO(parity-matrix):
			// replace with the documented poll-until-stable loop instead of
			// a fixed sleep once this case grows beyond one entry.
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
	mlResults := seedAndSearch(mlBackend, register, 2*time.Second)
	if len(mlResults) == 0 {
		t.Errorf("parity: memorylake Search(%q) returned no results for a query seeded from its own corpus entry", entry.GoldQuery)
	}

	verdict := Compare(ModeSetRank, "Search/decision-001-gold-query", Result{Value: sqliteResults}, Result{Value: mlResults})
	t.Logf("parity [%s] mode=%s pass=%v detail=%s (recall@k/MRR scoring is TODO(parity-matrix); this case only checks non-empty result sets today)",
		verdict.Case, verdict.Mode, verdict.Pass, verdict.Detail)
}

// TestMergeProjects_Unsupported is the UNSUPPORTED representative case from
// parity spec §4.3 (MergeProjects row): the design doc (§6) states
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
