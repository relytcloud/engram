package dataset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ds.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRetrievalValid(t *testing.T) {
	p := writeTemp(t, `{"id":"r-001","question":"where is visimap metadata stored?","expected_keywords":[["visimap"],["fdb","foundationdb"]],"category":"architecture"}

{"id":"r-002","question":"what must be regenerated after rebase?","expected_keywords":[["delta_kernel_ffi.h","ffi header"]],"expected_fact_hint":"CLAUDE.md pre-build","category":"gotcha"}
`)
	cases, err := LoadRetrieval(p)
	if err != nil {
		t.Fatalf("LoadRetrieval: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("got %d cases, want 2", len(cases))
	}
	if cases[0].ID != "r-001" || len(cases[0].ExpectedKeywords) != 2 {
		t.Errorf("case 0 parsed wrong: %+v", cases[0])
	}
	if cases[1].ExpectedFactHint != "CLAUDE.md pre-build" {
		t.Errorf("case 1 hint parsed wrong: %+v", cases[1])
	}
}

func TestLoadRetrievalRejectsBad(t *testing.T) {
	bad := map[string]string{
		"dup id":       `{"id":"a","question":"q","expected_keywords":[["k"]],"category":"c"}` + "\n" + `{"id":"a","question":"q2","expected_keywords":[["k"]],"category":"c"}`,
		"empty id":     `{"id":"","question":"q","expected_keywords":[["k"]],"category":"c"}`,
		"no question":  `{"id":"a","question":"","expected_keywords":[["k"]],"category":"c"}`,
		"no keywords":  `{"id":"a","question":"q","expected_keywords":[],"category":"c"}`,
		"empty group":  `{"id":"a","question":"q","expected_keywords":[[]],"category":"c"}`,
		"invalid json": `{not json}`,
	}
	for name, content := range bad {
		if _, err := LoadRetrieval(writeTemp(t, content)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		} else if !strings.Contains(err.Error(), "line 1") && name != "dup id" {
			t.Errorf("%s: error should cite the line, got: %v", name, err)
		}
	}
}
