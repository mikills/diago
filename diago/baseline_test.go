package diago

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBaseline(t *testing.T, report AuditReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baseline.json")
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyBaselineDropsKnownIgnoringLine(t *testing.T) {
	path := writeBaseline(t, AuditReport{ASTFindings: []ASTFinding{
		{Rule: "cyclomatic-complexity", File: "a.go", Symbol: "Foo", Message: "complex", Severity: "high", Line: 10},
	}})

	report := &AuditReport{
		Checks: []AuditCheck{{Name: "ast", Passed: true}},
		ASTFindings: []ASTFinding{
			// Same finding shifted to a new line — must be treated as known.
			{Rule: "cyclomatic-complexity", File: "a.go", Symbol: "Foo", Message: "complex", Severity: "high", Line: 42},
			// Genuinely new.
			{Rule: "large-file", File: "b.go", Message: "1000 lines", Severity: "critical", Line: 1},
		},
	}
	if err := applyBaseline(report, path); err != nil {
		t.Fatal(err)
	}

	if len(report.ASTFindings) != 1 || report.ASTFindings[0].Rule != "large-file" {
		t.Fatalf("expected only the new large-file finding, got %+v", report.ASTFindings)
	}
	if report.NewFindings != 1 || !report.BaselineApplied {
		t.Fatalf("NewFindings=%d BaselineApplied=%v", report.NewFindings, report.BaselineApplied)
	}
}

func TestApplyBaselineGatesNewCriticalsOnly(t *testing.T) {
	crit := ASTFinding{Rule: "large-file", File: "huge.go", Message: "5000 lines", Severity: "critical", Line: 1}
	path := writeBaseline(t, AuditReport{ASTFindings: []ASTFinding{crit}})

	// A baseline critical must not fail the audit.
	known := &AuditReport{Checks: []AuditCheck{{Name: "ast", Passed: false}}, ASTFindings: []ASTFinding{crit}}
	if err := applyBaseline(known, path); err != nil {
		t.Fatal(err)
	}
	if !known.OverallPass {
		t.Error("a baseline critical must not fail the audit")
	}

	// A new critical must fail it.
	added := &AuditReport{
		Checks: []AuditCheck{{Name: "ast", Passed: false}},
		ASTFindings: []ASTFinding{
			crit,
			{Rule: "large-file", File: "new.go", Message: "3000 lines", Severity: "critical", Line: 1},
		},
	}
	if err := applyBaseline(added, path); err != nil {
		t.Fatal(err)
	}
	if added.OverallPass {
		t.Error("a new critical must fail the audit")
	}
}

func TestApplyBaselineKeepsCommandFailures(t *testing.T) {
	path := writeBaseline(t, AuditReport{})
	report := &AuditReport{Checks: []AuditCheck{
		{Name: "test", Passed: false}, // a real test failure, unrelated to findings
		{Name: "ast", Passed: true},
	}}
	if err := applyBaseline(report, path); err != nil {
		t.Fatal(err)
	}
	if report.OverallPass {
		t.Error("a failing test check must keep the audit failing under baseline")
	}
}

func TestApplyBaselineCountsResolved(t *testing.T) {
	path := writeBaseline(t, AuditReport{ASTFindings: []ASTFinding{
		{Rule: "r1", File: "a.go", Message: "m1"},
		{Rule: "r2", File: "b.go", Message: "m2"},
	}})
	report := &AuditReport{
		Checks:      []AuditCheck{{Name: "ast", Passed: true}},
		ASTFindings: []ASTFinding{{Rule: "r1", File: "a.go", Message: "m1"}},
	}
	if err := applyBaseline(report, path); err != nil {
		t.Fatal(err)
	}
	if report.ResolvedFindings != 1 || report.NewFindings != 0 {
		t.Fatalf("resolved=%d new=%d, want 1/0", report.ResolvedFindings, report.NewFindings)
	}
}

func TestRelativizeReport(t *testing.T) {
	report := &AuditReport{
		ASTFindings: []ASTFinding{
			{File: "/repo/backend/internal/app/main.go"},
			{File: "/repo/backend/x.go"},
			{File: ""},
			{File: "/elsewhere/y.go"}, // outside workDir, left untouched
		},
		Checks: []AuditCheck{
			{Name: "ast", Output: "function-length /repo/backend/main.go:68 too long"},
			{Name: "u1000", Output: `{"file":"/repo/backend/internal/rag/p.go"}`},
		},
	}
	relativizeReport(report, "/repo/backend")

	wantFiles := []string{"internal/app/main.go", "x.go", "", "/elsewhere/y.go"}
	for i, w := range wantFiles {
		if report.ASTFindings[i].File != w {
			t.Errorf("findings[%d].File = %q, want %q", i, report.ASTFindings[i].File, w)
		}
	}
	for _, c := range report.Checks {
		if strings.Contains(c.Output, "/repo/backend") {
			t.Errorf("check %q output still has an absolute path: %q", c.Name, c.Output)
		}
	}
	if report.Checks[0].Output != "function-length main.go:68 too long" {
		t.Errorf("ast output = %q", report.Checks[0].Output)
	}
}

func TestSortFindingsIsDeterministic(t *testing.T) {
	findings := []ASTFinding{
		{File: "b.go", Line: 2, Rule: "r"},
		{File: "a.go", Line: 10, Rule: "r"},
		{File: "a.go", Line: 2, Rule: "z"},
		{File: "a.go", Line: 2, Rule: "a"},
	}
	sortFindings(findings)

	want := []ASTFinding{
		{File: "a.go", Line: 2, Rule: "a"},
		{File: "a.go", Line: 2, Rule: "z"},
		{File: "a.go", Line: 10, Rule: "r"},
		{File: "b.go", Line: 2, Rule: "r"},
	}
	for i := range want {
		if findings[i] != want[i] {
			t.Errorf("position %d = %+v, want %+v", i, findings[i], want[i])
		}
	}
}

func TestLoadAuditReportErrors(t *testing.T) {
	if _, err := loadAuditReport(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("expected error for missing baseline")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("not json"), 0o644)
	if _, err := loadAuditReport(bad); err == nil {
		t.Error("expected error for invalid JSON baseline")
	}
}
