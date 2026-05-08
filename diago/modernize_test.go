package diago

import "testing"

func TestParseModernizeOutput(t *testing.T) {
	t.Run("parses modern gopls modernize json", func(t *testing.T) {
		findings, err := parseModernizeOutput(`go: downloading something
{
  "example/pkg": {
    "rangeint": [
      {
        "posn": "/tmp/project/file.go:12:6",
        "message": "for loop can be modernized using range over int"
      }
    ]
  }
}`)
		if err != nil {
			t.Fatal(err)
		}
		assertModernizeFinding(t, findings, "rangeint")
	})

	t.Run("parses older gopls modernize json", func(t *testing.T) {
		findings, err := parseModernizeOutput(`{
  "example/pkg": {
    "modernize": [
      {
        "category": "rangeint",
        "posn": "/tmp/project/file.go:12:6",
        "message": "for loop can be modernized using range over int"
      }
    ]
  }
}`)
		if err != nil {
			t.Fatal(err)
		}
		assertModernizeFinding(t, findings, "rangeint")
	})
}

func assertModernizeFinding(t *testing.T, findings []ASTFinding, symbol string) {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	finding := findings[0]
	if finding.Rule != "modernize" || finding.Symbol != symbol || finding.File != "/tmp/project/file.go" || finding.Line != 12 {
		t.Fatalf("unexpected finding: %#v", finding)
	}
}
