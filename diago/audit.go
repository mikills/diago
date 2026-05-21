package diago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// AuditConfig controls built-in Go diagnostics that do not require external tools.
type AuditConfig struct {
	TargetPath   string       `json:"target_path"`
	OutputFile   string       `json:"output_file"`
	Format       OutputFormat `json:"format"`
	Race         bool         `json:"race"`
	Coverage     bool         `json:"coverage"`
	Deps         bool         `json:"deps"`
	AST          bool         `json:"ast"`
	Modernize    bool         `json:"modernize"`
	ModernizeFix bool         `json:"modernize_fix"`
	SummaryLimit int          `json:"summary_limit"`
}

// AuditReport contains diagnostics from Go toolchain-only checks.
type AuditReport struct {
	Target          string           `json:"target"`
	OverallPass     bool             `json:"overall_pass"`
	Checks          []AuditCheck     `json:"checks"`
	Summary         AuditSummary     `json:"summary"`
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	Coverage        *CoverageReport  `json:"coverage,omitempty"`
	Dependencies    []string         `json:"dependencies,omitempty"`
	ASTFindings     []ASTFinding     `json:"ast_findings,omitempty"`
}

// AuditSummary is a compact rollup for humans and agent consumption.
type AuditSummary struct {
	ChecksPassed      int               `json:"checks_passed"`
	ChecksFailed      int               `json:"checks_failed"`
	ASTTotal          int               `json:"ast_total"`
	ASTBySeverity     map[string]int    `json:"ast_by_severity,omitempty"`
	ASTByRule         map[string]int    `json:"ast_by_rule,omitempty"`
	CriticalHigh      []ASTFinding      `json:"critical_high,omitempty"`
	CoverageTotalPct  float64           `json:"coverage_total_pct,omitempty"`
	DependencyCount   int               `json:"dependency_count,omitempty"`
	FailedCheckOutput map[string]string `json:"failed_check_output,omitempty"`
}

// AuditCheck is the result of one command-backed diagnostic.
type AuditCheck struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
	Output  string `json:"output"`
}

// CoverageReport summarizes go test coverage output.
type CoverageReport struct {
	TotalPct float64        `json:"total_pct"`
	Files    []CoverageFile `json:"files"`
}

// CoverageFile is one row from go tool cover -func.
type CoverageFile struct {
	Name string  `json:"name"`
	Pct  float64 `json:"pct"`
}

func (c *AuditConfig) defaults() {
	if c.TargetPath == "" {
		c.TargetPath = "./..."
	}
	if c.OutputFile == "" {
		c.OutputFile = ".diago/audit.txt"
	}
	if c.Format == "" {
		c.Format = FormatText
	}
	if c.SummaryLimit == 0 {
		c.SummaryLimit = 25
	}
}

// RunAudit executes Go toolchain-only diagnostics and writes a report.
func RunAudit(cfg AuditConfig) (*AuditReport, error) {
	cfg.defaults()

	workDir, targetPath, err := resolveTarget(cfg.TargetPath)
	if err != nil {
		return nil, err
	}

	report := &AuditReport{Target: cfg.TargetPath, OverallPass: true}

	report.addCheck(runAuditCommand(workDir, "test", "go", "test", targetPath))
	report.addCheck(runAuditCommand(workDir, "vet", "go", "vet", targetPath))
	if cfg.Race {
		report.addCheck(runAuditCommand(workDir, "race", "go", "test", "-race", targetPath))
	}

	if cfg.Coverage {
		coverage, check := runCoverage(workDir, targetPath)
		report.addCheck(check)
		report.Coverage = coverage
	}

	if cfg.Deps {
		deps, check := runDeps(workDir, targetPath)
		report.addCheck(check)
		report.Dependencies = deps
	}

	if cfg.AST {
		findings, check := runASTAudit(workDir, targetPath)
		report.addCheck(check)
		report.ASTFindings = findings
	}

	if cfg.Modernize || cfg.ModernizeFix {
		findings, check := runModernizeAudit(workDir, targetPath, cfg.ModernizeFix)
		report.addCheck(check)
		report.ASTFindings = append(report.ASTFindings, findings...)
	}

	report.Summary = buildAuditSummary(report, cfg.SummaryLimit)
	report.Recommendations = BuildRecommendations(report.ASTFindings, cfg.SummaryLimit)

	if err := WriteAuditReport(cfg.OutputFile, report, cfg.Format); err != nil {
		return nil, fmt.Errorf("writing audit report: %w", err)
	}
	return report, nil
}

func (r *AuditReport) addCheck(check AuditCheck) {
	r.Checks = append(r.Checks, check)
	if !check.Passed {
		r.OverallPass = false
	}
}

func runAuditCommand(workDir, name string, args ...string) AuditCheck {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return AuditCheck{
		Name:    name,
		Command: strings.Join(args, " "),
		Passed:  err == nil,
		Output:  out.String(),
	}
}

func runCoverage(workDir, target string) (*CoverageReport, AuditCheck) {
	tmpDir, err := os.MkdirTemp("", "diago-audit-*")
	if err != nil {
		return nil, AuditCheck{Name: "coverage", Command: "mkdir temp", Passed: false, Output: err.Error()}
	}
	defer os.RemoveAll(tmpDir)

	profile := filepath.Join(tmpDir, "coverage.out")
	check := runAuditCommand(workDir, "coverage", "go", "test", "-coverprofile", profile, target)
	if !check.Passed {
		return nil, check
	}

	cmd := exec.Command("go", "tool", "cover", "-func", profile)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	check.Command += " && go tool cover -func coverage.out"
	check.Output += out.String()
	check.Passed = err == nil
	if err != nil {
		return nil, check
	}

	return parseCoverageFunc(out.String()), check
}

func parseCoverageFunc(text string) *CoverageReport {
	report := &CoverageReport{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pctText := strings.TrimSuffix(fields[len(fields)-1], "%")
		pct, err := strconv.ParseFloat(pctText, 64)
		if err != nil {
			continue
		}
		if fields[0] == "total:" {
			report.TotalPct = pct
			continue
		}
		report.Files = append(report.Files, CoverageFile{Name: fields[0], Pct: pct})
	}
	return report
}

func buildAuditSummary(report *AuditReport, criticalHighLimit int) AuditSummary {
	s := AuditSummary{
		ASTBySeverity:     map[string]int{},
		ASTByRule:         map[string]int{},
		FailedCheckOutput: map[string]string{},
	}
	addCheckSummary(&s, report.Checks)
	addCoverageSummary(&s, report)
	addASTSummary(&s, report.ASTFindings, criticalHighLimit)
	compactEmptySummaryMaps(&s)
	return s
}

func addCheckSummary(s *AuditSummary, checks []AuditCheck) {
	for _, check := range checks {
		if check.Passed {
			s.ChecksPassed++
			continue
		}
		s.ChecksFailed++
		s.FailedCheckOutput[check.Name] = firstLines(check.Output, 25)
	}
}

func addCoverageSummary(s *AuditSummary, report *AuditReport) {
	if report.Coverage != nil {
		s.CoverageTotalPct = report.Coverage.TotalPct
	}
	s.DependencyCount = len(report.Dependencies)
}

func addASTSummary(s *AuditSummary, findings []ASTFinding, criticalHighLimit int) {
	s.ASTTotal = len(findings)
	for _, f := range findings {
		s.ASTBySeverity[f.Severity]++
		s.ASTByRule[f.Rule]++
		if shouldIncludeCriticalHigh(f, len(s.CriticalHigh), criticalHighLimit) {
			s.CriticalHigh = append(s.CriticalHigh, f)
		}
	}
}

func shouldIncludeCriticalHigh(f ASTFinding, current, limit int) bool {
	if f.Severity != "critical" && f.Severity != "high" {
		return false
	}
	return limit < 0 || current < limit
}

func compactEmptySummaryMaps(s *AuditSummary) {
	if len(s.ASTBySeverity) == 0 {
		s.ASTBySeverity = nil
	}
	if len(s.ASTByRule) == 0 {
		s.ASTByRule = nil
	}
	if len(s.FailedCheckOutput) == 0 {
		s.FailedCheckOutput = nil
	}
}

func firstLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n... %d more lines", len(lines)-n)
}

func runASTAudit(workDir, target string) ([]ASTFinding, AuditCheck) {
	findings, err := AnalyzeAST(workDir, target)
	var out bytes.Buffer
	if err != nil {
		return nil, AuditCheck{Name: "ast", Command: "native ast analysis", Passed: false, Output: err.Error()}
	}
	critical := 0
	high := 0
	for _, f := range findings {
		if f.Severity == "critical" {
			critical++
		}
		if f.Severity == "high" {
			high++
		}
	}
	fmt.Fprintf(&out, "ast findings: %d (critical=%d, high=%d)\n", len(findings), critical, high)
	for i, f := range findings {
		if i >= 50 {
			fmt.Fprintf(&out, "... %d more findings\n", len(findings)-i)
			break
		}
		fmt.Fprintf(&out, "%s [%s] %s:%d %s %s\n", f.Rule, f.Severity, f.File, f.Line, f.Symbol, f.Message)
	}
	return findings, AuditCheck{Name: "ast", Command: "native ast analysis", Passed: critical == 0, Output: out.String()}
}

func runDeps(workDir, target string) ([]string, AuditCheck) {
	check := runAuditCommand(workDir, "deps", "go", "list", "-deps", target)
	if !check.Passed {
		return nil, check
	}
	var deps []string
	for _, line := range strings.Split(check.Output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			deps = append(deps, line)
		}
	}
	return deps, check
}

// WriteAuditReport writes audit results to disk.
func WriteAuditReport(path string, report *AuditReport, format OutputFormat) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	if format == FormatJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0644)
	}
	return writeAuditText(path, report)
}

func writeAuditText(path string, report *AuditReport) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "=== diago audit ===\n")
	fmt.Fprintf(&buf, "target: %s\n", report.Target)
	fmt.Fprintf(&buf, "overall pass: %t\n\n", report.OverallPass)
	writeAuditSummaryText(&buf, report)

	for _, check := range report.Checks {
		status := "PASS"
		if !check.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(&buf, "--- %s [%s] ---\n", check.Name, status)
		fmt.Fprintf(&buf, "$ %s\n", check.Command)
		if strings.TrimSpace(check.Output) == "" {
			fmt.Fprintf(&buf, "(no output)\n\n")
		} else {
			fmt.Fprintf(&buf, "%s\n", check.Output)
		}
	}

	if report.Coverage != nil {
		fmt.Fprintf(&buf, "--- coverage summary ---\n")
		fmt.Fprintf(&buf, "total: %.1f%%\n", report.Coverage.TotalPct)
	}
	if len(report.Dependencies) > 0 {
		fmt.Fprintf(&buf, "--- dependencies ---\n")
		fmt.Fprintf(&buf, "count: %d\n", len(report.Dependencies))
	}
	if len(report.ASTFindings) > 0 {
		fmt.Fprintf(&buf, "--- ast findings ---\n")
		for _, f := range report.ASTFindings {
			fmt.Fprintf(&buf, "%s [%s] %s:%d %s %s\n", f.Rule, f.Severity, f.File, f.Line, f.Symbol, f.Message)
		}
	}

	return os.WriteFile(path, buf.Bytes(), 0644)
}

func AuditRuleOrder() []string {
	return []string{
		"cyclomatic-complexity",
		"function-length",
		"nesting-depth",
		"parameter-count",
		"panic-outside-main",
		"os-exit-outside-main",
		"defer-in-loop",
		"goroutine-in-loop",
		"comment-debt",
		"ignored-call-result",
		"empty-error-branch",
		"swallowed-error",
		"recover-outside-defer",
		"missing-context-param",
		"background-context",
		"http-client-without-timeout",
		"resource-not-closed",
		"untested-exported-surface",
		"duplicate-string-literal",
		"magic-number",
		"long-switch",
		"long-if-chain",
		"large-composite-literal",
		"naked-return",
		"too-many-returns",
		"deep-anonymous-function",
		"dead-code",
		"large-file",
		"large-package",
		"long-test-name",
		"modernize",
	}
}

func writeCountTable(buf *bytes.Buffer, title string, order []string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Fprintf(buf, "%s:\n", title)
	seen := map[string]bool{}
	for _, key := range order {
		if count := counts[key]; count > 0 {
			fmt.Fprintf(buf, "  %-28s %d\n", key, count)
			seen[key] = true
		}
	}
	for key, count := range counts {
		if !seen[key] {
			fmt.Fprintf(buf, "  %-28s %d\n", key, count)
		}
	}
}

func writeAuditSummaryText(buf *bytes.Buffer, report *AuditReport) {
	s := report.Summary
	fmt.Fprintf(buf, "--- summary ---\n")
	fmt.Fprintf(buf, "checks: passed=%d failed=%d\n", s.ChecksPassed, s.ChecksFailed)
	if report.Coverage != nil {
		fmt.Fprintf(buf, "coverage: %.1f%%\n", s.CoverageTotalPct)
	}
	if s.DependencyCount > 0 {
		fmt.Fprintf(buf, "dependencies: %d\n", s.DependencyCount)
	}
	if s.ASTTotal > 0 {
		fmt.Fprintf(buf, "ast findings: %d\n", s.ASTTotal)
		writeCountTable(buf, "by severity", []string{"critical", "high", "medium", "low"}, s.ASTBySeverity)
		writeCountTable(buf, "by rule", AuditRuleOrder(), s.ASTByRule)
		fmt.Fprintf(buf, "critical/high findings:\n")
		for _, f := range s.CriticalHigh {
			fmt.Fprintf(buf, "  %s [%s] %s:%d %s %s\n", f.Rule, f.Severity, f.File, f.Line, f.Symbol, f.Message)
		}
	}
	if len(report.Recommendations) > 0 {
		fmt.Fprintf(buf, "recommendations:\n")
		for _, rec := range report.Recommendations {
			fmt.Fprintf(buf, "  %s [%s/%s] %s\n", rec.Rule, rec.Severity, rec.Confidence, rec.Message)
			if len(rec.Symbols) > 0 {
				fmt.Fprintf(buf, "    symbols: %s\n", strings.Join(rec.Symbols, ", "))
			}
		}
	}
	if len(s.FailedCheckOutput) > 0 {
		fmt.Fprintf(buf, "failed checks:\n")
		for name, output := range s.FailedCheckOutput {
			fmt.Fprintf(buf, "  %s:\n%s\n", name, output)
		}
	}
	fmt.Fprintf(buf, "\n")
}
