package memorylake

import (
	"strings"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// MigrateResult summarizes a bulk migration of existing SQLite observations
// into a MemoryLake-backed project (see MigrateObservations).
type MigrateResult struct {
	Total    int   // observations handed to MigrateObservations
	Migrated int   // newly written into MemoryLake as verbatim facts
	Skipped  int   // already present (same text) or empty — not re-written
	Failed   int   // fact-add failures (a failed batch counts each of its items)
	FirstErr error // first add error seen, if any (nil when Failed == 0)
}

// migrationFactText renders one observation as the verbatim fact text to store,
// sharing factText with the live AddObservation write path so a migrated memory
// and a live-saved one are rendered identically (title preserved).
func migrationFactText(o store.Observation) string {
	return factText(o.Title, o.Content)
}

// MigrateObservations seeds a freshly enabled project by writing its existing
// SQLite observations into MemoryLake through the direct, verbatim fact-add
// endpoint (Client.AddFacts) rather than the conversation-append path — so a
// migrated memory is stored as-is and synchronously (with a real fact id),
// instead of being paraphrased by an asynchronous mem0 extraction. Used by
// `engram memorylake enable` on first enable. SQLite stays the source of truth;
// nothing is deleted locally.
//
// The direct endpoint does NOT deduplicate, so MigrateObservations makes itself
// idempotent: it first lists the project's existing facts and skips any
// observation whose rendered text is already present. Re-running a migration
// therefore writes only genuinely new content instead of duplicating. New facts
// are written in batches; a failed batch is counted and skipped rather than
// aborting the run, with FirstErr holding the first error seen.
func (b *MemoryLakeBackend) MigrateObservations(obs []store.Observation) MigrateResult {
	res := MigrateResult{Total: len(obs)}

	// Idempotency guard: build the set of texts already stored for this project.
	// A listing failure is non-fatal — we proceed without dedup (at worst a
	// re-run duplicates), rather than refusing to migrate at all.
	existing := map[string]bool{}
	if facts, err := b.client.listAllFacts(b.ws, b.projID); err == nil {
		for _, f := range facts {
			existing[strings.TrimSpace(f.Fact)] = true
		}
	}

	// Collect the new (not-already-present, non-empty) fact texts to write.
	var texts []string
	for i := range obs {
		t := migrationFactText(obs[i])
		if t == "" {
			res.Skipped++
			continue
		}
		if existing[strings.TrimSpace(t)] {
			res.Skipped++
			continue
		}
		existing[strings.TrimSpace(t)] = true // dedup within this batch too
		texts = append(texts, t)
	}

	// Write in bounded batches so one huge project doesn't post a giant body.
	const batchSize = 100
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		chunk := texts[start:end]
		created, err := b.client.AddFacts(b.ws, b.projID, chunk)
		if err != nil {
			res.Failed += len(chunk)
			if res.FirstErr == nil {
				res.FirstErr = err
			}
			continue
		}
		if n := len(created); n > 0 {
			res.Migrated += n
		} else {
			// Endpoint accepted the write but echoed nothing back; count the
			// chunk as migrated so the summary reflects what we sent.
			res.Migrated += len(chunk)
		}
	}
	return res
}
