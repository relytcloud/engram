package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Checkpoint / resume for the L3 runner. The runner appends one JSON line per
// event to a sidecar file (`<out>.runs.jsonl`): a "run" line after execTask
// (before judging) and a "verdict" line after judging. On restart the sidecar
// is replayed so completed (task,arm,run) tuples are neither re-executed nor
// re-judged, and tuples that were executed but not yet judged skip execution
// and are only re-judged.
//
// The pure serialization / parse / classification logic lives here so it can be
// unit-tested without any claude calls.

// TupleKey identifies one (task, arm, run) unit of work.
type TupleKey struct {
	TaskID string
	Arm    string
	Run    int
}

// Status classifies how much of a tuple's work the checkpoint already records.
type Status int

const (
	// StatusToRun: nothing recorded — execTask then judge.
	StatusToRun Status = iota
	// StatusReJudge: run recorded but no verdict — reuse RunResult, re-judge.
	StatusReJudge
	// StatusComplete: both run and verdict recorded — reuse both, skip work.
	StatusComplete
)

// checkpointLine is one JSONL record. Exactly one of Result / Verdict is set,
// discriminated by Kind ("run" | "verdict").
type checkpointLine struct {
	Kind    string        `json:"kind"`
	TaskID  string        `json:"task_id"`
	Arm     string        `json:"arm"`
	Run     int           `json:"run"`
	Result  *RunResult    `json:"result,omitempty"`
	Verdict *JudgeVerdict `json:"verdict,omitempty"`
}

func (l checkpointLine) key() TupleKey {
	return TupleKey{TaskID: l.TaskID, Arm: l.Arm, Run: l.Run}
}

// Checkpoint is the replayed sidecar: the recorded run and/or verdict per tuple.
type Checkpoint struct {
	runs     map[TupleKey]RunResult
	verdicts map[TupleKey]JudgeVerdict
}

// Status reports how much recorded work exists for k.
func (c *Checkpoint) Status(k TupleKey) Status {
	_, hasRun := c.runs[k]
	_, hasVerdict := c.verdicts[k]
	switch {
	case hasRun && hasVerdict:
		return StatusComplete
	case hasRun:
		return StatusReJudge
	default:
		return StatusToRun
	}
}

// Run returns the recorded RunResult for k, if any.
func (c *Checkpoint) Run(k TupleKey) (RunResult, bool) {
	r, ok := c.runs[k]
	return r, ok
}

// Verdict returns the recorded JudgeVerdict for k, if any.
func (c *Checkpoint) Verdict(k TupleKey) (JudgeVerdict, bool) {
	v, ok := c.verdicts[k]
	return v, ok
}

// Classify partitions keys by Status into complete / re-judge / to-run,
// preserving input order. Used for the resume summary line.
func (c *Checkpoint) Classify(keys []TupleKey) (complete, reJudge, toRun []TupleKey) {
	for _, k := range keys {
		switch c.Status(k) {
		case StatusComplete:
			complete = append(complete, k)
		case StatusReJudge:
			reJudge = append(reJudge, k)
		default:
			toRun = append(toRun, k)
		}
	}
	return
}

// ParseCheckpoint replays sidecar bytes into a Checkpoint. Blank lines are
// skipped. A malformed FINAL line is treated as a torn write (a crash mid-flush)
// and dropped; a malformed non-final line is genuine corruption and errors.
// When a tuple has multiple records of the same kind, the last one wins.
func ParseCheckpoint(data []byte) (*Checkpoint, error) {
	cp := &Checkpoint{runs: map[TupleKey]RunResult{}, verdicts: map[TupleKey]JudgeVerdict{}}
	lines := bytes.Split(data, []byte("\n"))
	for i, raw := range lines {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		var l checkpointLine
		if err := json.Unmarshal(trimmed, &l); err != nil {
			// Tolerate a torn final line; error on corruption mid-stream.
			if isLastNonEmpty(lines, i) {
				break
			}
			return nil, fmt.Errorf("checkpoint line %d: %w", i+1, err)
		}
		switch l.Kind {
		case "run":
			if l.Result != nil {
				cp.runs[l.key()] = *l.Result
			}
		case "verdict":
			if l.Verdict != nil {
				cp.verdicts[l.key()] = *l.Verdict
			}
		default:
			if isLastNonEmpty(lines, i) {
				break
			}
			return nil, fmt.Errorf("checkpoint line %d: unknown kind %q", i+1, l.Kind)
		}
	}
	return cp, nil
}

// isLastNonEmpty reports whether index i is the last line in lines that has any
// non-whitespace content.
func isLastNonEmpty(lines [][]byte, i int) bool {
	for j := i + 1; j < len(lines); j++ {
		if len(bytes.TrimSpace(lines[j])) > 0 {
			return false
		}
	}
	return true
}

type syncer interface{ Sync() error }

// CheckpointWriter appends newline-terminated JSONL records to an io.Writer.
// If the underlying writer supports Sync (e.g. *os.File), each record is
// flushed to disk immediately so a crash loses at most the in-flight line.
type CheckpointWriter struct {
	w io.Writer
}

// NewCheckpointWriter wraps w for appending checkpoint records.
func NewCheckpointWriter(w io.Writer) *CheckpointWriter { return &CheckpointWriter{w: w} }

// WriteRun appends a "run" record for k.
func (c *CheckpointWriter) WriteRun(k TupleKey, res RunResult) error {
	return c.write(checkpointLine{Kind: "run", TaskID: k.TaskID, Arm: k.Arm, Run: k.Run, Result: &res})
}

// WriteVerdict appends a "verdict" record for k.
func (c *CheckpointWriter) WriteVerdict(k TupleKey, v JudgeVerdict) error {
	return c.write(checkpointLine{Kind: "verdict", TaskID: k.TaskID, Arm: k.Arm, Run: k.Run, Verdict: &v})
}

func (c *CheckpointWriter) write(l checkpointLine) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if s, ok := c.w.(syncer); ok {
		return s.Sync()
	}
	return nil
}
