package rules

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShippedRuleFiles ensures every example rule file in rules/ parses and
// compiles.
func TestShippedRuleFiles(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "rules", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no example rule files found")
	}
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		specs, err := ParseSpecs(f)
		_ = f.Close()
		if err != nil {
			t.Errorf("%s: parse failed: %v", file, err)
			continue
		}
		if _, err := Compile(specs); err != nil {
			t.Errorf("%s: compile failed: %v", file, err)
		}
	}
}
