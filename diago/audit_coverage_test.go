package diago

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCoverageSkipsPackagesWithoutTests(t *testing.T) {
	dir := coverageModule(t, false)
	targets, check := coverageTargets(dir, "./...")
	if !check.Passed {
		t.Fatalf("coverageTargets failed: %s", check.Output)
	}
	if len(targets) != 1 || targets[0] != "example.com/coverage/tested" {
		t.Fatalf("coverage targets = %v, want only tested package", targets)
	}
	report, coverageCheck := runCoverage(dir, "./...")
	if !coverageCheck.Passed {
		t.Fatalf("coverage failed: %s", coverageCheck.Output)
	}
	if report == nil || !strings.Contains(coverageCheck.Command, "example.com/coverage/tested") || strings.Contains(coverageCheck.Command, "notests") {
		t.Fatalf("unexpected coverage result: report=%+v check=%+v", report, coverageCheck)
	}
}

func TestRunCoverageWithNoTestsPasses(t *testing.T) {
	dir := t.TempDir()
	writeCoverageFile(t, dir, "go.mod", "module example.com/notests\n\ngo 1.22\n")
	writeCoverageFile(t, dir, "plain.go", "package notests\n\nfunc Value() int { return 1 }\n")
	report, check := runCoverage(dir, "./...")
	if !check.Passed || report == nil || !strings.Contains(check.Output, "coverage skipped") {
		t.Fatalf("no-test coverage should pass: report=%+v check=%+v", report, check)
	}
}

func TestRunCoveragePreservesTestFailures(t *testing.T) {
	dir := coverageModule(t, true)
	_, check := runCoverage(dir, "./...")
	if check.Passed {
		t.Fatalf("failing test must fail coverage check: %+v", check)
	}
	if !strings.Contains(check.Output, "wrong value") {
		t.Fatalf("coverage failure output lost test diagnostics: %q", check.Output)
	}
}

func TestRunCoveragePreservesPackagePatternAndEnvironment(t *testing.T) {
	dir := coverageModule(t, false)
	writeCoverageFile(t, dir, "other/other.go", "package other\n\nfunc Value() int { return 1 }\n")
	writeCoverageFile(t, dir, "other/other_test.go", "package other\n\nimport \"testing\"\n\nfunc TestFailure(t *testing.T) { t.Fatal(\"outside selected pattern\") }\n")
	writeCoverageFile(t, dir, "tested/env_test.go", "package tested\n\nimport (\"os\"; \"testing\")\n\nfunc TestEnvironment(t *testing.T) { if os.Getenv(\"DIAGO_COVERAGE_TEST\") != \"present\" { t.Fatal(\"environment missing\") } }\n")
	t.Setenv("DIAGO_COVERAGE_TEST", "present")

	_, check := runCoverage(dir, "./tested")
	if !check.Passed {
		t.Fatalf("selected package coverage failed: %+v", check)
	}
	if strings.Contains(check.Command, "example.com/coverage/other") || strings.Contains(check.Output, "outside selected pattern") {
		t.Fatalf("coverage escaped selected package pattern: %+v", check)
	}
}

func coverageModule(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	writeCoverageFile(t, dir, "go.mod", "module example.com/coverage\n\ngo 1.22\n")
	writeCoverageFile(t, dir, "notests/plain.go", "package notests\n\nfunc Value() int { return 1 }\n")
	writeCoverageFile(t, dir, "tested/tested.go", "package tested\n\nfunc Value() int { return 1 }\n")
	want := "1"
	if fail {
		want = "2"
	}
	testSource := "package tested\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != " + want + " { t.Fatal(\"wrong value\") } }\n"
	writeCoverageFile(t, dir, "tested/tested_test.go", testSource)
	return dir
}

func writeCoverageFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
