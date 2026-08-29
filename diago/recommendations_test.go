package diago

import "testing"

func TestRecommendations(t *testing.T) {
	scanner := ASTFinding{
		Rule: "scanner-err", Severity: "high", File: "scan.go", Line: 8, Symbol: "ScanLines",
		Message: "bufio.Scanner iteration may hide an error; call scanner.Err() after the loop",
	}
	firstRows := ASTFinding{
		Rule: "sql-rows-err", Severity: "high", File: "users.go", Line: 16, Symbol: "ListUsers",
		Message: "database/sql.Rows iteration may hide an error; call rows.Err() after the loop",
	}
	secondRows := ASTFinding{
		Rule: "sql-rows-err", Severity: "high", File: "orders.go", Line: 22, Symbol: "ListOrders",
		Message: "database/sql.Rows iteration may hide an error; call rows.Err() after the loop",
	}
	duplicateRows := ASTFinding{
		Rule: "sql-rows-err", Severity: "high", File: "users.go", Line: 30, Symbol: "ListUsers",
		Message: "database/sql.Rows iteration may hide an error; call rows.Err() after the loop",
	}
	thirdRows := ASTFinding{
		Rule: "sql-rows-err", Severity: "high", File: "events.go", Line: 10, Symbol: "ListEvents",
		Message: "database/sql.Rows iteration may hide an error; call rows.Err() after the loop",
	}

	tests := []struct {
		name     string
		findings []ASTFinding
		limit    int
		want     []Recommendation
	}{
		{
			name:     "groups iterator findings in audit order",
			findings: []ASTFinding{scanner, firstRows, secondRows, duplicateRows, thirdRows},
			want: []Recommendation{
				{
					Rule:       "sql-rows-err",
					Severity:   "high",
					Confidence: "high",
					Message:    "Call rows.Err after iterating database/sql rows so deferred iteration errors are not lost. (4 findings)",
					Symbols:    []string{"ListUsers", "ListOrders", "ListEvents"},
					Examples:   []ASTFinding{firstRows, secondRows, duplicateRows},
				},
				{
					Rule:       "scanner-err",
					Severity:   "high",
					Confidence: "high",
					Message:    "Call scanner.Err after scanning so tokenization and I/O errors are not lost. (1 finding)",
					Symbols:    []string{"ScanLines"},
					Examples:   []ASTFinding{scanner},
				},
			},
		},
		{
			name:     "limits ordered recommendations",
			findings: []ASTFinding{scanner, firstRows},
			limit:    1,
			want: []Recommendation{{
				Rule:       "sql-rows-err",
				Severity:   "high",
				Confidence: "high",
				Message:    "Call rows.Err after iterating database/sql rows so deferred iteration errors are not lost. (1 finding)",
				Symbols:    []string{"ListUsers"},
				Examples:   []ASTFinding{firstRows},
			}},
		},
		{
			name:     "omits rules without a template",
			findings: []ASTFinding{{Rule: "unknown", Severity: "high", Symbol: "Ignore"}},
			want:     nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := BuildRecommendations(test.findings, test.limit)
			assertRecommendations(t, got, test.want)
		})
	}
}

func TestBaselineRecommendations(t *testing.T) {
	known := ASTFinding{
		Rule: "scanner-err", Severity: "high", File: "scan.go", Line: 8, Symbol: "ScanLines",
		Message: "bufio.Scanner iteration may hide an error; call scanner.Err() after the loop",
	}
	added := ASTFinding{
		Rule: "sql-rows-err", Severity: "high", File: "users.go", Line: 16, Symbol: "ListUsers",
		Message: "database/sql.Rows iteration may hide an error; call rows.Err() after the loop",
	}
	report := &AuditReport{ASTFindings: []ASTFinding{known, added}}
	if err := applyBaseline(report, writeBaseline(t, AuditReport{ASTFindings: []ASTFinding{known}})); err != nil {
		t.Fatal(err)
	}

	got := BuildRecommendations(report.ASTFindings, 0)
	want := []Recommendation{{
		Rule:       "sql-rows-err",
		Severity:   "high",
		Confidence: "high",
		Message:    "Call rows.Err after iterating database/sql rows so deferred iteration errors are not lost. (1 finding)",
		Symbols:    []string{"ListUsers"},
		Examples:   []ASTFinding{added},
	}}
	assertRecommendations(t, got, want)
}

func assertRecommendations(t *testing.T, got, want []Recommendation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recommendations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Rule != want[i].Rule || got[i].Severity != want[i].Severity || got[i].Confidence != want[i].Confidence || got[i].Message != want[i].Message {
			t.Errorf("recommendations[%d] = %+v, want %+v", i, got[i], want[i])
		}
		assertRecommendationStrings(t, "symbols", got[i].Symbols, want[i].Symbols)
		if len(got[i].Examples) != len(want[i].Examples) {
			t.Errorf("recommendations[%d].Examples = %+v, want %+v", i, got[i].Examples, want[i].Examples)
			continue
		}
		for j := range want[i].Examples {
			if got[i].Examples[j] != want[i].Examples[j] {
				t.Errorf("recommendations[%d].Examples[%d] = %+v, want %+v", i, j, got[i].Examples[j], want[i].Examples[j])
			}
		}
	}
}

func assertRecommendationStrings(t *testing.T, field string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %q, want %q", field, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", field, i, got[i], want[i])
		}
	}
}
