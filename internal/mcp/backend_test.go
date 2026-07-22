package mcp

import (
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// TestStoreSatisfiesMemoryBackend is a compile-time-only assertion: if
// *store.Store ever stops implementing MemoryBackend, this test file will
// fail to compile (not just fail at runtime).
func TestStoreSatisfiesMemoryBackend(t *testing.T) {
	var _ MemoryBackend = (*store.Store)(nil) // does not compile if unimplemented
}
