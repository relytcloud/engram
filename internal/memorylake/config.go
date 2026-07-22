// Package memorylake holds configuration and per-project enablement state
// for the optional MemoryLake backend. Engram's SQLite store remains the
// source of truth; MemoryLake is an opt-in, per-project alternate backend.
package memorylake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds MemoryLake connection settings, sourced from environment
// variables with sane defaults.
type Config struct {
	BaseURL, APIKey, Workspace, Actor          string
	TimeoutMS, ExtractPollMS, ExtractMaxWaitMS int
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// LoadConfig reads MemoryLake configuration from the environment, filling in
// defaults for anything unset.
func LoadConfig() Config {
	ws := os.Getenv("ENGRAM_MEMORYLAKE_WORKSPACE")
	if ws == "" {
		ws = "engram"
	}
	return Config{
		BaseURL:          os.Getenv("ENGRAM_MEMORYLAKE_BASE_URL"),
		APIKey:           os.Getenv("ENGRAM_MEMORYLAKE_API_KEY"),
		Workspace:        ws,
		Actor:            os.Getenv("ENGRAM_MEMORYLAKE_ACTOR"),
		TimeoutMS:        envInt("ENGRAM_MEMORYLAKE_TIMEOUT_MS", 30000),
		ExtractPollMS:    envInt("ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS", 2000),
		ExtractMaxWaitMS: envInt("ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS", 30000),
	}
}

// ProjectEntry records when (and under which MemoryLake project id) a local
// Engram project was enabled for the MemoryLake backend.
type ProjectEntry struct {
	ProjID    string `json:"proj_id"`
	EnabledAt string `json:"enabled_at"`
}

// Enablement is the per-project MemoryLake enablement list persisted at
// DefaultEnablementPath. Projects not present here use the local SQLite
// backend (the default and the source of truth).
type Enablement struct {
	EnabledProjects map[string]ProjectEntry `json:"enabled_projects"`
}

// DefaultEnablementPath returns the standard location for the enablement
// list: ~/.engram/memorylake.json.
func DefaultEnablementPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".engram", "memorylake.json")
}

// LoadEnablement reads the enablement list from path. A missing file is not
// an error — it yields an empty Enablement (no projects on MemoryLake yet).
func LoadEnablement(path string) (*Enablement, error) {
	e := &Enablement{EnabledProjects: map[string]ProjectEntry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, e); err != nil {
		return nil, err
	}
	if e.EnabledProjects == nil {
		e.EnabledProjects = map[string]ProjectEntry{}
	}
	return e, nil
}

// Save writes the enablement list to path, creating parent directories as
// needed.
func (e *Enablement) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// IsEnabled reports whether project is enabled for the MemoryLake backend,
// returning its ProjectEntry when so.
func (e *Enablement) IsEnabled(project string) (ProjectEntry, bool) {
	entry, ok := e.EnabledProjects[project]
	return entry, ok
}
