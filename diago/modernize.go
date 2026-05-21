package diago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type modernizeDiagnostic struct {
	Category string `json:"category"`
	Posn     string `json:"posn"`
	Message  string `json:"message"`
}

func runModernizeAudit(workDir, targetPath string, fix bool) ([]ASTFinding, AuditCheck) {
	args := []string{"run", "golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest", "-json", "-test"}
	if fix {
		args = append(args, "-fix")
	}
	args = append(args, targetPath)
	cmd := exec.Command("go", args...)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	output := out.String()
	check := AuditCheck{Name: "modernize", Command: "go " + strings.Join(args, " "), Passed: err == nil, Output: output}
	if err != nil {
		check.Output = fmt.Sprintf("%v\n%s", err, output)
		return nil, check
	}
	if fix {
		return nil, check
	}
	findings, parseErr := parseModernizeOutput(output)
	if parseErr != nil {
		check.Passed = false
		check.Output = fmt.Sprintf("parse modernize output: %v\n%s", parseErr, output)
		return nil, check
	}
	return findings, check
}

func parseModernizeOutput(output string) ([]ASTFinding, error) {
	output = trimToJSONObject(output)
	if strings.TrimSpace(output) == "" {
		return nil, nil
	}
	var packages map[string]map[string][]modernizeDiagnostic
	if err := json.Unmarshal([]byte(output), &packages); err != nil {
		return nil, err
	}
	var findings []ASTFinding
	seen := map[string]bool{}
	for pkg, analyzers := range packages {
		for category, diagnostics := range analyzers {
			for _, diag := range diagnostics {
				file, line := parseModernizePosition(diag.Posn)
				symbol := category
				if diag.Category != "" {
					symbol = diag.Category
				}
				key := fmt.Sprintf("%s\x00%d\x00%s\x00%s", file, line, symbol, diag.Message)
				if seen[key] {
					continue
				}
				seen[key] = true
				findings = append(findings, ASTFinding{
					Rule:     "modernize",
					Severity: "low",
					Package:  pkg,
					File:     file,
					Line:     line,
					Symbol:   symbol,
					Message:  diag.Message,
				})
			}
		}
	}
	return findings, nil
}

func trimToJSONObject(output string) string {
	idx := strings.Index(output, "{")
	if idx < 0 {
		return strings.TrimSpace(output)
	}
	return output[idx:]
}

func parseModernizePosition(posn string) (string, int) {
	lastColon := strings.LastIndex(posn, ":")
	if lastColon < 0 {
		return posn, 0
	}
	withoutColumn := posn[:lastColon]
	lineColon := strings.LastIndex(withoutColumn, ":")
	if lineColon < 0 {
		return withoutColumn, 0
	}
	line, _ := strconv.Atoi(withoutColumn[lineColon+1:])
	return withoutColumn[:lineColon], line
}
