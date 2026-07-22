package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TopicIndex persists a project|scope|topic_key → MemoryLake fact id map so
// AddObservation can implement the same topic_key upsert semantics as
// internal/store's AddObservation (same project+scope+topic_key updates the
// existing row in place instead of inserting a new one — see store.go's
// topic_key match-and-UPDATE branch).
//
// MemoryLake's fact list can only be filtered server-side by fact_fuzzy (a
// substring of the fact's extracted text) — metadata fields such as
// topic_key are not queryable (see search.go's fuzzyFacts comment and the
// task-12 brief). Without a local index, finding "the fact for this
// project+scope+topic_key" would require listing and metadata-scanning every
// fact in the project on every save, which is both slow and racy against
// MemoryLake's asynchronous extraction. This index trades that for an
// eventually-consistent local cache: it is only ever populated by this
// backend's own successful saves (see AddObservation), so it stays accurate
// for engram-authored facts without needing to reconcile against MemoryLake's
// state on load.
type TopicIndex struct {
	mu sync.Mutex

	// FactByKey maps topicIndexKey(project, scope, topicKey) -> fact id.
	FactByKey map[string]string `json:"fact_by_key"`

	path string
}

// topicIndexPath returns the per-project TopicIndex location:
// ~/.engram/memorylake-topics-<projID>.json.
func topicIndexPath(projID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake-topics-"+projID+".json")
}

// LoadTopicIndex reads the topic index from path. A missing file is not an
// error — it yields a fresh, empty index.
func LoadTopicIndex(path string) (*TopicIndex, error) {
	t := &TopicIndex{
		FactByKey: map[string]string{},
		path:      path,
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, t); err != nil {
		return nil, err
	}
	if t.FactByKey == nil {
		t.FactByKey = map[string]string{}
	}
	t.path = path
	return t, nil
}

// topicIndexKey builds the composite lookup key mirroring internal/store's
// topic_key upsert match (project + scope + topic_key). project, scope, and
// topicKey are each normalized the same way internal/store normalizes them
// before its own topic_key match query (store.go's AddObservation), so that
// e.g. scope="" and scope="project" collide onto the same key here exactly as
// they collide onto the same row there — see normalizeIndexProject /
// normalizeIndexScope / normalizeIndexTopicKey below (task-12 hardening
// brief, I1).
func topicIndexKey(project, scope, topicKey string) string {
	return normalizeIndexProject(project) + "|" + normalizeIndexScope(scope) + "|" + normalizeIndexTopicKey(topicKey)
}

// normalizeIndexProject mirrors store.NormalizeProject (exported): lowercase
// + trim + collapse consecutive hyphens/underscores. See
// internal/store/store.go:NormalizeProject.
func normalizeIndexProject(project string) string {
	normalized, _ := store.NormalizeProject(project)
	return normalized
}

// normalizeIndexScope mirrors internal/store's unexported normalizeScope:
// empty or anything other than "personal"/"global" collapses to "project".
// Not exported by internal/store, so its core rule is copied here verbatim —
// see internal/store/store.go:normalizeScope. Keep in sync if that rule ever
// changes.
func normalizeIndexScope(scope string) string {
	v := strings.TrimSpace(strings.ToLower(scope))
	switch v {
	case "personal", "global":
		return v
	default:
		return "project"
	}
}

// normalizeIndexTopicKey mirrors internal/store's unexported
// normalizeTopicKey: lowercase + trim + collapse internal whitespace runs
// into single hyphens, capped at 120 runes. Not exported by internal/store,
// so its core rule is copied here verbatim — see
// internal/store/store.go:normalizeTopicKey. Keep in sync if that rule ever
// changes.
func normalizeIndexTopicKey(topic string) string {
	v := strings.TrimSpace(strings.ToLower(topic))
	if v == "" {
		return ""
	}
	v = strings.Join(strings.Fields(v), "-")
	if len(v) > 120 {
		v = v[:120]
	}
	return v
}

// Lookup returns the MemoryLake fact id previously recorded for
// project+scope+topicKey, if any.
func (t *TopicIndex) Lookup(project, scope, topicKey string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.FactByKey[topicIndexKey(project, scope, topicKey)]
	return f, ok
}

// Put records factID as the fact for project+scope+topicKey and persists the
// index to disk.
func (t *TopicIndex) Put(project, scope, topicKey, factID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.FactByKey[topicIndexKey(project, scope, topicKey)] = factID
	return t.save()
}

// RemoveByFactID deletes every entry that currently points at factID and
// persists the index to disk if anything was removed (a no-op, returning
// false/nil, when nothing pointed at it).
//
// The index is keyed by project|scope|topic_key, not by fact id, so "the
// entry for this fact" is found by reverse-scanning values rather than by
// recomputing the key — which conveniently sidesteps a real gap: MemoryLake
// fact metadata never records engram's logical project name (only
// scope/topic_key are stamped, see mapper.go's FactMetadata), so callers like
// UpdateObservation cannot always reconstruct the exact old key once
// scope/topic_key changes. Scanning by value needs no such reconstruction: it
// removes every pointer to this fact regardless of which key it was filed
// under.
//
// Used by DeleteObservation (a forgotten fact must never stay upsert-able)
// and UpdateObservation (a fact whose scope/topic_key changed must not keep
// answering lookups for its old identity) — see task-12 hardening brief C2.
// Callers should treat a non-nil error as best-effort/non-fatal: the fact's
// own PATCH/forget already succeeded on MemoryLake by the time this runs, and
// AddObservation's hit-validation (task-12 hardening brief C1) independently
// self-heals a stale pointer left behind by a failed purge, so losing this
// cleanup does not reopen the data-loss/drift bug — it only costs an extra
// round trip on the next save.
func (t *TopicIndex) RemoveByFactID(factID string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	removed := false
	for k, v := range t.FactByKey {
		if v == factID {
			delete(t.FactByKey, k)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}
	return true, t.save()
}

// Save persists the index to its path, creating parent directories as
// needed.
func (t *TopicIndex) Save() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.save()
}

// save writes the index to disk. Callers must hold t.mu.
func (t *TopicIndex) save() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(t.path, b, 0o644)
}
