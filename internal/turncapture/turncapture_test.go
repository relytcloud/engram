package turncapture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript writes lines as a JSONL file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const (
	// A plain human message: content is a bare JSON string.
	lineUserPlain = `{"type":"user","sessionId":"sess-1","isSidechain":false,"message":{"role":"user","content":"add a retry to the uploader"}}`
	// The assistant's final prose reply.
	lineAsstText = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"Done — the uploader now retries three times."}]}}`
	// Thinking and tool_use must be discarded.
	lineAsstThinking = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me look at the file"}]}}`
	lineAsstToolUse  = `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{}}]}}`
	// A tool result arrives as type:"user" — it must NOT end the turn.
	lineToolResult = `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
)

func TestLastTurnPlainTextTurn(t *testing.T) {
	p := writeTranscript(t, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q, want sess-1", turn.SessionID)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q", turn.AssistantText)
	}
}

// TestLastTurnSkipsThinkingToolUseAndToolResults is the core boundary test:
// a turn full of tool traffic must still yield exactly the human message and
// the assistant's prose, and the tool_result entries (which are type:"user")
// must not be mistaken for the start of the turn.
func TestLastTurnSkipsThinkingToolUseAndToolResults(t *testing.T) {
	p := writeTranscript(t,
		lineUserPlain,
		lineAsstThinking,
		lineAsstToolUse,
		lineToolResult,
		lineAsstToolUse,
		lineToolResult,
		lineAsstText,
	)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q; tool_result entries must not end the turn", turn.UserText)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q; thinking/tool_use must be discarded", turn.AssistantText)
	}
}

// TestLastTurnStopsAtTurnBoundary verifies the previous turn does not bleed in.
func TestLastTurnStopsAtTurnBoundary(t *testing.T) {
	prevUser := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"first question"}}`
	prevAsst := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}]}}`
	p := writeTranscript(t, prevUser, prevAsst, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "first question") {
		t.Fatalf("UserText leaked the previous turn: %q", turn.UserText)
	}
	if strings.Contains(turn.AssistantText, "first answer") {
		t.Fatalf("AssistantText leaked the previous turn: %q", turn.AssistantText)
	}
}

// TestLastTurnCollectsQueuedCommandInterjection covers a mid-turn message the
// user typed while the assistant was working: Claude Code records it as an
// attachment of type queued_command, not as a normal user entry.
func TestLastTurnCollectsQueuedCommandInterjection(t *testing.T) {
	queued := `{"type":"attachment","sessionId":"sess-1","attachment":{"type":"queued_command","prompt":"also bump the timeout"}}`
	// The paired queue-operation entry repeats the same text and must be ignored.
	queueOp := `{"type":"queue-operation","operation":"enqueue","sessionId":"sess-1","content":"also bump the timeout"}`
	p := writeTranscript(t, lineUserPlain, queueOp, queued, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	want := "add a retry to the uploader\n\nalso bump the timeout"
	if turn.UserText != want {
		t.Fatalf("UserText = %q, want %q", turn.UserText, want)
	}
	if strings.Count(turn.UserText, "also bump the timeout") != 1 {
		t.Fatalf("queue-operation must not duplicate the interjection: %q", turn.UserText)
	}
}

// TestLastTurnIgnoresMetaEntries: injected context (skill bodies, system
// prompts) arrives as an isMeta user entry. It is neither captured nor treated
// as a turn boundary.
func TestLastTurnIgnoresMetaEntries(t *testing.T) {
	meta := `{"type":"user","isMeta":true,"sessionId":"sess-1","message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /tmp/skill"}]}}`
	p := writeTranscript(t, lineUserPlain, meta, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "Base directory") {
		t.Fatalf("isMeta content must not be captured: %q", turn.UserText)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
}

func TestLastTurnSkipsSidechainEntries(t *testing.T) {
	subUser := `{"type":"user","isSidechain":true,"sessionId":"sess-1","message":{"role":"user","content":"subagent task"}}`
	subAsst := `{"type":"assistant","isSidechain":true,"sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"subagent answer"}]}}`
	p := writeTranscript(t, lineUserPlain, subUser, subAsst, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if strings.Contains(turn.UserText, "subagent") || strings.Contains(turn.AssistantText, "subagent") {
		t.Fatalf("sidechain entries must be skipped: user=%q asst=%q", turn.UserText, turn.AssistantText)
	}
}

func TestLastTurnStripsWrapperTags(t *testing.T) {
	wrapped := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"<system-reminder>be nice</system-reminder>please refactor this<local-command-stdout>ok</local-command-stdout>"}}`
	p := writeTranscript(t, wrapped, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "please refactor this" {
		t.Fatalf("UserText = %q; wrapper tags must be removed whole", turn.UserText)
	}
}

// TestLastTurnBareSlashCommandKeepsCommandName: a slash command with no
// arguments is nothing but wrapper tags. Rather than dropping the turn, the
// command itself stands in as the user's input.
func TestLastTurnBareSlashCommandKeepsCommandName(t *testing.T) {
	slash := `{"type":"user","sessionId":"sess-1","message":{"role":"user","content":"<command-message>brainstorming</command-message>\n<command-name>superpowers:brainstorming</command-name>"}}`
	p := writeTranscript(t, slash, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "/superpowers:brainstorming" {
		t.Fatalf("UserText = %q, want /superpowers:brainstorming", turn.UserText)
	}
}

func TestLastTurnJoinsMultipleAssistantTextBlocks(t *testing.T) {
	first := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"part one"}]}}`
	second := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"part two"}]}}`
	p := writeTranscript(t, lineUserPlain, first, lineAsstToolUse, second)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.AssistantText != "part one\n\npart two" {
		t.Fatalf("AssistantText = %q, want the blocks joined in order", turn.AssistantText)
	}
}

// TestLastTurnSkipsCorruptLines: the transcript is a live file, so the final
// line can be half-written. A bad line is skipped, not fatal.
func TestLastTurnSkipsCorruptLines(t *testing.T) {
	p := writeTranscript(t, lineUserPlain, lineAsstText, `{"type":"assistant","message":{"content":[{"type":"te`)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("a truncated final line must not fail the parse: %v", err)
	}
	if turn.AssistantText != "Done — the uploader now retries three times." {
		t.Fatalf("AssistantText = %q", turn.AssistantText)
	}
}

func TestLastTurnNoUserMessageYieldsEmptyUserText(t *testing.T) {
	p := writeTranscript(t, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("reaching the top of the file is not an error: %v", err)
	}
	if turn.UserText != "" {
		t.Fatalf("UserText = %q, want empty", turn.UserText)
	}
	if turn.AssistantText == "" {
		t.Fatal("AssistantText should still be captured")
	}
}

func TestLastTurnMissingFileIsError(t *testing.T) {
	if _, err := LastTurn(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Fatal("an unreadable transcript must be an error")
	}
}

// TestLastTurnTailWindowOnHugeTranscript exercises the large-file path: with a
// 1-byte whole-file ceiling the tail window is used, its first (possibly
// truncated) line is dropped, and the turn is still found inside the window.
func TestLastTurnTailWindowOnHugeTranscript(t *testing.T) {
	t.Setenv("ENGRAM_TURN_MAX_TRANSCRIPT_BYTES", "1")
	t.Setenv("ENGRAM_TURN_TAIL_WINDOW_BYTES", "4096")

	filler := `{"type":"assistant","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"old"}]}}`
	p := writeTranscript(t, filler, lineUserPlain, lineAsstText)

	turn, err := LastTurn(p)
	if err != nil {
		t.Fatalf("LastTurn: %v", err)
	}
	if turn.UserText != "add a retry to the uploader" {
		t.Fatalf("UserText = %q", turn.UserText)
	}
}
