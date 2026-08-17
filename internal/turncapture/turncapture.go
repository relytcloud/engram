// Package turncapture extracts the last completed conversational turn — one
// human message plus the assistant's final reply — from an AI coding agent's
// session transcript, and renders it as a single text blob suitable for
// appending to a MemoryLake conversation.
//
// The Turn type is agent-agnostic; only the parser is Claude-Code-specific.
// Adding a Codex or OpenCode transcript format later means adding one file
// here and touching nothing else in the tree.
//
// Deliberately dependency-free: no network, no store, no MemoryLake concepts.
package turncapture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Turn is one round of conversation in the form it gets written to MemoryLake.
// A Turn with an empty UserText or AssistantText must not be written — see
// Merged.
type Turn struct {
	SessionID     string
	UserText      string
	AssistantText string
}

// entry is the subset of a Claude Code transcript JSONL line this package
// reads. Every other field in the line is ignored.
type entry struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	SessionID   string `json:"sessionId"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
	} `json:"attachment"`
}

// block is one element of a message's content array.
type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// LastTurn parses the Claude Code transcript at path and returns its final
// turn.
//
// Error contract, deliberately narrow because the caller is a fire-and-forget
// hook: an unreadable path is an error; a malformed JSONL line is skipped
// silently (the transcript is a live file — Claude Code may be mid-write when
// the Stop hook fires); and reaching the top of the file without finding a
// human message returns a Turn with an empty UserText and a nil error, leaving
// the "is this worth writing" decision to Merged.
func LastTurn(path string) (Turn, error) {
	lines, err := readLines(path)
	if err != nil {
		return Turn{}, fmt.Errorf("turncapture: read transcript: %w", err)
	}

	var t Turn
	var userParts, assistantParts []string

	// Scan backwards from the end of the file: the turn we want is the tail.
	// Each captured fragment is prepended, so the parts come out in
	// chronological order even though the walk is reversed.
scan:
	for i := len(lines) - 1; i >= 0; i-- {
		var e entry
		if err := json.Unmarshal([]byte(lines[i]), &e); err != nil {
			continue
		}
		if e.IsSidechain {
			continue
		}
		// The last line carrying a session id wins, which makes this the
		// session the transcript itself claims to belong to.
		if t.SessionID == "" && e.SessionID != "" {
			t.SessionID = e.SessionID
		}

		switch {
		case e.Type == "assistant":
			if txt := textBlocks(e.Message.Content); txt != "" {
				assistantParts = append([]string{txt}, assistantParts...)
			}

		case e.Type == "attachment" && e.Attachment.Type == "queued_command":
			// A message the user typed while the assistant was still working.
			// Part of this turn's input, but not its starting boundary.
			if p := strings.TrimSpace(e.Attachment.Prompt); p != "" {
				userParts = append([]string{p}, userParts...)
			}

		case e.Type == "user":
			// Tool results are recorded as type:"user" too. Treating them as
			// the turn boundary would clip every turn at its last tool call.
			if hasBlockType(e.Message.Content, "tool_result") {
				continue
			}
			// isMeta entries are injected context (skill bodies, system
			// prompts), not something the human typed.
			if e.IsMeta {
				continue
			}
			txt := cleanUserText(rawText(e.Message.Content))
			if txt == "" {
				// A wrapper-only entry (e.g. a bare
				// <local-command-stdout>...</local-command-stdout> from a
				// slash command like /login) is machine-injected output, not
				// human input. It must not be mistaken for the turn
				// boundary, or the real human message above it is never
				// reached and the turn comes back with no UserText at all.
				continue
			}
			userParts = append([]string{txt}, userParts...)
			// A real human message starts this turn — stop here.
			break scan
		}
	}

	t.UserText = strings.Join(compact(userParts), "\n\n")
	t.AssistantText = strings.Join(compact(assistantParts), "\n\n")
	return t, nil
}

// readLines reads path into lines. Whole-file for anything up to
// ENGRAM_TURN_MAX_TRANSCRIPT_BYTES; beyond that only the final
// ENGRAM_TURN_TAIL_WINDOW_BYTES are read and the window's first line is
// dropped because the window boundary almost certainly cut it in half.
//
// Uses bufio.Reader rather than bufio.Scanner on purpose: a single transcript
// line (a large tool_result, a whole file's contents) routinely exceeds
// Scanner's token limit, and Scanner's response to that is to abort the whole
// read rather than skip the line.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	dropFirst := false
	if maxWhole := int64(envInt("ENGRAM_TURN_MAX_TRANSCRIPT_BYTES", 64<<20)); st.Size() > maxWhole {
		window := int64(envInt("ENGRAM_TURN_TAIL_WINDOW_BYTES", 2<<20))
		if window > st.Size() {
			window = st.Size()
		}
		offset := st.Size() - window
		if offset > 0 {
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				return nil, err
			}
			// The seek landed inside the file, almost certainly mid-line —
			// the window's first captured line is a truncated fragment, not
			// a real entry, so it must be dropped. When offset is 0 the
			// window covers the whole file and nothing was cut.
			dropFirst = true
		}
	}

	br := bufio.NewReaderSize(f, 256*1024)
	var lines []string
	for {
		line, readErr := br.ReadString('\n')
		if line != "" {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, readErr
		}
	}
	if dropFirst && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, nil
}

// envInt reads a positive integer from the environment, falling back to def for
// an unset, non-numeric, or non-positive value. Mirrors the tolerant behavior of
// internal/memorylake's envInt.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// rawText renders a message's content as plain text. Content is either a bare
// JSON string (the common shape for a typed message) or an array of blocks.
func rawText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return textBlocks(raw)
}

// textBlocks joins every "text" block of a content array, discarding thinking,
// tool_use, tool_result and image blocks. A non-array content yields "".
func textBlocks(raw json.RawMessage) string {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if txt := strings.TrimSpace(b.Text); txt != "" {
			parts = append(parts, txt)
		}
	}
	return strings.Join(parts, "\n\n")
}

// hasBlockType reports whether a content array contains a block of type want.
func hasBlockType(raw json.RawMessage, want string) bool {
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == want {
			return true
		}
	}
	return false
}

// wrapperTags are the XML-ish envelopes Claude Code puts around injected
// content inside an otherwise human-authored message. Their contents are not
// what the user typed, so they are removed whole rather than kept as noise.
var wrapperTags = []string{
	"command-message",
	"command-name",
	"command-args",
	"system-reminder",
	"local-command-stdout",
}

// cleanUserText removes every wrapper tag block from s. When that leaves
// nothing but s did carry a <command-name>, the slash command itself stands in
// (e.g. "/superpowers:brainstorming") so a bare slash-command turn stays
// attributable instead of being dropped entirely.
func cleanUserText(s string) string {
	commandName := strings.TrimSpace(innerText(s, "command-name"))

	out := s
	for _, tag := range wrapperTags {
		out = stripTag(out, tag)
	}
	out = strings.TrimSpace(out)

	if out == "" && commandName != "" {
		return "/" + strings.TrimPrefix(commandName, "/")
	}
	return out
}

// stripTag removes every <tag>…</tag> block, contents included, from s. An
// unterminated opener drops everything from the opener onward rather than
// leaving a dangling marker in the captured text.
func stripTag(s, tag string) string {
	open, closing := "<"+tag+">", "</"+tag+">"
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return s
		}
		rest := s[i+len(open):]
		j := strings.Index(rest, closing)
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + rest[j+len(closing):]
	}
}

// innerText returns the contents of the first <tag>…</tag> block in s, or "".
func innerText(s, tag string) string {
	open, closing := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, closing)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// compact trims each part and drops the ones that end up empty.
func compact(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
