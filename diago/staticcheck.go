package diago

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type staticcheckDiagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Location struct {
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"location"`
	Message string `json:"message"`
}

func runU1000Audit(workDir, targetPath string) ([]ASTFinding, AuditCheck) {
	args := []string{"run", "honnef.co/go/tools/cmd/staticcheck@latest", "-f", "json", "-checks=U1000", "-fail=", targetPath}
	cmd := exec.Command("go", args...)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String() + stderr.String()
	check := AuditCheck{Name: "u1000", Command: "go " + strings.Join(args, " "), Passed: err == nil, Output: output}
	if err != nil {
		check.Output = fmt.Sprintf("%v\n%s", err, output)
		return nil, check
	}
	findings, parseErr := parseStaticcheckOutput(stdout.String())
	if parseErr != nil {
		check.Passed = false
		check.Output = fmt.Sprintf("parse staticcheck output: %v\n%s", parseErr, output)
		return nil, check
	}
	return findings, check
}

func parseStaticcheckOutput(output string) ([]ASTFinding, error) {
	var findings []ASTFinding
	dec := json.NewDecoder(strings.NewReader(output))
	for dec.More() {
		var diag staticcheckDiagnostic
		if err := dec.Decode(&diag); err != nil {
			return nil, err
		}
		if diag.Code != "U1000" {
			continue
		}
		findings = append(findings, ASTFinding{
			Rule:     "u1000",
			Severity: "low",
			File:     diag.Location.File,
			Line:     diag.Location.Line,
			Symbol:   diag.Code,
			Message:  diag.Message,
		})
	}
	return findings, nil
}
