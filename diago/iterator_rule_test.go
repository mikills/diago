package diago

import (
	"strings"
	"testing"
)

func TestIteratorIncompleteRule(t *testing.T) {
	const rule = "iterator-analysis-incomplete"
	descriptor, ok := descriptorForRule(rule)
	if !ok || descriptor.Kind != "analysis" || descriptor.Source != "native" || descriptor.Severity != "medium" || !descriptor.DefaultEnabled || descriptor.FixSafety != FixSafetyNone {
		t.Fatalf("invalid analysis-warning metadata: %+v", descriptor)
	}
	count := 0
	for _, id := range AuditRuleOrder() {
		if id == rule {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("audit order contains rule %d times", count)
	}
	finding := astFinding(rule, "high", astLocation{file: "p.go", line: 1}, "work", "analysis budget exceeded")
	if finding.Severity != "medium" {
		t.Fatalf("analysis warning is not a code hazard: %+v", finding)
	}
	recommendations := BuildRecommendations([]ASTFinding{finding}, 0)
	if len(recommendations) != 1 || recommendations[0].Rule != rule || recommendations[0].Severity != "medium" || !strings.Contains(recommendations[0].Message, "manually") {
		t.Fatalf("missing actionable incompleteness guidance: %+v", recommendations)
	}
}
