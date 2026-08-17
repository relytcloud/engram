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

// TestMergedFloorRebalancing drives the two three-step floor-adjustment
// branches in Merged: when the proportional split would give one half less
// than minPartBudget, that half is pinned to the floor and the other half
// absorbs whatever budget remains.
//
// Sizes are derived from minPartBudget and the header lengths rather than
// hardcoded byte counts, so the test keeps covering the branch even if the
// floor constant changes. budget is set to 4*minPartBudget specifically so
// that raising one half to the floor can never push the other below it too
// (4*minPartBudget - minPartBudget = 3*minPartBudget >= minPartBudget) —
// each case below is isolated to exactly one of the two adjustment branches.
func TestMergedFloorRebalancing(t *testing.T) {
	const (
		budget   = 4 * minPartBudget
		maxBytes = budget + len(userHeader) + len(assistantHeader)
	)

	cases := []struct {
		name                                string
		userSize, assistantSize             int
		wantUserBudget, wantAssistantBudget int
	}{
		{
			// userSize:assistantSize = 3:20 makes the proportional user share
			// budget*3/23 ≈ 0.13*budget = 0.52*minPartBudget, comfortably
			// under the floor for any minPartBudget, so userBudget gets
			// pinned up and assistantBudget takes the rest.
			name:                "user tiny relative to assistant: user pinned to floor",
			userSize:            3 * minPartBudget,
			assistantSize:       20 * minPartBudget,
			wantUserBudget:      minPartBudget,
			wantAssistantBudget: budget - minPartBudget,
		},
		{
			// Mirror of the above: assistant is the tiny side now, so its
			// proportional share falls under the floor and assistantBudget
			// gets pinned up, with userBudget recomputed as the remainder.
			name:                "assistant tiny relative to user: assistant pinned to floor",
			userSize:            20 * minPartBudget,
			assistantSize:       3 * minPartBudget,
			wantUserBudget:      budget - minPartBudget,
			wantAssistantBudget: minPartBudget,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := Turn{
				UserText:      strings.Repeat("u", tc.userSize),
				AssistantText: strings.Repeat("a", tc.assistantSize),
			}

			got, ok := turn.Merged(maxBytes)
			if !ok {
				t.Fatal("a turn with both halves above the floor must be writable")
			}
			if len(got) != maxBytes {
				t.Fatalf("len(got) = %d, want exactly maxBytes = %d (both halves truncate and the marker fits, so truncateMiddle fills the budget exactly)", len(got), maxBytes)
			}
			if !strings.Contains(got, "truncated") {
				t.Fatal("both halves are over budget here, so truncation must be marked")
			}

			idx := strings.Index(got, assistantHeader)
			if idx < 0 {
				t.Fatal("assistant header not found in merged content")
			}
			userPart := got[len(userHeader):idx]
			assistantPart := got[idx+len(assistantHeader):]

			if len(userPart) != tc.wantUserBudget {
				t.Fatalf("user part = %d bytes, want %d (the floor-rebalanced budget)", len(userPart), tc.wantUserBudget)
			}
			if len(assistantPart) != tc.wantAssistantBudget {
				t.Fatalf("assistant part = %d bytes, want %d (the floor-rebalanced budget)", len(assistantPart), tc.wantAssistantBudget)
			}
			if len(userPart) < minPartBudget || len(assistantPart) < minPartBudget {
				t.Fatalf("both halves must respect the floor: user=%d assistant=%d floor=%d", len(userPart), len(assistantPart), minPartBudget)
			}
			// The invariant the arithmetic depends on: the two halves must
			// exactly exhaust the shared budget, never overshoot or leave
			// bytes on the table.
			if len(userPart)+len(assistantPart) != budget {
				t.Fatalf("userBudget + assistantBudget = %d, want exactly budget = %d", len(userPart)+len(assistantPart), budget)
			}
		})
	}
}
