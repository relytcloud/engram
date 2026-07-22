package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
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
// topic_key upsert match (project + scope + topic_key). Callers are expected
// to pass the same project/scope strings used elsewhere on the write path
// (AddObservationParams.Project / .Scope) — this backend does not replicate
// store's normalizeScope/normalizeTopicKey helpers, so no additional
// normalization happens here (first-cut; see task brief).
func topicIndexKey(project, scope, topicKey string) string {
	return project + "|" + scope + "|" + topicKey
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
