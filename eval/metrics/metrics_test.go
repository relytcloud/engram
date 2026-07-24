package metrics

import "testing"

func TestRecallAtK(t *testing.T) {
	ranks := []int{1, 3, 0, 11} // 0 = miss
	cases := []struct {
		k    int
		want float64
	}{{1, 0.25}, {5, 0.5}, {10, 0.5}, {11, 0.75}}
	for _, c := range cases {
		if got := RecallAtK(ranks, c.k); got != c.want {
			t.Errorf("RecallAtK(k=%d) = %v, want %v", c.k, got, c.want)
		}
	}
	if got := RecallAtK(nil, 5); got != 0 {
		t.Errorf("RecallAtK(empty) = %v, want 0", got)
	}
}

func TestMRR(t *testing.T) {
	got := MRR([]int{1, 2, 0, 4}) // (1 + 0.5 + 0 + 0.25) / 4
	if want := 0.4375; got != want {
		t.Errorf("MRR = %v, want %v", got, want)
	}
}

func TestApproxTokens(t *testing.T) {
	if got := ApproxTokens("abcd"); got != 1 {
		t.Errorf("ApproxTokens(4 bytes) = %d, want 1", got)
	}
	if got := ApproxTokens("abcde"); got != 2 {
		t.Errorf("ApproxTokens(5 bytes) = %d, want 2", got)
	}
	if got := ApproxTokens(""); got != 0 {
		t.Errorf("ApproxTokens(empty) = %d, want 0", got)
	}
}

func TestHitsKeywordGroups(t *testing.T) {
	text := "ZDB stores visimap metadata in FDB via GMetaService"
	if !HitsKeywordGroups(text, [][]string{{"visimap"}, {"fdb", "foundationdb"}}) {
		t.Error("expected hit: both groups satisfied")
	}
	if HitsKeywordGroups(text, [][]string{{"visimap"}, {"parquet"}}) {
		t.Error("expected miss: second group unsatisfied")
	}
	if !HitsKeywordGroups("VISIMAP", [][]string{{"visimap"}}) {
		t.Error("expected case-insensitive hit")
	}
	if HitsKeywordGroups(text, nil) {
		t.Error("expected miss on empty groups (vacuous hit would poison recall)")
	}
}
