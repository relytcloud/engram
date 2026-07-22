package memorylake

import (
	"testing"

	"github.com/Gentleman-Programming/engram/internal/store"
)

func TestObservationFromFactPrefersRaw(t *testing.T) {
	f := Fact{
		ID:   "fact-1",
		Fact: "paraphrased text",
		Metadata: map[string]any{
			"engram_raw":   "EXACT original",
			"engram_type":  "decision",
			"engram_scope": "project",
			"topic_key":    "arch/db",
		},
	}
	obs := ObservationFromFact(f)
	if obs.Content != "EXACT original" {
		t.Fatalf("content=%q, want raw", obs.Content)
	}
	if obs.Type != "decision" || obs.Scope != "project" {
		t.Fatalf("metadata not decoded: %+v", obs)
	}
	if obs.TopicKey == nil || *obs.TopicKey != "arch/db" {
		t.Fatalf("topic_key not decoded: %+v", obs)
	}
}

func TestObservationFromFactFallsBackToFactWhenRawMissing(t *testing.T) {
	f := Fact{
		ID:   "fact-2",
		Fact: "paraphrased text",
		Metadata: map[string]any{
			"engram_type":  "note",
			"engram_scope": "project",
		},
	}
	obs := ObservationFromFact(f)
	if obs.Content != "paraphrased text" {
		t.Fatalf("content=%q, want fallback to f.Fact", obs.Content)
	}
	if obs.TopicKey != nil {
		t.Fatalf("topic_key should be nil when absent, got %+v", obs.TopicKey)
	}
}

func TestFactMetadataCarriesRaw(t *testing.T) {
	md := FactMetadata(store.AddObservationParams{
		Title:    "T",
		Content:  "C",
		Type:     "bugfix",
		Scope:    "project",
		TopicKey: "x/y",
	}, "obs-9", "C")
	if md["engram_raw"] != "C" || md["engram_type"] != "bugfix" {
		t.Fatalf("bad metadata: %+v", md)
	}
	if md["engram_title"] != "T" || md["engram_scope"] != "project" {
		t.Fatalf("bad metadata: %+v", md)
	}
	if md["engram_obs_id"] != "obs-9" {
		t.Fatalf("bad metadata: %+v", md)
	}
	if md["topic_key"] != "x/y" {
		t.Fatalf("bad metadata: %+v", md)
	}
}

func TestFactMetadataOmitsTopicKeyWhenEmpty(t *testing.T) {
	md := FactMetadata(store.AddObservationParams{
		Title: "T", Content: "C", Type: "note", Scope: "session",
	}, "obs-1", "C")
	if _, ok := md["topic_key"]; ok {
		t.Fatalf("topic_key must be absent when empty, got %+v", md)
	}
}
