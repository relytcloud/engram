package memorylake

import (
	"github.com/Gentleman-Programming/engram/internal/store"
)

// promptDedupKey builds the local dedup key AddPromptIfMissing uses to
// detect a repeat prompt: session id + normalized project + content hash,
// mirroring the match internal/store's AddPromptIfMissing performs (`WHERE
// session_id = ? AND ifnull(project,”) = ? AND content = ?`, see
// store.go:2590). Prefixed "prompt:" purely by convention (there is no
// longer a shared key namespace to avoid colliding with, now that the
// process-global IDMap is retired — see MemoryLakeBackend.promptIDs' doc
// comment) — kept so it stays visually distinct from passive.go's
// passiveDedupKey in logs/debugging.
func promptDedupKey(sessionID, project, content string) string {
	norm, _ := store.NormalizeProject(project)
	return "prompt:" + sessionID + "|" + norm + "|" + contentHash(content)
}

// appendPrompt posts p's content as a MemoryLake conversation message (the
// same mechanism AddObservation uses via AppendObservation, see
// writequeue.go) on the conversation keyed by p.SessionID (or
// defaultConversationCustomID when no session is given), and returns the
// MemoryLake message id — the same pending, non-fact sync_id shape
// AddObservation itself returns (see its doc comment for why prompts, like
// observations, have no synchronously-available fact id).
func (b *MemoryLakeBackend) appendPrompt(p store.AddPromptParams) (string, error) {
	convCustomID := p.SessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}
	msgID, err := b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, store.AddObservationParams{
		SessionID: p.SessionID,
		Project:   p.Project,
		Type:      "prompt",
		Title:     "User prompt",
		Content:   p.Content,
	})
	if err != nil {
		return "", err
	}
	if msgID == "" {
		msgID = contentHash(p.Content)
	}
	return msgID, nil
}

// AddPrompt persists p as a MemoryLake conversation message. See
// appendPrompt's doc comment.
func (b *MemoryLakeBackend) AddPrompt(p store.AddPromptParams) (string, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return b.appendPrompt(p)
}

// AddPromptIfMissing mirrors internal/store's AddPromptIfMissing: a prior
// call with the same session_id+project+content (see promptDedupKey) returns
// the existing id and inserted=false without any MemoryLake round trip;
// otherwise it behaves like AddPrompt and reports inserted=true.
//
// The dedup cache (b.promptIDs) is in-process only — see its doc comment on
// MemoryLakeBackend for why that's an accepted trade-off now that the
// process-global, disk-persisted IDMap that used to back this is retired.
func (b *MemoryLakeBackend) AddPromptIfMissing(p store.AddPromptParams) (string, bool, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	key := promptDedupKey(p.SessionID, p.Project, p.Content)

	b.promptMu.Lock()
	if id, ok := b.promptIDs[key]; ok {
		b.promptMu.Unlock()
		return id, false, nil
	}
	b.promptMu.Unlock()

	id, err := b.appendPrompt(p)
	if err != nil {
		return "", false, err
	}

	b.promptMu.Lock()
	if b.promptIDs == nil {
		b.promptIDs = map[string]string{}
	}
	b.promptIDs[key] = id
	b.promptMu.Unlock()

	return id, true, nil
}
