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

// Config holds MemoryLake connection settings, resolved from three layers in
// precedence order (highest first): environment variables, the persisted
// `engram memorylake config` file, then built-in defaults. See LoadConfig.
type Config struct {
	BaseURL, APIKey, Workspace, Actor          string
	TimeoutMS, ExtractPollMS, ExtractMaxWaitMS int
}

// DefaultBaseURL is the MemoryLake V3 API base URL used when neither the
// ENGRAM_MEMORYLAKE_BASE_URL env var nor a persisted `engram memorylake config`
// value supplies one.
const DefaultBaseURL = "https://app.memorylake.cn/openapi/memorylake"

// DefaultWorkspace is the MemoryLake workspace memories live under when nothing
// overrides it.
const DefaultWorkspace = "engram"

// Connection holds the persisted MemoryLake connection settings written by
// `engram memorylake config`. Every field is optional: an unset field falls
// back first to its env var (which wins over the persisted value) and finally
// to a built-in default. It shares DefaultEnablementPath()'s file with the
// enablement list, so config edits and enable/disable preserve each other.
type Connection struct {
	BaseURL   string `json:"base_url,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Actor     string `json:"actor,omitempty"`
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// pickStr resolves a single string setting: env var (highest precedence), then
// the persisted value, then the built-in default.
func pickStr(env, stored, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	if stored != "" {
		return stored
	}
	return def
}

// LoadConfig resolves MemoryLake configuration by merging, in precedence order:
// environment variables > the persisted `engram memorylake config` file >
// built-in defaults. Reading the persisted file is best-effort — a missing or
// unreadable file simply means "nothing persisted", leaving env vars and
// defaults in effect.
func LoadConfig() Config {
	var conn Connection
	if e, err := LoadEnablement(DefaultEnablementPath()); err == nil && e.Connection != nil {
		conn = *e.Connection
	}
	return Config{
		BaseURL:          pickStr("ENGRAM_MEMORYLAKE_BASE_URL", conn.BaseURL, DefaultBaseURL),
		APIKey:           pickStr("ENGRAM_MEMORYLAKE_API_KEY", conn.APIKey, ""),
		Workspace:        pickStr("ENGRAM_MEMORYLAKE_WORKSPACE", conn.Workspace, DefaultWorkspace),
		Actor:            pickStr("ENGRAM_MEMORYLAKE_ACTOR", conn.Actor, ""),
		TimeoutMS:        envInt("ENGRAM_MEMORYLAKE_TIMEOUT_MS", 30000),
		ExtractPollMS:    envInt("ENGRAM_MEMORYLAKE_EXTRACT_POLL_MS", 2000),
		ExtractMaxWaitMS: envInt("ENGRAM_MEMORYLAKE_EXTRACT_MAX_WAIT_MS", 30000),
	}
}

// LoadConnection returns the persisted MemoryLake connection settings from
// path, or a zero Connection when the file is missing or stores none.
func LoadConnection(path string) (Connection, error) {
	e, err := LoadEnablement(path)
	if err != nil {
		return Connection{}, err
	}
	if e.Connection == nil {
		return Connection{}, nil
	}
	return *e.Connection, nil
}

// SaveConnection persists conn into path, preserving the enablement list that
// already lives in the same file.
func SaveConnection(path string, conn Connection) error {
	e, err := LoadEnablement(path)
	if err != nil {
		return err
	}
	e.Connection = &conn
	return e.Save(path)
}

// ProjectEntry records when (and under which MemoryLake project id) a local
// Engram project was enabled for the MemoryLake backend.
type ProjectEntry struct {
	ProjID    string `json:"proj_id"`
	EnabledAt string `json:"enabled_at"`
}

// Enablement is the persisted MemoryLake state at DefaultEnablementPath: the
// per-project enablement list plus the optional connection config. Projects not
// present in EnabledProjects use the local SQLite backend (the default and the
// source of truth).
type Enablement struct {
	// Connection is the persisted connection config set via
	// `engram memorylake config`. nil when never configured (env vars +
	// defaults then apply). Stored in the same file so config edits and
	// enable/disable preserve each other.
	Connection      *Connection             `json:"connection,omitempty"`
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
	// 0o600: this file may hold the MemoryLake API key (persisted via
	// `engram memorylake config`), so keep it readable only by the owner.
	return os.WriteFile(path, b, 0o600)
}

// IsEnabled reports whether project is enabled for the MemoryLake backend,
// returning its ProjectEntry when so.
func (e *Enablement) IsEnabled(project string) (ProjectEntry, bool) {
	entry, ok := e.EnabledProjects[project]
	return entry, ok
}
