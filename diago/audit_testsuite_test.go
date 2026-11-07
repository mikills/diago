package diago

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanTestRuns(t *testing.T) {
	cases := []struct {
		name string
		cfg  AuditConfig
		want []testRunKind
	}{
		{name: "default", cfg: AuditConfig{AST: true}, want: []testRunKind{testRunPlain}},
		{name: "modernize and u1000", cfg: AuditConfig{AST: true, Modernize: true, U1000: true}, want: []testRunKind{testRunPlain}},
		{name: "race", cfg: AuditConfig{Race: true}, want: []testRunKind{testRunRace}},
		{name: "coverage", cfg: AuditConfig{Coverage: true}, want: []testRunKind{testRunCoverage}},
		{name: "race and coverage", cfg: AuditConfig{Race: true, Coverage: true}, want: []testRunKind{testRunRace, testRunCoverage}},
		{name: "tests skipped", cfg: AuditConfig{AST: true, SkipTests: true}, want: nil},
		{name: "tests skipped with race", cfg: AuditConfig{Race: true, SkipTests: true}, want: []testRunKind{testRunRace}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, planTestRuns(tc.cfg))
		})
	}
}

func TestChooseTestCheckSource(t *testing.T) {
	cases := []struct {
		name                string
		cfg                 AuditConfig
		racePassed          bool
		coverageTestsPassed bool
		want                testCheckSource
	}{
		{name: "plain run only", cfg: AuditConfig{}, want: testFromPlainRun},
		{name: "skipped", cfg: AuditConfig{Race: true, SkipTests: true}, racePassed: true, want: testSkipped},
		{name: "race passed", cfg: AuditConfig{Race: true}, racePassed: true, want: testFromRaceRun},
		{name: "race failed", cfg: AuditConfig{Race: true}, want: testNeedsPlainRun},
		{name: "coverage passed", cfg: AuditConfig{Coverage: true}, coverageTestsPassed: true, want: testFromCoverageRun},
		{name: "coverage failed", cfg: AuditConfig{Coverage: true}, want: testNeedsPlainRun},
		{name: "race failed and coverage passed", cfg: AuditConfig{Race: true, Coverage: true}, coverageTestsPassed: true, want: testFromCoverageRun},
		{name: "race passed and coverage failed", cfg: AuditConfig{Race: true, Coverage: true}, racePassed: true, want: testFromRaceRun},
		{name: "both failed", cfg: AuditConfig{Race: true, Coverage: true}, want: testNeedsPlainRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chooseTestCheckSource(tc.cfg, tc.racePassed, tc.coverageTestsPassed))
		})
	}
}

func TestCoverageTestsPassed(t *testing.T) {
	passed := AuditCheck{Passed: true}
	failed := AuditCheck{}
	assert.False(t, coverageResult{}.testsPassed(), "a failed package listing")
	assert.False(t, coverageResult{Listed: true}.testsPassed(), "no packages")
	assert.True(t, coverageResult{Listed: true, Untested: []string{"a"}}.testsPassed(), "no packages with tests")
	assert.True(t, coverageResult{Listed: true, Tests: &passed}.testsPassed())
	assert.False(t, coverageResult{Listed: true, Tests: &failed}.testsPassed())
}

func TestDeadCodeFixRerun(t *testing.T) {
	cfg := AuditConfig{DeadCode: true, DeadCodeFix: true}
	assert.False(t, shouldRerunTestsAfterDeadCodeFix(cfg, 0))
	assert.True(t, shouldRerunTestsAfterDeadCodeFix(cfg, 2))

	cfg.ModernizeFix = true
	assert.True(t, shouldRerunTestsAfterDeadCodeFix(cfg, 0))
}

func TestSharedRunKeepsTestCheckName(t *testing.T) {
	race := AuditCheck{Name: "race", Command: "go test -race ./...", Passed: false, Output: "WARNING: DATA RACE", DurationMS: 50}
	check := sharedTestCheck(io.Discard, race, testFromRaceRun)
	assert.Equal(t, AuditCheck{Name: "test", Command: race.Command, Passed: false, Output: race.Output}, check)

	tests := AuditCheck{Name: "coverage", Command: "go test -coverprofile p ./a", Passed: true, Output: "ok a"}
	check = testCheckFromCoverage(io.Discard, "", coverageResult{Listed: true, Tests: &tests})
	assert.Equal(t, "test", check.Name)
	assert.True(t, check.Passed)
	assert.Equal(t, tests.Command, check.Command)
}

func TestSkippedRerunKeepsTestResult(t *testing.T) {
	report := &AuditReport{Checks: []AuditCheck{{Name: "test", Passed: false}}}
	check := skippedTestAfterDeadCodeFix(report, "./...")
	assert.Equal(t, "test-after-deadcode-fix", check.Name)
	assert.False(t, check.Passed)

	check = skippedTestAfterDeadCodeFix(&AuditReport{}, "./...")
	assert.True(t, check.Passed)
}

func TestTimedCheckWritesProgress(t *testing.T) {
	var progress bytes.Buffer
	check := timedCheck(&progress, "vet", func() AuditCheck { return AuditCheck{Name: "vet", Passed: false} })

	lines := strings.Split(strings.TrimSpace(progress.String()), "\n")
	require.Len(t, lines, 2)
	assert.Equal(t, "diago: vet started", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "diago: vet failed in "), lines[1])
	assert.False(t, check.Passed)
}

func TestProgressStaysOutOfReport(t *testing.T) {
	dir := t.TempDir()
	writeCoverageFile(t, dir, "go.mod", "module example.com/progress\n\ngo 1.22\n")
	writeCoverageFile(t, dir, "plain.go", "package progress\n\nfunc Value() int { return 1 }\n")
	output := filepath.Join(dir, "audit.json")
	var progress bytes.Buffer

	report, err := RunAudit(AuditConfig{TargetPath: dir, OutputFile: output, Format: FormatJSON, SkipTests: true, Progress: &progress})
	require.NoError(t, err)

	names := make([]string, 0, len(report.Checks))
	for _, check := range report.Checks {
		names = append(names, check.Name)
	}
	assert.Equal(t, []string{"vet"}, names)
	assert.Contains(t, progress.String(), "diago: vet started")

	data, err := os.ReadFile(output)
	require.NoError(t, err)
	var written AuditReport
	require.NoError(t, json.Unmarshal(data, &written))
	assert.NotContains(t, string(data), "diago: vet")
	require.Len(t, written.Checks, 1)
	assert.Equal(t, "vet", written.Checks[0].Name)
}
