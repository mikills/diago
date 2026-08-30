package diago

import "testing"

func TestParseStaticcheckOutput(t *testing.T) {
	t.Run("parses u1000 json lines", func(t *testing.T) {
		findings, err := parseStaticcheckOutput(`{"code":"U1000","severity":"warning","location":{"file":"/tmp/sample.go","line":2,"column":6},"message":"func unused is unused"}
{"code":"S1000","severity":"warning","location":{"file":"/tmp/sample.go","line":3,"column":6},"message":"other"}
`)
		if err != nil {
			t.Fatal(err)
		}
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		finding := findings[0]
		if finding.Rule != "u1000" || finding.File != "/tmp/sample.go" || finding.Line != 2 || finding.Message != "func unused is unused" {
			t.Fatalf("unexpected finding: %#v", finding)
		}
	})
}

func TestCuratedStaticcheckOutput(t *testing.T) {
	findings, err := parseCuratedStaticcheckOutput(`{"code":"SA2000","severity":"warning","location":{"file":"/tmp/wait.go","line":8,"column":2},"message":"sync.WaitGroup.Add should not be called from inside a goroutine"}
{"code":"SA4006","severity":"warning","location":{"file":"/tmp/value.go","line":12,"column":2},"message":"this value of err is never used"}
{"code":"U1000","severity":"warning","location":{"file":"/tmp/unused.go","line":2,"column":6},"message":"func unused is unused"}
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2", len(findings))
	}
	want := []ASTFinding{
		{Rule: "staticcheck/SA2000", Severity: "high", File: "/tmp/wait.go", Line: 8, Symbol: "SA2000", Message: "sync.WaitGroup.Add should not be called from inside a goroutine"},
		{Rule: "staticcheck/SA4006", Severity: "high", File: "/tmp/value.go", Line: 12, Symbol: "SA4006", Message: "this value of err is never used"},
	}
	for i := range want {
		if findings[i] != want[i] {
			t.Errorf("findings[%d] = %#v, want %#v", i, findings[i], want[i])
		}
	}
	if got := staticcheckCodes(curatedStaticcheckProfile.Rules); got != "SA2000,SA4006,SA4010,SA5001,SA5010" {
		t.Fatalf("curated check codes = %q", got)
	}
}
