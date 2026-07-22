package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// idmapKeySep separates the MemoryLake project id from the fact id (or other
// per-project dedup key) inside a composite IDMap key. It is the ASCII unit
// separator (0x1f) — a control byte that never appears in a MemoryLake project
// id, a fact id, or any of this package's prefixed dedup keys ("prompt:...",
// "passive:...") — so the two halves can always be recovered unambiguously.
const idmapKeySep = "\x1f"

// IDMap persists a process-global bijection between engram's local int64
// observation IDs (store.Observation.ID) and MemoryLake's opaque string fact
// IDs, each qualified by the MemoryLake project the fact belongs to. Engram's
// callers (CLI, MCP, HTTP API) all expect an int64 observation ID; MemoryLake
// hands back string fact IDs, so this map is what lets the MemoryLake backend
// present observations with stable, engram-shaped IDs.
//
// A SINGLE shared instance backed by ONE file (~/.engram/memorylake-idmap.json,
// see DefaultIDMapPath) is used for EVERY enabled project in a process, so the
// int64 ids it hands out are globally unique. This matters for correctness, not
// just tidiness: `engram serve` is a long-lived process that can route many
// enabled projects at once, and several HTTP endpoints identify their target
// purely by int64 id (GET/PATCH/DELETE /observations/{id}, ...). Were each
// project to keep its own IDMap starting at 1 (the earlier per-project design),
// two projects' first observations would both be id=1 and a by-id lookup could
// silently return the wrong project's content. Qualifying every key by projID
// makes each int64 map to exactly one (projID, factID) pair, so a backend bound
// to one project can reject an id minted for another (see
// MemoryLakeBackend.factForID).
//
// The forward direction (IntByKey, keyed by the composite projID+sep+factID) is
// the source of truth and is what gets persisted to disk. The reverse index
// (keyByInt) is rebuilt from it on Load and never serialized directly.
type IDMap struct {
	mu sync.Mutex

	Next     int64            `json:"next"`
	IntByKey map[string]int64 `json:"int_by_key"`

	// keyByInt is the rebuilt reverse index (global int64 -> composite key);
	// unexported so encoding/json never serializes it.
	keyByInt map[int64]string

	path string
}

// idmapCompositeKey builds the persisted map key for a (projID, factID) pair.
func idmapCompositeKey(projID, factID string) string {
	return projID + idmapKeySep + factID
}

// idmapSplitKey recovers the (projID, factID) halves of a composite key. ok is
// false for a malformed key with no separator (should never happen for keys
// written by idmapCompositeKey, but guards against a hand-edited file).
func idmapSplitKey(key string) (projID, factID string, ok bool) {
	i := strings.IndexByte(key, idmapKeySep[0])
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}

// DefaultIDMapPath is the single, process-global IDMap file shared by every
// MemoryLake-enabled project: ~/.engram/memorylake-idmap.json. (Earlier builds
// used a per-project file, memorylake-idmap-<projID>.json; those are obsolete
// and can be deleted — no migration is performed, the data is a local id cache
// that rebuilds itself as observations are re-read.)
func DefaultIDMapPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake-idmap.json")
}

// LoadIDMap reads the id map from path. A missing file is not an error — it
// yields a fresh, empty map starting at 1 (0 is reserved so a zero-value
// Observation.ID reads unambiguously as "unassigned").
func LoadIDMap(path string) (*IDMap, error) {
	m := &IDMap{
		Next:     1,
		IntByKey: map[string]int64{},
		keyByInt: map[int64]string{},
		path:     path,
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
	if m.IntByKey == nil {
		m.IntByKey = map[string]int64{}
	}
	if m.Next == 0 {
		m.Next = 1
	}
	m.keyByInt = make(map[int64]string, len(m.IntByKey))
	for key, i := range m.IntByKey {
		m.keyByInt[i] = key
	}
	m.path = path
	return m, nil
}

// IntFor returns the int64 id assigned to (projID, factID), allocating and
// persisting a new globally-unique one (via Next, incremented) if that pair has
// not been seen before.
func (m *IDMap) IntFor(projID, factID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := idmapCompositeKey(projID, factID)
	if i, ok := m.IntByKey[key]; ok {
		return i
	}
	i := m.Next
	m.Next++
	m.IntByKey[key] = i
	m.keyByInt[i] = key
	_ = m.save()
	return i
}

// FactFor reverse-looks-up the MemoryLake (projID, factID) pair a previously
// assigned int64 id belongs to. A backend bound to a single project MUST verify
// the returned projID matches its own before trusting factID: an id minted for
// another project must read as not-found to it (see
// MemoryLakeBackend.factForID) — that guard is what stops a by-id lookup from
// crossing project boundaries.
func (m *IDMap) FactFor(id int64) (projID, factID string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, found := m.keyByInt[id]
	if !found {
		return "", "", false
	}
	return idmapSplitKey(key)
}

// IntIfExists returns the int64 previously assigned to (projID, key) without
// allocating one on a miss — unlike IntFor, which always allocates (and
// persists) a fresh id the first time a key is seen. Used by callers that need
// a pure "have we recorded this key before" existence check rather than an
// assignment, e.g. prompt/passive-capture content-hash dedup, which must
// distinguish "already seen" from "seen for the first time" without the side
// effect of allocating an id for keys that turn out to be new misses handled
// elsewhere.
//
// key need not be a MemoryLake fact id: IDMap's bijection works over arbitrary
// strings, so callers are free to use their own prefixed key namespaces (e.g.
// "prompt:...", "passive:...") to borrow this same persisted id-allocation
// mechanism for concerns that have nothing to do with facts, so long as those
// namespaces cannot collide with real fact ids. The projID argument keeps those
// namespaces per-project just as the fact keys are.
func (m *IDMap) IntIfExists(projID, key string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.IntByKey[idmapCompositeKey(projID, key)]
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
