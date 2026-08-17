package turncapture

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMergedRendersBothSpeakers(t *testing.T) {
	turn := Turn{UserText: "fix the uploader", AssistantText: "done"}

	got, ok := turn.Merged(32768)
	if !ok {
		t.Fatal("a complete turn must be writable")
	}
	want := "**User:**\nfix the uploader\n\n**Assistant:**\ndone"
	if got != want {
		t.Fatalf("Merged = %q, want %q", got, want)
	}
}

func TestMergedRejectsIncompleteTurns(t *testing.T) {
	cases := []struct {
		name string
		turn Turn
	}{
		{"no user text", Turn{AssistantText: "done"}},
		{"no assistant text", Turn{UserText: "fix it"}},
		{"whitespace only", Turn{UserText: "   ", AssistantText: "\n\t"}},
		{"both empty", Turn{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.turn.Merged(32768); ok {
				t.Fatal("an incomplete turn must not be writable")
			}
		})
	}
}

func TestMergedExactlyAtLimitIsNotTruncated(t *testing.T) {
	turn := Turn{UserText: "abc", AssistantText: "de"}
	full, ok := turn.Merged(0) // 0 disables the ceiling
	if !ok {
		t.Fatal("want writable")
	}

	got, ok := turn.Merged(len(full))
	if !ok {
		t.Fatal("want writable")
	}
	if got != full {
		t.Fatalf("content exactly at the limit must be untouched, got %q", got)
	}
}

func TestMergedTruncatesOverLimitKeepingHeadAndTail(t *testing.T) {
	user := strings.Repeat("u", 20000)
	asst := strings.Repeat("a", 20000)
	turn := Turn{UserText: user, AssistantText: asst}

	got, ok := turn.Merged(8192)
	if !ok {
		t.Fatal("an over-long turn must still be writable, just truncated")
	}
	if len(got) > 8192 {
		t.Fatalf("len = %d, must not exceed the 8192 ceiling", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation must be marked in the content: %q", got[:200])
	}
	// Both ends of both parts must survive.
	if !strings.Contains(got, "**User:**\nuuu") {
		t.Fatal("the head of the user text must survive")
	}
	if !strings.HasSuffix(got, "a") {
		t.Fatal("the tail of the assistant text must survive")
	}
}

func TestMergedTruncationKeepsValidUTF8(t *testing.T) {
	// Multibyte runes throughout, so a naive byte cut lands mid-rune.
	user := strings.Repeat("用户说的话", 2000)
	asst := strings.Repeat("助手的回复", 2000)
	turn := Turn{UserText: user, AssistantText: asst}

	got, ok := turn.Merged(4096)
	if !ok {
		t.Fatal("want writable")
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation must not split a multibyte rune")
	}
	if len(got) > 4096 {
		t.Fatalf("len = %d, must not exceed the ceiling", len(got))
	}
}

// TestMergedRejectsUnworkableBudget: with a ceiling too small to give both
// parts their floor, the whole turn is dropped rather than written as a stub.
func TestMergedRejectsUnworkableBudget(t *testing.T) {
	turn := Turn{
		UserText:      strings.Repeat("u", 5000),
		AssistantText: strings.Repeat("a", 5000),
	}
	if _, ok := turn.Merged(512); ok {
		t.Fatal("a budget below the two-part floor must drop the turn")
	}
}
