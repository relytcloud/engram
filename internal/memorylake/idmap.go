package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// IDMap persists a bijection between engram's local int64 observation IDs
// (store.Observation.ID) and MemoryLake's opaque string fact IDs. Engram's
// callers (CLI, MCP, HTTP API) all expect an int64 observation ID; MemoryLake
// hands back string fact IDs, so this map is what lets the MemoryLake backend
// present observations with stable, engram-shaped IDs.
//
// The forward direction (IntByFact) is the source of truth and is what gets
// persisted to disk. The reverse index (FactByInt) is rebuilt from it on
// Load and never serialized directly.
type IDMap struct {
	mu sync.Mutex

	Next      int64             `json:"next"`
	IntByFact map[string]int64  `json:"int_by_fact"`
	FactByInt map[string]string `json:"-"`

	path string
}

// LoadIDMap reads the id map from path. A missing file is not an error — it
// yields a fresh, empty map starting at 1 (0 is reserved so a zero-value
// Observation.ID reads unambiguously as "unassigned").
func LoadIDMap(path string) (*IDMap, error) {
	m := &IDMap{
		Next:      1,
		IntByFact: map[string]int64{},
		FactByInt: map[string]string{},
		path:      path,
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, m); err != nil {
		return nil, err
	}
	if m.IntByFact == nil {
		m.IntByFact = map[string]int64{}
	}
	if m.Next == 0 {
		m.Next = 1
	}
	m.FactByInt = make(map[string]string, len(m.IntByFact))
	for factID, i := range m.IntByFact {
		m.FactByInt[strconv.FormatInt(i, 10)] = factID
	}
	m.path = path
	return m, nil
}

// IntFor returns the int64 id assigned to factID, allocating and persisting a
// new one (via Next, incremented) if factID has not been seen before.
func (m *IDMap) IntFor(factID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i, ok := m.IntByFact[factID]; ok {
		return i
	}
	i := m.Next
	m.Next++
	m.IntByFact[factID] = i
	m.FactByInt[strconv.FormatInt(i, 10)] = factID
	_ = m.save()
	return i
}

// FactFor reverse-looks-up the MemoryLake fact id for a previously assigned
// int64 id.
func (m *IDMap) FactFor(id int64) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.FactByInt[strconv.FormatInt(id, 10)]
	return f, ok
}

// IntIfExists returns the int64 previously assigned to key without
// allocating one on a miss — unlike IntFor, which always allocates (and
// persists) a fresh id the first time a key is seen. Used by callers that
// need a pure "have we recorded this key before" existence check rather than
// an assignment, e.g. prompt/passive-capture content-hash dedup, which must
// distinguish "already seen" from "seen for the first time" without the side
// effect of allocating an id for keys that turn out to be new misses handled
// elsewhere.
//
// key need not be a MemoryLake fact id: IDMap's bijection works over
// arbitrary strings, so callers are free to use their own prefixed key
// namespaces (e.g. "prompt:...", "passive:...") to borrow this same
// persisted id-allocation mechanism for concerns that have nothing to do
// with facts, so long as those namespaces cannot collide with real fact ids.
func (m *IDMap) IntIfExists(key string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.IntByFact[key]
	return i, ok
}

// Save persists the map to its path, creating parent directories as needed.
func (m *IDMap) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save()
}

// save writes the map to disk. Callers must hold m.mu.
func (m *IDMap) save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, b, 0o644)
}
