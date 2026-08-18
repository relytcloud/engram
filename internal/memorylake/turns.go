package memorylake

import (
	"fmt"
	"strings"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// AppendTurn appends one completed agent turn — already rendered by
// turncapture.Turn.Merged — as a single message on the MemoryLake conversation
// keyed by sessionID, and returns the MemoryLake message id.
//
// It rides the same path AddPrompt uses (see writequeue.go's
// AppendObservation): ensure the conversation exists by custom_id, then append
// one message whose own custom_id is a hash of the content. That hash is what
// makes replaying a turn safe — MemoryLake resolves the duplicate custom_id to
// the message it already has instead of creating a second one, so re-running
// `engram turn` over the same transcript is a no-op.
//
// Like AddObservation, this does not wait for MemoryLake's extraction pipeline
// and never backfills its result: any fact minted from this turn shows up later
// through the normal read paths (Search, Timeline, FormatContext).
//
// An empty (or whitespace-only) content is rejected rather than posted: a blank
// message would consume an extraction slot and produce nothing.
func (b *MemoryLakeBackend) AppendTurn(sessionID, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("memorylake: AppendTurn: content is empty")
	}

	convCustomID := sessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}

	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	// Type/Title are not part of the conversation-append request body (only
	// Content is) — they are set so logs and future debugging can tell what
	// this message was.
	return b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, store.AddObservationParams{
		SessionID: sessionID,
		Type:      "turn",
		Title:     "Conversation turn",
		Content:   content,
	})
}
