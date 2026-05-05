package diago

import "fmt"

// Recommendation is deterministic advice derived from audit findings.
type Recommendation struct {
	Rule       string       `json:"rule"`
	Severity   string       `json:"severity"`
	Confidence string       `json:"confidence"`
	Message    string       `json:"message"`
	Symbols    []string     `json:"symbols,omitempty"`
	Examples   []ASTFinding `json:"examples,omitempty"`
}

type recommendationTemplate struct {
	severity   string
	confidence string
	message    string
}

var recommendationTemplates = map[string]recommendationTemplate{
	"cyclomatic-complexity":       {"high", "high", "Split complex branching into smaller functions, guard clauses, or table-driven logic."},
	"function-length":             {"medium", "high", "Split long functions by responsibility: validation, orchestration, persistence, and response formatting."},
	"nesting-depth":               {"medium", "high", "Reduce nesting with early returns, helper functions, or clearer state transitions."},
	"parameter-count":             {"medium", "high", "Replace long parameter lists with an options/config struct or domain value."},
	"panic-outside-main":          {"high", "high", "Return an error instead of panicking outside main/test code unless the failure is unrecoverable."},
	"os-exit-outside-main":        {"critical", "high", "Avoid os.Exit outside main. Return errors so callers can clean up and tests can assert behavior."},
	"defer-in-loop":               {"high", "high", "Move the loop body into a helper or close explicitly so resources are released each iteration."},
	"goroutine-in-loop":           {"high", "medium", "Check loop variable capture and use bounded concurrency with context cancellation."},
	"ignored-call-result":         {"medium", "medium", "Handle or document ignored call results. Use explicit comments for intentional best-effort calls."},
	"empty-error-branch":          {"high", "high", "Handle the error, return it, or document why it is intentionally ignored."},
	"swallowed-error":             {"high", "high", "Propagate the error or wrap it with context instead of returning nil/empty results."},
	"recover-outside-defer":       {"high", "high", "Call recover only from a deferred function on the same goroutine."},
	"missing-context-param":       {"medium", "medium", "Accept context.Context and pass it into I/O or long-running operations."},
	"background-context":          {"medium", "medium", "Use caller-provided context instead of creating a root context inside library/service code."},
	"http-client-without-timeout": {"high", "high", "Set http.Client.Timeout or use request contexts with deadlines."},
	"resource-not-closed":         {"high", "medium", "Close opened resources, usually with defer immediately after the error check."},
	"untested-exported-surface":   {"medium", "medium", "Add package tests for exported API or reduce exported surface area."},
	"duplicate-string-literal":    {"low", "low", "Extract repeated semantic strings to constants. Ignore incidental test/log strings."},
	"magic-number":                {"low", "low", "Name repeated numeric literals with constants when they carry domain meaning."},
	"long-switch":                 {"medium", "medium", "Consider a lookup table, strategy map, or smaller validation functions."},
	"long-if-chain":               {"medium", "medium", "Replace long if/else chains with table-driven dispatch or smaller predicates."},
	"large-composite-literal":     {"medium", "medium", "Move large literals to fixtures, generated data, or focused constructors."},
	"naked-return":                {"medium", "high", "Use explicit return values in longer functions with named results."},
	"too-many-returns":            {"medium", "medium", "Review control flow. Keep guard clauses but consolidate repeated exits where it improves clarity."},
	"deep-anonymous-function":     {"medium", "medium", "Name nested callbacks or extract them to helpers."},
	"dead-code":                   {"low", "medium", "Remove unused unexported declarations or add references/tests if they are intentionally retained."},
	"large-file":                  {"medium", "high", "Split large files around cohesive types, handlers, or workflows."},
	"large-package":               {"medium", "medium", "Split large packages by responsibility or internal subdomain."},
}

func BuildRecommendations(findings []ASTFinding, limit int) []Recommendation {
	if limit == 0 {
		limit = 10
	}
	groups := map[string][]ASTFinding{}
	for _, finding := range findings {
		if _, ok := recommendationTemplates[finding.Rule]; ok {
			groups[finding.Rule] = append(groups[finding.Rule], finding)
		}
	}

	var out []Recommendation
	for _, rule := range AuditRuleOrder() {
		items := groups[rule]
		if len(items) == 0 {
			continue
		}
		t := recommendationTemplates[rule]
		examples := items
		if len(examples) > 3 {
			examples = examples[:3]
		}
		symbols := recommendationSymbols(items, 8)
		out = append(out, Recommendation{
			Rule:       rule,
			Severity:   maxSeverity(t.severity, items),
			Confidence: t.confidence,
			Message:    fmt.Sprintf("%s (%d findings)", t.message, len(items)),
			Symbols:    symbols,
			Examples:   examples,
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func recommendationSymbols(findings []ASTFinding, limit int) []string {
	seen := map[string]bool{}
	var out []string
	for _, finding := range findings {
		if finding.Symbol == "" || seen[finding.Symbol] {
			continue
		}
		seen[finding.Symbol] = true
		out = append(out, finding.Symbol)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func maxSeverity(defaultSeverity string, findings []ASTFinding) string {
	if len(findings) == 0 {
		return defaultSeverity
	}
	best := findings[0].Severity
	for _, finding := range findings[1:] {
		if severityRank(finding.Severity) > severityRank(best) {
			best = finding.Severity
		}
	}
	return best
}

func severityRank(severity string) int {
	switch severity {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}
