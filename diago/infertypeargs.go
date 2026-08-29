package diago

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const inferTypeArgsMessage = "unnecessary type arguments"

// runInferTypeArgsAudit reports redundant explicit type arguments on generic
// calls. The analyzer lives only inside gopls, so it runs via `gopls check`
// and is report-only (no -fix).
func runInferTypeArgsAudit(workDir, targetPath string) ([]ASTFinding, AuditCheck) {
	check := AuditCheck{Name: "infertypeargs", Command: "go run golang.org/x/tools/gopls@" + goplsModernizeVersion + " check -severity hint <files>", ToolVersion: "gopls " + goplsModernizeVersion}
	files, err := goFilesForTarget(workDir, targetPath)
	if err != nil {
		check.Output = err.Error()
		return nil, check
	}
	if len(files) == 0 {
		check.Passed = true
		check.Output = "no Go files to check"
		return nil, check
	}

	args := append([]string{"run", "golang.org/x/tools/gopls@" + goplsModernizeVersion, "check", "-severity", "hint"}, files...)
	cmd := exec.Command("go", args...)
	cmd.Dir = workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	output := stdout.String() + stderr.String()
	check.Passed = runErr == nil
	check.Output = output
	if runErr != nil {
		check.Output = fmt.Sprintf("%v\n%s", runErr, output)
		return nil, check
	}
	return parseInferTypeArgsOutput(stdout.String()), check
}

// goFilesForTarget lists every Go file (tests included) under target, because
// gopls check takes file paths, not package patterns.
func goFilesForTarget(workDir, targetPath string) ([]string, error) {
	pkgs, err := listPackages(workDir, targetPath)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, pkg := range pkgs {
		for _, group := range [][]string{pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
			for _, name := range group {
				files = append(files, filepath.Join(pkg.Dir, name))
			}
		}
	}
	return files, nil
}

// parseInferTypeArgsOutput selects the infertypeargs lines from gopls check
// output, which prints "file:line:colrange: message" for every analyzer.
func parseInferTypeArgsOutput(output string) []ASTFinding {
	var findings []ASTFinding
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		sep := strings.LastIndex(line, ": ")
		if sep < 0 || line[sep+2:] != inferTypeArgsMessage {
			continue
		}
		file, lineNo := parseModernizePosition(line[:sep])
		findings = append(findings, ASTFinding{
			Rule:     "infertypeargs",
			Severity: "medium",
			File:     file,
			Line:     lineNo,
			Symbol:   "infertypeargs",
			Message:  inferTypeArgsMessage,
		})
	}
	return findings
}
