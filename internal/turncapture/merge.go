package turncapture

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	userHeader      = "**User:**\n"
	assistantHeader = "\n\n**Assistant:**\n"

	// minPartBudget is the floor each half of a turn gets before truncation is
	// considered pointless. Below this a "turn" is too mutilated to extract
	// anything useful from, so it is dropped instead of written.
	minPartBudget = 1024
)

// Merged renders t as the single message body written to a MemoryLake
// conversation:
//
//	**User:**
//	<user text>
//
//	**Assistant:**
//	<assistant text>
//
// The speaker labels live in the text because MemoryLake derives a message's
// role from its actor's type and only HUMAN actors can be created through the
// API — so both halves are posted as the same HUMAN actor and the role
// information has to survive in the prose (design decision D3).
//
// ok is false when the turn must not be written: either half is empty, or
// maxBytes is too small to leave both halves their floor. maxBytes <= 0
// disables the ceiling.
func (t Turn) Merged(maxBytes int) (string, bool) {
	user := strings.TrimSpace(t.UserText)
	assistant := strings.TrimSpace(t.AssistantText)
	if user == "" || assistant == "" {
		return "", false
	}

	full := userHeader + user + assistantHeader + assistant
	if maxBytes <= 0 || len(full) <= maxBytes {
		return full, true
	}

	budget := maxBytes - len(userHeader) - len(assistantHeader)
	if budget < 2*minPartBudget {
		return "", false
	}

	// Split the budget in proportion to the original sizes, then hold each
	// half to its floor.
	userBudget := budget * len(user) / (len(user) + len(assistant))
	if userBudget < minPartBudget {
		userBudget = minPartBudget
	}
	assistantBudget := budget - userBudget
	if assistantBudget < minPartBudget {
		assistantBudget = minPartBudget
		userBudget = budget - assistantBudget
	}

	return userHeader + truncateMiddle(user, userBudget) +
		assistantHeader + truncateMiddle(assistant, assistantBudget), true
}

// truncateMiddle shortens s to at most budget bytes by keeping its head (60%)
// and tail (40%) with a marker in between. Head-and-tail rather than head-only
// because a turn's conclusion usually sits at the end — the most valuable part
// is exactly what a head-only cut would throw away.
//
// Both cuts land on rune boundaries, so the result is always valid UTF-8.
func truncateMiddle(s string, budget int) string {
	if len(s) <= budget {
		return s
	}

	marker := fmt.Sprintf("\n…[truncated %d bytes]…\n", len(s)-budget)
	if len(marker) >= budget {
		// No room for both a marker and content: keep a head-only slice.
		return trimTrailingPartialRune(s[:budget])
	}

	keep := budget - len(marker)
	head := keep * 6 / 10
	tail := keep - head
	return trimTrailingPartialRune(s[:head]) + marker + trimLeadingPartialRune(s[len(s)-tail:])
}

// trimTrailingPartialRune drops an incomplete UTF-8 sequence from the end of s.
// Bounded to 3 steps (the longest possible partial sequence) so a string that
// legitimately contains U+FFFD is never eaten.
func trimTrailingPartialRune(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// trimLeadingPartialRune drops an incomplete UTF-8 sequence from the start of s.
func trimLeadingPartialRune(s string) string {
	for i := 0; i < 3 && len(s) > 0; i++ {
		if r, size := utf8.DecodeRuneInString(s); r != utf8.RuneError || size > 1 {
			break
		}
		s = s[1:]
	}
	return s
}
