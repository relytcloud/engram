package mcp

import (
	"testing"
)

// TestSQLiteBackendSatisfiesMemoryBackend is a compile-time-only assertion:
// if sqliteBackend ever stops implementing MemoryBackend, this test file will
// fail to compile (not just fail at runtime). *store.Store itself no longer
// implements MemoryBackend directly — by-id methods are keyed by sync_id
// string, translated to/from *store.Store's own int64 primary key by the
// thin sqliteBackend adapter (sqlite_backend.go) — see
// docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md §3 (A1').
func TestSQLiteBackendSatisfiesMemoryBackend(t *testing.T) {
	var _ MemoryBackend = (*sqliteBackend)(nil) // does not compile if unimplemented
}
