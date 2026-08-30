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

const staticcheckVersion = "v0.8.1"

type staticcheckRule struct {
	Code           string
	Rule           string
	Severity       string
	Recommendation string
}

type staticcheckProfile struct {
	CheckName string
	Rules     []staticcheckRule
}

var u1000StaticcheckProfile = staticcheckProfile{
	CheckName: "u1000",
	Rules:     []staticcheckRule{{Code: "U1000", Rule: "u1000", Severity: "low"}},
}

var curatedStaticcheckProfile = staticcheckProfile{
	CheckName: "staticcheck",
	Rules: []staticcheckRule{
		{Code: "SA2000", Rule: "staticcheck/SA2000", Severity: "high", Recommendation: "Call WaitGroup.Add before starting the goroutine so the counter cannot race with Wait."},
		{Code: "SA4006", Rule: "staticcheck/SA4006", Severity: "high", Recommendation: "Use or return the overwritten value; an error check or dead assignment may be missing."},
		{Code: "SA4010", Rule: "staticcheck/SA4010", Severity: "high", Recommendation: "Assign append's result so the updated slice is observed."},
		{Code: "SA5001", Rule: "staticcheck/SA5001", Severity: "high", Recommendation: "Check the open error before deferring Close on the returned resource."},
		{Code: "SA5010", Rule: "staticcheck/SA5010", Severity: "high", Recommendation: "Change the assertion or reconcile conflicting interface method signatures; this conversion can never succeed."},
	},
}

func runU1000Audit(workDir, targetPath string) ([]ASTFinding, AuditCheck) {
	return runStaticcheckAudit(workDir, targetPath, u1000StaticcheckProfile)
}

func runCuratedStaticcheckAudit(workDir, targetPath string) ([]ASTFinding, AuditCheck) {
	return runStaticcheckAudit(workDir, targetPath, curatedStaticcheckProfile)
}

func runStaticcheckAudit(workDir, targetPath string, profile staticcheckProfile) ([]ASTFinding, AuditCheck) {
	args := []string{"run", "honnef.co/go/tools/cmd/staticcheck@" + staticcheckVersion, "-f", "json", "-checks=" + staticcheckCodes(profile.Rules), "-fail=", targetPath}
	cmd := exec.Command("go", args...)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String() + stderr.String()
	check := AuditCheck{Name: profile.CheckName, Command: "go " + strings.Join(args, " "), ToolVersion: "staticcheck " + staticcheckVersion, Passed: err == nil, Output: output}
	if err != nil {
		check.Output = fmt.Sprintf("%v\n%s", err, output)
		return nil, check
	}
	findings, parseErr := parseStaticcheckProfileOutput(stdout.String(), profile)
	if parseErr != nil {
		check.Passed = false
		check.Output = fmt.Sprintf("parse staticcheck output: %v\n%s", parseErr, output)
		return nil, check
	}
	return findings, check
}

func parseStaticcheckOutput(output string) ([]ASTFinding, error) {
	return parseStaticcheckProfileOutput(output, u1000StaticcheckProfile)
}

func parseCuratedStaticcheckOutput(output string) ([]ASTFinding, error) {
	return parseStaticcheckProfileOutput(output, curatedStaticcheckProfile)
}

func parseStaticcheckProfileOutput(output string, profile staticcheckProfile) ([]ASTFinding, error) {
	rules := make(map[string]staticcheckRule, len(profile.Rules))
	for _, rule := range profile.Rules {
		rules[rule.Code] = rule
	}
	var findings []ASTFinding
	dec := json.NewDecoder(strings.NewReader(output))
	for dec.More() {
		var diag staticcheckDiagnostic
		if err := dec.Decode(&diag); err != nil {
			return nil, err
		}
		rule, ok := rules[diag.Code]
		if !ok {
			continue
		}
		findings = append(findings, ASTFinding{
			Rule:     rule.Rule,
			Severity: rule.Severity,
			File:     diag.Location.File,
			Line:     diag.Location.Line,
			Symbol:   diag.Code,
			Message:  diag.Message,
		})
	}
	return findings, nil
}

func staticcheckCodes(rules []staticcheckRule) string {
	codes := make([]string, 0, len(rules))
	for _, rule := range rules {
		codes = append(codes, rule.Code)
	}
	return strings.Join(codes, ",")
}

func curatedStaticcheckRule(id string) (staticcheckRule, bool) {
	for _, rule := range curatedStaticcheckProfile.Rules {
		if rule.Rule == id {
			return rule, true
		}
	}
	return staticcheckRule{}, false
}
