package diago

import "testing"

func TestParseInferTypeArgsOutput(t *testing.T) {
	output := `/repo/main.go:6:14-19: unnecessary type arguments
/repo/other.go:10:2-8: simplify slice expression
/repo/main.go:20:5-9: unnecessary type arguments
/repo/z.go:3:1-2: unnecessary type arguments here`

	findings := parseInferTypeArgsOutput(output)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}
	for _, f := range findings {
		if f.Rule != "infertypeargs" || f.Severity != "low" || f.Message != inferTypeArgsMessage {
			t.Fatalf("unexpected finding: %+v", f)
		}
	}
	if findings[0].File != "/repo/main.go" || findings[0].Line != 6 {
		t.Fatalf("first finding pos = %s:%d, want /repo/main.go:6", findings[0].File, findings[0].Line)
	}
	if findings[1].Line != 20 {
		t.Fatalf("second finding line = %d, want 20", findings[1].Line)
	}
}

func TestParseInferTypeArgsOutputEmpty(t *testing.T) {
	if findings := parseInferTypeArgsOutput(""); len(findings) != 0 {
		t.Fatalf("got %d findings, want 0", len(findings))
	}
}
