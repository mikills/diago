package diago

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveDeadCodeFindings(t *testing.T) {
	t.Run("removes unexported dead functions only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sample.go")
		content := `package sample

func deadHelper() int {
	return 1
}

// keep documented hooks
func documentedHook() {}

func live() int {
	return deadHelper()
}
`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		check := removeDeadCodeFindings([]ASTFinding{
			{Rule: "dead-code", File: path, Line: 3, Symbol: "deadHelper", Message: "unexported function deadHelper appears unused within package"},
			{Rule: "dead-code", File: path, Line: 8, Symbol: "documentedHook", Message: "unexported function documentedHook appears unused within package"},
		})
		if !check.Passed {
			t.Fatal(check.Output)
		}
		gotBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := string(gotBytes)
		if strings.Contains(got, "func deadHelper") {
			t.Fatalf("deadHelper was not removed:\n%s", got)
		}
		if !strings.Contains(got, "func documentedHook") {
			t.Fatalf("documented function should remain:\n%s", got)
		}
	})
}
