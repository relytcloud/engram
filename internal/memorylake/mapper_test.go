package memorylake

import "testing"

// TestObservationFromFact_ContentIsFactTextVerbatim verifies content comes
// straight from fact.Fact — engram_raw is no longer written or read (Option A
// thin adapter, spec §2/§4: content management is mem0's job now).
func TestObservationFromFact_ContentIsFactTextVerbatim(t *testing.T) {
	f := Fact{
		ID:   "fact-1",
		Fact: "mem0's own text",
		Metadata: map[string]any{
			"engram_raw":   "an old build's stamped verbatim copy",
			"engram_type":  "decision",
			"engram_scope": "project",
			"topic_key":    "arch/db",
		},
	}
	obs := ObservationFromFact(f)
	if obs.Content != "mem0's own text" {
		t.Fatalf("content=%q, want fact.Fact verbatim (engram_raw must not be read any more)", obs.Content)
	}
	if obs.SyncID != "fact-1" {
		t.Fatalf("SyncID=%q, want fact-1 (the fact id, no id-mapping indirection)", obs.SyncID)
	}
	if obs.Type != "decision" || obs.Scope != "project" {
		t.Fatalf("metadata not decoded: %+v", obs)
	}
	if obs.TopicKey == nil || *obs.TopicKey != "arch/db" {
		t.Fatalf("topic_key not decoded: %+v", obs)
	}
}

func TestObservationFromFact_NoMetadataStillDecodesContentAndSyncID(t *testing.T) {
	f := Fact{
		ID:   "fact-2",
		Fact: "a fact mem0 created with no engram-authored metadata at all",
	}
	obs := ObservationFromFact(f)
	if obs.Content != "a fact mem0 created with no engram-authored metadata at all" {
		t.Fatalf("content=%q, want fact.Fact", obs.Content)
	}
	if obs.SyncID != "fact-2" {
		t.Fatalf("SyncID=%q, want fact-2", obs.SyncID)
	}
	if obs.TopicKey != nil {
		t.Fatalf("topic_key should be nil when absent, got %+v", obs.TopicKey)
	}
	if obs.RevisionCount != 1 {
		t.Fatalf("RevisionCount=%d, want 1 (default when engram_rev absent)", obs.RevisionCount)
	}
}
