package memorylake

import "github.com/Gentleman-Programming/engram/internal/store"

// MigrateResult summarizes a bulk migration of existing SQLite observations
// into a MemoryLake-backed project (see MigrateObservations).
type MigrateResult struct {
	Total    int   // observations handed to MigrateObservations
	Migrated int   // successfully appended into MemoryLake
	Failed   int   // per-observation append failures (skipped, not fatal)
	FirstErr error // first append error seen, if any (nil when Failed == 0)
}

// MigrateObservations appends each observation into this MemoryLake-backed
// project through the normal AddObservation write path, so a migrated memory is
// indistinguishable from one saved live (same params, same async fact
// extraction downstream). Used by `engram memorylake enable` to seed a freshly
// enabled project with the memories it already has in local SQLite.
//
// The append is idempotent on content: AppendObservation keys each MemoryLake
// message's custom_id off a hash of the content, so re-running a migration
// re-sends only genuinely new content rather than duplicating what is already
// there. A per-observation failure is counted and skipped rather than aborting
// the whole run — FirstErr holds the first error seen (nil when none), so the
// caller can surface "migrated N, failed M" and still keep the project enabled.
func (b *MemoryLakeBackend) MigrateObservations(obs []store.Observation) MigrateResult {
	res := MigrateResult{Total: len(obs)}
	for i := range obs {
		o := obs[i]
		p := store.AddObservationParams{
			SessionID: o.SessionID,
			Type:      o.Type,
			Title:     o.Title,
			Content:   o.Content,
			Scope:     o.Scope,
		}
		if o.Project != nil {
			p.Project = *o.Project
		}
		if o.ToolName != nil {
			p.ToolName = *o.ToolName
		}
		if o.TopicKey != nil {
			p.TopicKey = *o.TopicKey
		}
		if _, err := b.AddObservation(p); err != nil {
			res.Failed++
			if res.FirstErr == nil {
				res.FirstErr = err
			}
			continue
		}
		res.Migrated++
	}
	return res
}
