package main

import (
	"strings"
	"testing"
)

// A -tasks filter produces a PARTIAL L3 grid, so it must never land on the
// default full-grid scorecard path. Guard fires at flag-validation time, before
// any arm materialization or claude spend.
func TestValidateL3Flags(t *testing.T) {
	if err := validateL3Flags("", "fix-001"); err == nil {
		t.Fatal("-tasks without -out must be rejected")
	} else if !strings.Contains(err.Error(), "-tasks requires an explicit -out") {
		t.Errorf("unexpected message: %v", err)
	}
	if err := validateL3Flags("", "  "); err != nil {
		t.Errorf("whitespace-only -tasks is not a filter: %v", err)
	}
	if err := validateL3Flags("", ""); err != nil {
		t.Errorf("no filter + default out must be allowed: %v", err)
	}
	if err := validateL3Flags("eval/results/partial.json", "fix-001"); err != nil {
		t.Errorf("explicit -out must be allowed: %v", err)
	}
}
