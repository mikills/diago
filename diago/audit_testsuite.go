package diago

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// testRunKind names one go test run of the target that the audit starts.
type testRunKind string

const (
	testRunPlain    testRunKind = "test"
	testRunRace     testRunKind = "race"
	testRunCoverage testRunKind = "coverage"
)

// testCheckSource tells where the test check gets its result.
type testCheckSource string

const (
	testSkipped         testCheckSource = "skipped"
	testFromPlainRun    testCheckSource = "plain run"
	testFromRaceRun     testCheckSource = "race run"
	testFromCoverageRun testCheckSource = "coverage run"
	testNeedsPlainRun   testCheckSource = "plain rerun"
)

// planTestRuns returns the go test runs that start before the test check is known.
// A passed race run or coverage run also supplies the test check. Race and
// coverage stay in separate runs, because the atomic coverage counters that
// -race forces are synchronisation that can hide a data race.
func planTestRuns(cfg AuditConfig) []testRunKind {
	var runs []testRunKind
	if cfg.Race {
		runs = append(runs, testRunRace)
	}
	if cfg.Coverage {
		runs = append(runs, testRunCoverage)
	}
	if len(runs) == 0 && !cfg.SkipTests {
		runs = append(runs, testRunPlain)
	}
	return runs
}

// chooseTestCheckSource selects the source of the test check after the planned runs.
// A failed shared run does not tell which check failed, so the plain run starts again.
func chooseTestCheckSource(cfg AuditConfig, racePassed, coverageTestsPassed bool) testCheckSource {
	switch {
	case cfg.SkipTests:
		return testSkipped
	case !cfg.Race && !cfg.Coverage:
		return testFromPlainRun
	case cfg.Race && racePassed:
		return testFromRaceRun
	case cfg.Coverage && coverageTestsPassed:
		return testFromCoverageRun
	default:
		return testNeedsPlainRun
	}
}

// shouldRerunTestsAfterDeadCodeFix reports whether a fix in this audit can have changed a file.
// The modernize fix does not report its edits, so it always counts as a change.
func shouldRerunTestsAfterDeadCodeFix(cfg AuditConfig, removedFuncs int) bool {
	return removedFuncs > 0 || cfg.ModernizeFix
}

type testSuiteResult struct {
	test     AuditCheck
	race     AuditCheck
	coverage coverageResult
}

func runTestSuite(cfg AuditConfig, workDir, targetPath string) testSuiteResult {
	var result testSuiteResult
	for _, run := range planTestRuns(cfg) {
		switch run {
		case testRunPlain:
			result.test = runPlainTestCheck(cfg.Progress, workDir, targetPath)
		case testRunRace:
			result.race = timedCheck(cfg.Progress, "race", func() AuditCheck {
				return runAuditCommand(workDir, "race", "go", "test", "-race", targetPath)
			})
		case testRunCoverage:
			result.coverage = runTimedCoverage(cfg.Progress, workDir, targetPath)
		}
	}

	switch chooseTestCheckSource(cfg, cfg.Race && result.race.Passed, result.coverage.testsPassed()) {
	case testFromRaceRun:
		result.test = sharedTestCheck(cfg.Progress, result.race, testFromRaceRun)
	case testFromCoverageRun:
		result.test = testCheckFromCoverage(cfg.Progress, workDir, result.coverage)
	case testNeedsPlainRun:
		result.test = runPlainTestCheck(cfg.Progress, workDir, targetPath)
	}
	return result
}

func runPlainTestCheck(progress io.Writer, workDir, targetPath string) AuditCheck {
	return timedCheck(progress, "test", func() AuditCheck {
		return runAuditCommand(workDir, "test", "go", "test", targetPath)
	})
}

func runTimedCoverage(progress io.Writer, workDir, targetPath string) coverageResult {
	var result coverageResult
	check := timedCheck(progress, "coverage", func() AuditCheck {
		result = runCoverage(workDir, targetPath)
		return result.Check
	})
	result.Check = check
	return result
}

func sharedTestCheck(progress io.Writer, run AuditCheck, source testCheckSource) AuditCheck {
	fmt.Fprintf(progress, "diago: test %s (result from the %s)\n", passFailWord(run.Passed), source)
	return AuditCheck{Name: "test", Command: run.Command, Passed: run.Passed, Output: run.Output}
}

// testCheckFromCoverage completes the test check from a passed coverage run.
// The coverage run tests only packages with test files, so the other packages are built here.
func testCheckFromCoverage(progress io.Writer, workDir string, coverage coverageResult) AuditCheck {
	check := AuditCheck{Name: "test", Passed: true}
	if coverage.Tests != nil {
		check.Command = coverage.Tests.Command
		check.Output = coverage.Tests.Output
	}
	if len(coverage.Untested) == 0 {
		return sharedTestCheck(progress, check, testFromCoverageRun)
	}
	build := timedCheck(progress, "test", func() AuditCheck {
		args := append([]string{"go", "test"}, coverage.Untested...)
		return runAuditCommand(workDir, "test", args...)
	})
	commands := []string{build.Command}
	if check.Command != "" {
		commands = append([]string{check.Command}, commands...)
	}
	check.Command = strings.Join(commands, " && ")
	check.Output += build.Output
	check.Passed = build.Passed
	check.DurationMS = build.DurationMS
	return check
}

func skippedTestAfterDeadCodeFix(report *AuditReport, targetPath string) AuditCheck {
	check := AuditCheck{
		Name:    "test-after-deadcode-fix",
		Command: "go test " + targetPath,
		Passed:  true,
		Output:  "skipped: the dead code fix changed no files, so the test check result applies\n",
	}
	for _, earlier := range report.Checks {
		if earlier.Name == "test" {
			check.Passed = earlier.Passed
		}
	}
	return check
}

// timedCheck runs one audit step, writes its start and end to progress, and records its duration.
func timedCheck(progress io.Writer, name string, run func() AuditCheck) AuditCheck {
	fmt.Fprintf(progress, "diago: %s started\n", name)
	started := time.Now()
	check := run()
	elapsed := time.Since(started)
	check.DurationMS = elapsed.Milliseconds()
	fmt.Fprintf(progress, "diago: %s %s in %s\n", name, passFailWord(check.Passed), elapsed.Round(100*time.Millisecond))
	return check
}

func timedFindings(progress io.Writer, name string, run func() ([]ASTFinding, AuditCheck)) ([]ASTFinding, AuditCheck) {
	var findings []ASTFinding
	check := timedCheck(progress, name, func() AuditCheck {
		var check AuditCheck
		findings, check = run()
		return check
	})
	return findings, check
}

func passFailWord(passed bool) string {
	if passed {
		return "passed"
	}
	return "failed"
}
