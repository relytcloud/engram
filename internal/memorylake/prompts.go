package memorylake

import (
	"strconv"

	"github.com/Gentleman-Programming/engram/internal/store"
)

// promptDedupKey builds the local dedup key AddPromptIfMissing uses to
// detect a repeat prompt: session id + normalized project + content hash,
// mirroring the match internal/store's AddPromptIfMissing performs (`WHERE
// session_id = ? AND ifnull(project,”) = ? AND content = ?`, see
// store.go:2590). Prefixed "prompt:" so it can never collide with a
// MemoryLake fact id or a provisional message id sharing the same IDMap key
// space (see idmap.go's IntIfExists doc comment — IDMap accepts any string
// key, not just fact ids).
func promptDedupKey(sessionID, project, content string) string {
	norm, _ := store.NormalizeProject(project)
	return "prompt:" + sessionID + "|" + norm + "|" + contentHash(content)
}

// appendPrompt posts p's content as a MemoryLake conversation message (the
// same mechanism AddObservation uses via AppendObservation, see
// writequeue.go) on the conversation keyed by p.SessionID (or
// defaultConversationCustomID when no session is given), and returns the
// stable int64 id the shared IDMap assigns to promptDedupKey.
//
// Unlike internal/store's AddPrompt, which always inserts a new user_prompts
// row even for byte-identical content, MemoryLake's message model is
// idempotent on custom_id (see AppendObservation's doc comment): two prompts
// with byte-identical session_id+project+content collapse onto the same
// dedup key and therefore the same id here. This is an accepted, documented
// divergence rather than a fabricated distinct-row guarantee this backend
// cannot actually provide over MemoryLake's API.
func (b *MemoryLakeBackend) appendPrompt(p store.AddPromptParams) (int64, error) {
	convCustomID := p.SessionID
	if convCustomID == "" {
		convCustomID = defaultConversationCustomID
	}
	if _, err := b.client.AppendObservation(b.ws, b.projID, convCustomID, b.actorID, store.AddObservationParams{
		SessionID: p.SessionID,
		Project:   p.Project,
		Type:      "prompt",
		Title:     "User prompt",
		Content:   p.Content,
	}); err != nil {
		return 0, err
	}
	return b.idmap.IntFor(b.projID, promptDedupKey(p.SessionID, p.Project, p.Content)), nil
}

// AddPrompt persists p as a MemoryLake conversation message. See
// appendPrompt's doc comment for how this differs from internal/store's
// always-distinct-row AddPrompt. Returns the decimal string of the IDMap
// int64 id (see backend.go's parseSyncID doc comment for why this backend's
// sync_id is presently that decimal string, not yet the real fact id).
func (b *MemoryLakeBackend) AddPrompt(p store.AddPromptParams) (string, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	id, err := b.appendPrompt(p)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// AddPromptIfMissing mirrors internal/store's AddPromptIfMissing: a prior
// call with the same session_id+project+content (see promptDedupKey) returns
// the existing id and inserted=false without any MemoryLake round trip;
// otherwise it behaves like AddPrompt and reports inserted=true.
func (b *MemoryLakeBackend) AddPromptIfMissing(p store.AddPromptParams) (string, bool, error) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()

	key := promptDedupKey(p.SessionID, p.Project, p.Content)
	if id, ok := b.idmap.IntIfExists(b.projID, key); ok {
		return strconv.FormatInt(id, 10), false, nil
	}
	id, err := b.appendPrompt(p)
	if err != nil {
		return "", false, err
	}
	return strconv.FormatInt(id, 10), true, nil
}
