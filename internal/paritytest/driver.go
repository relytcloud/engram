//go:build parity

// Package paritytest is the SQLite-vs-MemoryLake differential test harness
// described in
// docs/superpowers/specs/2026-07-22-memorylake-sqlite-parity-testing.md
// ("parity spec"). It exists to give #5 ("MemoryLake accuracy/behavior must
// not be worse than SQLite") a runnable measurement instead of a one-off
// manual check: each Case defined against the shared mcp.MemoryBackend
// interface is run once against a *store.Store (SQLite, temp db) and once
// against a *memorylake.MemoryLakeBackend (a throwaway project under the
// configured MemoryLake tenant), and the two results are compared according
// to the Case's CompareMode.
//
// # Build tag
//
// This package is gated behind `-tags parity` and is intentionally excluded
// from the default `go test ./...` run (see CLAUDE.md and
// .github/workflows/ci.yml) — it is slow, costs money (real LLM extraction
// calls) and needs live MemoryLake credentials. `go build -tags parity
// ./internal/paritytest/` must always compile clean regardless of whether
// those credentials are present; this file is deliberately not a _test.go
// file so that compile check exercises real code, not just test scaffolding.
// The actual comparison cases live in parity_test.go, run via
// `go test -tags parity ./internal/paritytest/`; a CI "parity" job (nightly
// or manually triggered, with ENGRAM_MEMORYLAKE_API_KEY injected as a
// secret) is the intended runner. Cases that need a live MemoryLake tenant
// call RequireMemoryLake(t), which calls t.Skip when credentials are absent,
// so the same `go test -tags parity` invocation is also safe (skips, does
// not fail) in a plain dev checkout with no MemoryLake key configured.
//
// # Scope of this first cut
//
// The parity spec's §4 matrix lists ~20 MemoryBackend methods x >=3 cases
// each. This package wires the driver plus a small number of representative
// cases (see parity_test.go) covering the two ends of the compare-mode
// spectrum. NOTE: under the thin-adapter design
// (docs/superpowers/specs/2026-07-23-memorylake-thin-adapter-design.md) the
// verbatim `metadata.engram_raw` round-trip is retired — a MemoryLake fact's
// content is now the mem0 extraction, not the original text — so the former
// cross-backend EXACT content comparison no longer holds. The representative
// content case is therefore split: the SQLite side still round-trips
// verbatim (ModeExact, self-check against the original), while the MemoryLake
// side is scored SEMANTIC (non-empty extraction that preserves the annotated
// key_facts, via CompareSemantic). The second representative case is a
// SET/RANK Search case, where disagreement is expected and must be scored
// against gold annotations rather than asserted equal. The remaining matrix
// rows are tracked as follow-up:
//
//	TODO(parity-matrix): UpdateObservation, DeleteObservation (BEHAVIOR +
//	  UNSUPPORTED hard-delete), Timeline, FormatContext, Stats/Count
//	  (spec §4.1); Pin/Unpin, review, session lifecycle, prompts,
//	  PassiveCapture (spec §4.2); ProjectExists/ListProjectNames,
//	  MergeProjects (UNSUPPORTED), FindCandidates, JudgeRelation/
//	  JudgeBySemantic (spec §4.3). Each needs its Case(s) added to
//	  parity_test.go plus, where the mode is SEMANTIC or SET/RANK, the
//	  LLM-judge protocol from spec §3.1 (testdata/judge-prompt.md is not
//	  yet written — also follow-up) and the recall@k/MRR scaffolding
//	  hinted at by Verdict.Metrics below.
package paritytest

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gentleman-Programming/engram/internal/mcp"
	"github.com/Gentleman-Programming/engram/internal/memorylake"
	"github.com/Gentleman-Programming/engram/internal/store"
)

// CompareMode is the comparison rubric applied to a Case's two results, per
// parity spec §3.
type CompareMode int

const (
	// ModeExact requires byte-for-byte equality. Under the thin-adapter
	// design this only applies to the SQLite side (a verbatim read-back
	// self-check against the original content) — the MemoryLake side no
	// longer stores the original text (engram_raw is retired), so it is
	// scored SEMANTIC instead. Any EXACT difference on the SQLite path is a
	// regression.
	ModeExact CompareMode = iota
	// ModeSemantic scores a MemoryLake read-back against annotated key_facts
	// (spec §3.1). CompareSemantic implements a lightweight token-overlap
	// stand-in (non-empty extraction that preserves the key_facts) until the
	// real two-reviewer LLM judge lands — see CompareSemantic and Verdict.
	ModeSemantic
	// ModeSetRank compares ranked/unordered result sets against gold
	// annotations (recall@k, precision@k, MRR, set IoU per spec §3).
	ModeSetRank
	// ModeBehavior compares post-condition state (e.g. "is it pinned now")
	// rather than the literal response shape.
	ModeBehavior
	// ModeUnsupported asserts MemoryLake returns an explicit "not
	// supported" error/degradation rather than silently doing the wrong
	// thing (e.g. MergeProjects, hard_delete).
	ModeUnsupported
)

func (m CompareMode) String() string {
	switch m {
	case ModeExact:
		return "EXACT"
	case ModeSemantic:
		return "SEMANTIC"
	case ModeSetRank:
		return "SET_RANK"
	case ModeBehavior:
		return "BEHAVIOR"
	case ModeUnsupported:
		return "UNSUPPORTED"
	default:
		return "UNKNOWN"
	}
}

// Case is one parity test case: a named scenario, run identically against
// both backends via Run, whose two Results are then reconciled with
// Compare according to Mode.
//
// Run receives the corpus entry (may be zero-valued for cases that don't
// need one) alongside the backend under test so table-driven cases can
// share fixtures loaded from testdata/corpus.jsonl.
type Case struct {
	Name   string
	Method string // MemoryBackend method under test, e.g. "AddObservation"
	Mode   CompareMode
	Entry  CorpusEntry
	Run    func(b mcp.MemoryBackend, entry CorpusEntry) (Result, error)
}

// Result is what a Case.Run produces for one backend. Value carries
// whatever shape is natural for that method (a string for EXACT content
// comparisons, a []store.SearchResult for SET/RANK, ...); callers type-assert
// it back out in Compare/verdict-building code. Meta carries small
// diagnostic strings (e.g. timing) that don't participate in comparison.
type Result struct {
	Value any
	Meta  map[string]string
}

// Verdict is the outcome of comparing a SQLite Result against a MemoryLake
// Result for one Case, per the "记分卡" (scorecard) row described in parity
// spec §3.2.
type Verdict struct {
	Case    string
	Mode    CompareMode
	Pass    bool
	Winner  string // "sqlite" | "memorylake" | "tie" | "" (n/a for UNSUPPORTED)
	Detail  string
	Metrics map[string]float64 // TODO(parity-matrix): recall@k / MRR / IoU for SET_RANK
}

// Compare reconciles a SQLite result and a MemoryLake result for one Case
// under mode. Only ModeExact has a real, generic implementation (string
// equality) since it is the one mode with an objective pass/fail rule that
// does not need domain-specific scoring or an LLM judge.
//
// ModeSemantic is handled by the dedicated CompareSemantic helper (it needs
// the annotated key_facts, which the two-Result shape here does not carry),
// not by this function.
//
// TODO(parity-matrix): ModeSemantic's CompareSemantic is a token-overlap
// stand-in for the real LLM-judge protocol (spec §3.1: two independent
// reviewers + a tiebreaker, scoring 0-5 on completeness and
// hallucination-freeness); ModeSetRank still needs the recall@k/MRR/IoU
// metrics; ModeBehavior needs per-method post-condition equality checks;
// ModeUnsupported needs a per-method "is this the expected degradation error"
// predicate. Each Case that uses those modes documents, in its own comment,
// what "compare" is expected to mean until this lands.
func Compare(mode CompareMode, name string, sqlite, ml Result) Verdict {
	v := Verdict{Case: name, Mode: mode}
	switch mode {
	case ModeExact:
		sqliteStr, sOK := sqlite.Value.(string)
		mlStr, mOK := ml.Value.(string)
		if !sOK || !mOK {
			v.Pass = false
			v.Detail = fmt.Sprintf("EXACT case requires string Results, got %T vs %T", sqlite.Value, ml.Value)
			v.Winner = "sqlite"
			return v
		}
		if sqliteStr == mlStr {
			v.Pass = true
			v.Winner = "tie"
			v.Detail = "verbatim match"
			return v
		}
		v.Pass = false
		v.Winner = "sqlite"
		v.Detail = fmt.Sprintf("verbatim mismatch: sqlite=%q memorylake=%q", sqliteStr, mlStr)
		return v
	default:
		v.Pass = false
		v.Winner = ""
		v.Detail = mode.String() + " comparison is not implemented yet (TODO(parity-matrix))"
		return v
	}
}

// CompareSemantic scores a MemoryLake read-back (mlContent, the mem0
// extraction of the original observation) against the human-annotated
// keyFacts for one Case, per parity spec §3.1 and the thin-adapter design
// (engram_raw retired → no verbatim comparison, only semantic preservation).
//
// It is deliberately a lightweight, deterministic stand-in for the real
// two-reviewer LLM judge (TODO(parity-matrix)): it passes when the extraction
// is non-empty and preserves a majority of the salient tokens drawn from the
// annotated key_facts (semanticKeyFactRecall >= semanticPassThreshold). This
// catches the hard regressions that matter today (empty/garbage extraction,
// or an extraction that dropped the observation's key facts entirely) without
// pretending to the precision of an LLM equivalence judge. When keyFacts is
// empty the bar degrades to "extraction is non-empty".
func CompareSemantic(name string, keyFacts []string, mlContent string) Verdict {
	v := Verdict{Case: name, Mode: ModeSemantic, Winner: "memorylake", Metrics: map[string]float64{}}
	if strings.TrimSpace(mlContent) == "" {
		v.Pass = false
		v.Detail = "empty MemoryLake extraction (expected a non-empty semantic fact)"
		return v
	}
	recall := semanticKeyFactRecall(keyFacts, mlContent)
	v.Metrics["key_fact_token_recall"] = recall
	if recall >= semanticPassThreshold {
		v.Pass = true
		v.Detail = fmt.Sprintf("non-empty extraction preserves key_facts (token recall %.2f >= %.2f)", recall, semanticPassThreshold)
		return v
	}
	v.Pass = false
	v.Detail = fmt.Sprintf("extraction dropped too many key_facts (token recall %.2f < %.2f): %q", recall, semanticPassThreshold, mlContent)
	return v
}

// semanticPassThreshold is the fraction of salient key_fact tokens that must
// survive into the MemoryLake extraction for CompareSemantic to pass. It is
// intentionally lenient — mem0 paraphrases and compresses, so the goal is to
// detect facts being lost entirely, not to demand verbatim retention.
const semanticPassThreshold = 0.5

// semanticKeyFactRecall returns the fraction of salient tokens (deduplicated
// across all key_facts) that appear, case-insensitively, in content. Returns
// 1.0 when there are no key_facts to check (so a non-empty extraction passes
// by default) and 1.0 when there are key_facts but none yield salient tokens.
func semanticKeyFactRecall(keyFacts []string, content string) float64 {
	haystack := strings.ToLower(content)
	want := map[string]bool{}
	for _, kf := range keyFacts {
		for _, tok := range salientTokens(kf) {
			want[tok] = true
		}
	}
	if len(want) == 0 {
		return 1.0
	}
	hits := 0
	for tok := range want {
		if strings.Contains(haystack, tok) {
			hits++
		}
	}
	return float64(hits) / float64(len(want))
}

// salientTokens lowercases s and returns its content-bearing tokens: those
// longer than three characters that are not common English/Spanish stop
// words, so recall is measured on distinctive terms (identifiers, numbers,
// domain nouns) rather than filler.
func salientTokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r == '.' || r == '/' || r == '_' || r == '-' ||
			(r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
	var out []string
	for _, f := range fields {
		f = strings.Trim(f, "./-_")
		if len(f) <= 3 || semanticStopWords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

// semanticStopWords are short, high-frequency words excluded from
// salientTokens so key_fact recall is measured on distinctive terms.
var semanticStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true,
	"that": true, "this": true, "into": true, "not": true, "are": true,
	"was": true, "reason": true, "still": true, "para": true, "como": true,
	"solo": true, "antes": true, "sobre": true, "cuando": true,
}

// CorpusEntry is one line of testdata/corpus.jsonl: a representative
// observation plus the human annotations the parity spec (§2.2, §3.1) needs
// to score SEMANTIC/SET_RANK cases.
type CorpusEntry struct {
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Scope     string   `json:"scope"`
	TopicKey  string   `json:"topic_key,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Lang      string   `json:"lang,omitempty"`
	KeyFacts  []string `json:"key_facts,omitempty"`
	GoldQuery string   `json:"gold_query,omitempty"`
	// RelevantIDs names other corpus entry IDs that GoldQuery should recall,
	// for SET_RANK cases that need a gold hit set (spec §2.2).
	RelevantIDs []string `json:"relevant_ids,omitempty"`
}

// LoadCorpus reads testdata/corpus.jsonl (one JSON object per line, blank
// lines and lines starting with "#" ignored) into a slice of CorpusEntry.
func LoadCorpus(path string) ([]CorpusEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("paritytest: read corpus %s: %w", path, err)
	}
	var entries []CorpusEntry
	for i, line := range splitLines(b) {
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var e CorpusEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("paritytest: corpus %s line %d: %w", path, i+1, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, trimCR(b[start:i]))
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, trimCR(b[start:]))
	}
	return out
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// NewSQLiteBackend constructs a SQLite-backed mcp.MemoryBackend rooted at a
// fresh t.TempDir(), isolated per the parity spec §2.1.
func NewSQLiteBackend(t *testing.T) mcp.MemoryBackend {
	t.Helper()
	cfg, err := store.DefaultConfig()
	if err != nil {
		t.Fatalf("paritytest: store.DefaultConfig: %v", err)
	}
	cfg.DataDir = t.TempDir()
	s, err := store.New(cfg)
	if err != nil {
		t.Fatalf("paritytest: store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return mcp.NewSQLiteBackend(s)
}

// RequireMemoryLake loads MemoryLake configuration from the environment and
// skips the test when ENGRAM_MEMORYLAKE_API_KEY (or ENGRAM_MEMORYLAKE_BASE_URL)
// is unset, so `go test -tags parity ./internal/paritytest/` is a no-op skip
// (not a failure) in a checkout without live credentials.
func RequireMemoryLake(t *testing.T) memorylake.Config {
	t.Helper()
	cfg := memorylake.LoadConfig()
	if cfg.APIKey == "" || cfg.BaseURL == "" {
		t.Skip("paritytest: ENGRAM_MEMORYLAKE_API_KEY / ENGRAM_MEMORYLAKE_BASE_URL not set; skipping MemoryLake side of parity case (see parity spec §2.1 for the intended CI job)")
	}
	return cfg
}

// NewMemoryLakeBackend provisions a throwaway MemoryLake project
// (engram-parity-<random>) under cfg.Workspace and returns a
// *memorylake.MemoryLakeBackend bound to it, plus a register func that case
// bodies call with every observation sync_id (fact id string) they create so
// t.Cleanup can forget (soft-delete) them afterward — isolation per parity
// spec §2.1.
//
// TODO(parity-matrix): the parity spec (§2.1) also calls for deleting the
// throwaway project itself and unbinding the actor once the case finishes;
// internal/memorylake's Client does not expose project-delete/actor-unbind
// endpoints yet (see spec §12's endpoint list — project/actor deletion is
// not among them), so this cleanup is currently limited to forgetting
// (soft-deleting) facts. The throwaway project itself is left behind in the
// MemoryLake tenant; it is empty of live facts and named distinctly
// (engram-parity-*) so it is easy to bulk-clean out of band.
func NewMemoryLakeBackend(t *testing.T, cfg memorylake.Config) (backend *memorylake.MemoryLakeBackend, register func(id string)) {
	t.Helper()
	client := memorylake.NewClient(cfg)

	ws, err := client.ResolveWorkspaceID(cfg.Workspace)
	if err != nil {
		t.Fatalf("paritytest: ResolveWorkspaceID(%q): %v", cfg.Workspace, err)
	}

	projName := fmt.Sprintf("engram-parity-%d-%04x", time.Now().UnixNano(), rand.Intn(0x10000))
	projID, err := client.EnsureProject(ws, projName)
	if err != nil {
		t.Fatalf("paritytest: EnsureProject(%q): %v", projName, err)
	}

	// Thin-adapter NewBackend(cfg, ws, projID): the idmap is gone (a
	// MemoryLake sync_id is the fact id directly, no int64<->id translation
	// table) and NewBackend resolves the workspace and ensures the actor
	// internally, so only the workspace *name* and the resolved project id
	// are passed here.
	backend, err = memorylake.NewBackend(cfg, cfg.Workspace, projID)
	if err != nil {
		t.Fatalf("paritytest: memorylake.NewBackend: %v", err)
	}

	// Observation ids are now opaque sync_id strings (fact ids), per the
	// thin-adapter interface contract.
	var createdIDs []string
	t.Cleanup(func() {
		for _, id := range createdIDs {
			// Best-effort: forgetting is soft-delete, never hard, per
			// backend.DeleteObservation's documented MemoryLake limitation.
			_ = backend.DeleteObservation(id, true)
		}
	})

	register = func(id string) { createdIDs = append(createdIDs, id) }
	return backend, register
}

// corpusPath is the standard location of the fixture corpus relative to this
// package, per parity spec §2.2.
func corpusPath() string {
	return filepath.Join("testdata", "corpus.jsonl")
}
