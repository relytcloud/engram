package e2e

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckpointWriteRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCheckpointWriter(&buf)
	k := TupleKey{TaskID: "arch-001", Arm: "memory", Run: 0}
	res := RunResult{TaskID: "arch-001", Arm: "memory", ResultText: "hi", InputTokens: 10, OutputTokens: 5, DurationS: 1.5}
	v := JudgeVerdict{Score: 7.5, PointsHit: []string{"a"}, Reasoning: "ok"}
	if err := cw.WriteRun(k, res); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	if err := cw.WriteVerdict(k, v); err != nil {
		t.Fatalf("WriteVerdict: %v", err)
	}
	// Two newline-terminated JSON lines.
	if n := strings.Count(buf.String(), "\n"); n != 2 {
		t.Fatalf("want 2 lines, got %d: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), `"kind":"run"`) || !strings.Contains(buf.String(), `"kind":"verdict"`) {
		t.Fatalf("missing kind discriminators: %q", buf.String())
	}

	cp, err := ParseCheckpoint(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseCheckpoint: %v", err)
	}
	if cp.Status(k) != StatusComplete {
		t.Fatalf("want StatusComplete, got %v", cp.Status(k))
	}
	gotRes, ok := cp.Run(k)
	if !ok || gotRes.ResultText != "hi" || gotRes.InputTokens != 10 || gotRes.DurationS != 1.5 {
		t.Fatalf("Run mismatch: %+v ok=%v", gotRes, ok)
	}
	gotV, ok := cp.Verdict(k)
	if !ok || gotV.Score != 7.5 || len(gotV.PointsHit) != 1 {
		t.Fatalf("Verdict mismatch: %+v ok=%v", gotV, ok)
	}
}

func TestCheckpointStatusClassification(t *testing.T) {
	var buf bytes.Buffer
	cw := NewCheckpointWriter(&buf)
	kComplete := TupleKey{TaskID: "t1", Arm: "memory", Run: 0}
	kReJudge := TupleKey{TaskID: "t2", Arm: "memory", Run: 0}
	kToRun := TupleKey{TaskID: "t3", Arm: "memory", Run: 0}

	cw.WriteRun(kComplete, RunResult{TaskID: "t1"})
	cw.WriteVerdict(kComplete, JudgeVerdict{Score: 5})
	cw.WriteRun(kReJudge, RunResult{TaskID: "t2"})

	cp, err := ParseCheckpoint(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseCheckpoint: %v", err)
	}
	if cp.Status(kComplete) != StatusComplete {
		t.Errorf("kComplete: want StatusComplete, got %v", cp.Status(kComplete))
	}
	if cp.Status(kReJudge) != StatusReJudge {
		t.Errorf("kReJudge: want StatusReJudge, got %v", cp.Status(kReJudge))
	}
	if cp.Status(kToRun) != StatusToRun {
		t.Errorf("kToRun: want StatusToRun, got %v", cp.Status(kToRun))
	}

	complete, reJudge, toRun := cp.Classify([]TupleKey{kComplete, kReJudge, kToRun})
	if len(complete) != 1 || len(reJudge) != 1 || len(toRun) != 1 {
		t.Errorf("Classify counts: complete=%d reJudge=%d toRun=%d", len(complete), len(reJudge), len(toRun))
	}
}

func TestParseCheckpointEmptyAndBlankLines(t *testing.T) {
	cp, err := ParseCheckpoint(nil)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if cp.Status(TupleKey{TaskID: "x"}) != StatusToRun {
		t.Errorf("empty checkpoint should classify everything as ToRun")
	}
	// Blank lines interspersed must be tolerated.
	data := []byte("\n{\"kind\":\"run\",\"task_id\":\"t1\",\"arm\":\"a\",\"run\":0,\"result\":{}}\n\n")
	cp, err = ParseCheckpoint(data)
	if err != nil {
		t.Fatalf("blank lines: %v", err)
	}
	if cp.Status(TupleKey{TaskID: "t1", Arm: "a", Run: 0}) != StatusReJudge {
		t.Errorf("want StatusReJudge after single run line")
	}
}

func TestParseCheckpointTornFinalLine(t *testing.T) {
	// A crash mid-write can leave a truncated final JSON line; resume must
	// tolerate it (drop the torn line) rather than fatal.
	good := "{\"kind\":\"run\",\"task_id\":\"t1\",\"arm\":\"a\",\"run\":0,\"result\":{}}\n"
	torn := good + "{\"kind\":\"verdict\",\"task_id\":\"t1\",\"ar"
	cp, err := ParseCheckpoint([]byte(torn))
	if err != nil {
		t.Fatalf("torn final line should not error: %v", err)
	}
	if cp.Status(TupleKey{TaskID: "t1", Arm: "a", Run: 0}) != StatusReJudge {
		t.Errorf("torn verdict line must be dropped, leaving ReJudge")
	}
}

func TestParseCheckpointMalformedMiddleLineErrors(t *testing.T) {
	// A malformed non-final line is genuine corruption, not a torn write.
	data := []byte("not json\n{\"kind\":\"run\",\"task_id\":\"t1\",\"arm\":\"a\",\"run\":0,\"result\":{}}\n")
	if _, err := ParseCheckpoint(data); err == nil {
		t.Error("expected error on malformed middle line")
	}
}

func TestCheckpointLastVerdictWins(t *testing.T) {
	// Re-judged tuples append a second verdict line; last one must win.
	var buf bytes.Buffer
	cw := NewCheckpointWriter(&buf)
	k := TupleKey{TaskID: "t1", Arm: "a", Run: 0}
	cw.WriteRun(k, RunResult{TaskID: "t1"})
	cw.WriteVerdict(k, JudgeVerdict{Score: 3})
	cw.WriteVerdict(k, JudgeVerdict{Score: 8})
	cp, err := ParseCheckpoint(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseCheckpoint: %v", err)
	}
	v, _ := cp.Verdict(k)
	if v.Score != 8 {
		t.Errorf("want last verdict 8, got %v", v.Score)
	}
}
