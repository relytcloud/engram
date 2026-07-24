package tokenmeter

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeCtx struct{ out string }

func (f fakeCtx) FormatContext(project, scope string) (string, error) { return f.out, nil }

func TestScriptOutputTokens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.sh")
	// 8 bytes of stdout -> 2 approx tokens
	if err := os.WriteFile(p, []byte("printf 'abcdefgh'"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ScriptOutputTokens(p, os.Environ())
	if err != nil {
		t.Fatalf("ScriptOutputTokens: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d tokens, want 2", got)
	}
}

func TestContextTokens(t *testing.T) {
	got, err := ContextTokens(fakeCtx{out: "abcd"}, "phoenix")
	if err != nil || got != 1 {
		t.Errorf("ContextTokens = %d, %v; want 1, nil", got, err)
	}
}

func TestComposite(t *testing.T) {
	if got := Composite(100, 50, 200, 3); got != 750 {
		t.Errorf("Composite = %v, want 750", got)
	}
}
