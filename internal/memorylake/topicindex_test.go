package memorylake

import "testing"

// ─── Task 12 hardening (I1): key normalization ──────────────────────────────

// TestTopicIndexKey_EmptyScopeCollapsesToProject verifies scope="" and
// scope="project" produce the identical index key, mirroring internal/store's
// normalizeScope (empty/unrecognized scope collapses to "project" before the
// topic_key match query — see internal/store/store.go:normalizeScope).
func TestTopicIndexKey_EmptyScopeCollapsesToProject(t *testing.T) {
	a := topicIndexKey("proj", "", "arch/db")
	b := topicIndexKey("proj", "project", "arch/db")
	if a != b {
		t.Fatalf("topicIndexKey with scope=%q and scope=%q differ: %q vs %q, want identical", "", "project", a, b)
	}
}

// TestTopicIndexKey_UnrecognizedScopeCollapsesToProject verifies any scope
// string that isn't "personal"/"global" (case-insensitively) also collapses
// to "project", matching normalizeScope's default branch.
func TestTopicIndexKey_UnrecognizedScopeCollapsesToProject(t *testing.T) {
	want := topicIndexKey("proj", "project", "arch/db")
	for _, scope := range []string{"", "PROJECT", "  ", "bogus", "Project"} {
		if got := topicIndexKey("proj", scope, "arch/db"); got != want {
			t.Fatalf("topicIndexKey(scope=%q)=%q, want %q (collapse to project)", scope, got, want)
		}
	}
}

// TestTopicIndexKey_RecognizedScopesStayDistinct verifies "personal" and
// "global" are NOT collapsed into "project" (only unrecognized/empty values
// are), and case-normalize the same as normalizeScope.
func TestTopicIndexKey_RecognizedScopesStayDistinct(t *testing.T) {
	personal := topicIndexKey("proj", "personal", "arch/db")
	global := topicIndexKey("proj", "global", "arch/db")
	project := topicIndexKey("proj", "project", "arch/db")
	if personal == global || personal == project || global == project {
		t.Fatalf("personal/global/project keys must all differ: %q / %q / %q", personal, global, project)
	}
	if got := topicIndexKey("proj", "PERSONAL", "arch/db"); got != personal {
		t.Fatalf("scope case not normalized: %q != %q", got, personal)
	}
}

// TestTopicIndexKey_ProjectNormalization verifies project goes through
// store.NormalizeProject (lowercase + trim + collapse repeated hyphens),
// exactly as internal/store applies it before its own topic_key match.
func TestTopicIndexKey_ProjectNormalization(t *testing.T) {
	want := topicIndexKey("my-proj", "global", "arch/db")
	for _, project := range []string{"My-Proj", "  my-proj  ", "MY-PROJ", "my--proj"} {
		if got := topicIndexKey(project, "global", "arch/db"); got != want {
			t.Fatalf("topicIndexKey(project=%q)=%q, want %q", project, got, want)
		}
	}
}

// TestTopicIndexKey_TopicKeyNormalization verifies topic_key goes through the
// same lowercase+whitespace-collapse rule as internal/store's
// normalizeTopicKey.
func TestTopicIndexKey_TopicKeyNormalization(t *testing.T) {
	want := topicIndexKey("proj", "global", "arch-db")
	for _, tk := range []string{"Arch DB", "  arch   db  ", "ARCH-DB"} {
		if got := topicIndexKey("proj", "global", tk); got != want {
			t.Fatalf("topicIndexKey(topicKey=%q)=%q, want %q", tk, got, want)
		}
	}
}

// TestTopicIndex_LookupAfterPutWithDifferentButEquivalentScope verifies the
// full Put→Lookup round trip: a Put under scope="" is found by a Lookup under
// scope="project" (and vice versa), since both normalize to the same key —
// this is the concrete "second save upserts instead of appending" scenario
// from the task-12 hardening brief.
func TestTopicIndex_LookupAfterPutWithDifferentButEquivalentScope(t *testing.T) {
	ti, err := LoadTopicIndex(t.TempDir() + "/topics.json")
	if err != nil {
		t.Fatalf("LoadTopicIndex: %v", err)
	}
	if err := ti.Put("proj", "", "arch/db", "fact-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	factID, ok := ti.Lookup("proj", "project", "arch/db")
	if !ok || factID != "fact-1" {
		t.Fatalf("Lookup(scope=project) after Put(scope=\"\") = (%q, %v), want (fact-1, true)", factID, ok)
	}
}

// ─── Task 12 hardening (C2): RemoveByFactID ─────────────────────────────────

func TestTopicIndex_RemoveByFactID_RemovesAllMatchingEntriesOnly(t *testing.T) {
	ti, err := LoadTopicIndex(t.TempDir() + "/topics.json")
	if err != nil {
		t.Fatalf("LoadTopicIndex: %v", err)
	}
	// Simulate index drift: two different keys both (incorrectly) pointing at
	// the same fact, plus one key pointing at a different fact.
	ti.FactByKey["proj|global|a"] = "fact-1"
	ti.FactByKey["proj|personal|b"] = "fact-1"
	ti.FactByKey["proj|global|c"] = "fact-2"

	removed, err := ti.RemoveByFactID("fact-1")
	if err != nil {
		t.Fatalf("RemoveByFactID: %v", err)
	}
	if !removed {
		t.Fatal("RemoveByFactID(fact-1) = false, want true (two entries pointed at it)")
	}
	if _, ok := ti.FactByKey["proj|global|a"]; ok {
		t.Fatal("proj|global|a still present after RemoveByFactID(fact-1)")
	}
	if _, ok := ti.FactByKey["proj|personal|b"]; ok {
		t.Fatal("proj|personal|b still present after RemoveByFactID(fact-1)")
	}
	if v, ok := ti.FactByKey["proj|global|c"]; !ok || v != "fact-2" {
		t.Fatalf("unrelated entry proj|global|c was disturbed: %q, %v", v, ok)
	}
}

func TestTopicIndex_RemoveByFactID_NoopWhenNothingPoints(t *testing.T) {
	ti, err := LoadTopicIndex(t.TempDir() + "/topics.json")
	if err != nil {
		t.Fatalf("LoadTopicIndex: %v", err)
	}
	if err := ti.Put("proj", "global", "arch/db", "fact-1"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	removed, err := ti.RemoveByFactID("does-not-exist")
	if err != nil {
		t.Fatalf("RemoveByFactID: %v", err)
	}
	if removed {
		t.Fatal("RemoveByFactID(unknown) = true, want false (no-op)")
	}
	// Untouched entry must still resolve.
	if factID, ok := ti.Lookup("proj", "global", "arch/db"); !ok || factID != "fact-1" {
		t.Fatalf("unrelated entry disturbed by no-op RemoveByFactID: (%q, %v)", factID, ok)
	}
}
