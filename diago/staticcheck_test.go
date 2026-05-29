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
