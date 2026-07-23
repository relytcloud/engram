package mcp

// BackendSelector resolves which MemoryBackend a mem_* tool call should use
// for a given project. project is the resolved (possibly empty) project
// name for the call; an empty string means "no project is known yet" (for
// example mem_current_project, or the initial lookup a handler performs
// before it has resolved a project from request arguments).
//
// This is the seam that lets Engram route different projects to different
// storage backends (for example, a local SQLite project alongside a
// MemoryLake-backed project). Task 2 only wires the seam through the mem_*
// handlers with a selector that always resolves to the same backend — actual
// per-project routing to alternative backends is introduced in a later task.
type BackendSelector func(project string) MemoryBackend

// StaticSelector returns a BackendSelector that always resolves to b,
// regardless of the requested project. This is the default selector used by
// NewServer and its siblings: every mem_* call is served by the same local
// SQLite backend, preserving today's behavior exactly.
func StaticSelector(b MemoryBackend) BackendSelector {
	return func(string) MemoryBackend { return b }
}
