package diago

import (
	"encoding/json"
	"fmt"
	"os"
)

// applyBaseline keeps only AST findings absent from the baseline report so the
// audit gates on new debt, then recomputes the pass on what remains.
func applyBaseline(report *AuditReport, baselinePath string) error {
	baseline, err := loadAuditReport(baselinePath)
	if err != nil {
		return err
	}

	// Count per key so repeated findings (e.g. interface{} -> any in one file)
	// keep granularity: only occurrences beyond the baseline count are new.
	baselineCounts := make(map[string]int, len(baseline.ASTFindings))
	for _, f := range baseline.ASTFindings {
		baselineCounts[astFindingKey(f)]++
	}

	currentCounts := make(map[string]int, len(report.ASTFindings))
	newFindings := make([]ASTFinding, 0, len(report.ASTFindings))
	for _, f := range report.ASTFindings {
		key := astFindingKey(f)
		currentCounts[key]++
		if currentCounts[key] > baselineCounts[key] {
			newFindings = append(newFindings, f)
		}
	}

	resolved := 0
	for key, baseCount := range baselineCounts {
		if missing := baseCount - currentCounts[key]; missing > 0 {
			resolved += missing
		}
	}

	report.ASTFindings = newFindings
	report.BaselineApplied = true
	report.NewFindings = len(newFindings)
	report.ResolvedFindings = resolved
	recomputeAuditPass(report)
	return nil
}

// astFindingKey excludes Line so a line-shifting edit elsewhere does not
// resurface a known finding.
func astFindingKey(f ASTFinding) string {
	return f.Rule + "\x00" + f.File + "\x00" + f.Symbol + "\x00" + f.Message
}

func recomputeAuditPass(report *AuditReport) {
	newCriticals := 0
	for _, f := range report.ASTFindings {
		if f.Severity == "critical" {
			newCriticals++
		}
	}
	for i := range report.Checks {
		if report.Checks[i].Name == "ast" {
			report.Checks[i].Passed = newCriticals == 0
		}
	}
	report.OverallPass = true
	for _, c := range report.Checks {
		if !c.Passed {
			report.OverallPass = false
		}
	}
}

func loadAuditReport(path string) (*AuditReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline %s: %w", path, err)
	}
	var report AuditReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parsing baseline %s as JSON (write one with `audit -format json`): %w", path, err)
	}
	return &report, nil
}
