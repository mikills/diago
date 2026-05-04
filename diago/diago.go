package diago

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Finding represents a single hotspot from a pprof profile.
type Finding struct {
	Function string  `json:"function"`
	File     string  `json:"file,omitempty"`
	Line     int     `json:"line,omitempty"`
	FlatPct  float64 `json:"flat_pct"`
	CumPct   float64 `json:"cum_pct"`
}

// EscapeFinding represents a heap escape reported by the compiler.
type EscapeFinding struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Detail string `json:"detail"`
}

// SummaryItem is a top-level hotspot across all profile types.
type SummaryItem struct {
	ProfileType string  `json:"profile_type"`
	Function    string  `json:"function"`
	File        string  `json:"file,omitempty"`
	Line        int     `json:"line,omitempty"`
	CumPct      float64 `json:"cum_pct"`
}

// Report holds all profiling results.
type Report struct {
	Package         string          `json:"package"`
	BenchFilter     string          `json:"bench_filter"`
	Threshold       float64         `json:"threshold"`
	Summary         []SummaryItem   `json:"summary"`
	BenchmarkOutput string          `json:"benchmark_output"`
	CPUFindings     []Finding       `json:"cpu_findings"`
	MemFindings     []Finding       `json:"mem_findings"`
	MutexFindings   []Finding       `json:"mutex_findings"`
	BlockFindings   []Finding       `json:"block_findings"`
	EscapeFindings  []EscapeFinding `json:"escape_findings"`
	RawCPU          string          `json:"raw_cpu"`
	RawMem          string          `json:"raw_mem"`
	RawMutex        string          `json:"raw_mutex"`
	RawBlock        string          `json:"raw_block"`
	RawEscape       string          `json:"raw_escape"`
}

// ChangedFinding tracks how a hotspot changed between two runs.
type ChangedFinding struct {
	Function  string  `json:"function"`
	File      string  `json:"file,omitempty"`
	Line      int     `json:"line,omitempty"`
	OldCumPct float64 `json:"old_cum_pct"`
	NewCumPct float64 `json:"new_cum_pct"`
	DeltaPct  float64 `json:"delta_pct"`
}

// CompareReport shows what changed between two profiling runs.
type CompareReport struct {
	CPUImproved    []ChangedFinding `json:"cpu_improved"`
	CPURegressed   []ChangedFinding `json:"cpu_regressed"`
	MemImproved    []ChangedFinding `json:"mem_improved"`
	MemRegressed   []ChangedFinding `json:"mem_regressed"`
	MutexImproved  []ChangedFinding `json:"mutex_improved"`
	MutexRegressed []ChangedFinding `json:"mutex_regressed"`
	BlockImproved  []ChangedFinding `json:"block_improved"`
	BlockRegressed []ChangedFinding `json:"block_regressed"`
	EscapesAdded   []EscapeFinding  `json:"escapes_added"`
	EscapesRemoved []EscapeFinding  `json:"escapes_removed"`
}

// OutputFormat controls the findings file format.
type OutputFormat string

const (
	FormatText OutputFormat = "text"
	FormatJSON OutputFormat = "json"
)

// Config controls what gets profiled.
type Config struct {
	// target package path (e.g. "./..." or "./petname")
	TargetPath string

	// output file for findings
	OutputFile string

	// benchmark filter regex (default ".")
	BenchFilter string

	// minimum cumulative percentage to report
	Threshold float64

	// output format
	Format OutputFormat
}

func (c *Config) defaults() {
	if c.OutputFile == "" {
		c.OutputFile = ".diago/perf.txt"
	}
	if c.BenchFilter == "" {
		c.BenchFilter = "."
	}
	if c.Threshold <= 0 {
		c.Threshold = 1.0
	}
	if c.Format == "" {
		c.Format = FormatText
	}
}

// Run executes benchmarks with profiling and writes findings to disk.
func Run(cfg Config) (*Report, error) {
	cfg.defaults()

	tmpDir, err := os.MkdirTemp("", "diago-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	workDir, targetPath, err := prepareProfileTarget(cfg)
	if err != nil {
		return nil, err
	}

	report := &Report{Package: cfg.TargetPath, BenchFilter: cfg.BenchFilter, Threshold: cfg.Threshold}
	if err := collectProfiles(report, cfg, workDir, targetPath, tmpDir); err != nil {
		return nil, err
	}
	collectEscapeAnalysis(report, workDir, targetPath)
	report.Summary = buildSummary(report)

	if err := writeOutput(cfg.OutputFile, report, cfg.Format); err != nil {
		return nil, fmt.Errorf("writing findings: %w", err)
	}
	return report, nil
}

func prepareProfileTarget(cfg Config) (workDir, targetPath string, err error) {
	workDir, targetPath, err = resolveTarget(cfg.TargetPath)
	if err != nil {
		return "", "", err
	}
	hasBench, err := hasBenchmarks(workDir, targetPath, cfg.BenchFilter)
	if err != nil {
		return "", "", fmt.Errorf("listing benchmarks: %w", err)
	}
	if !hasBench {
		return "", "", fmt.Errorf("no benchmarks found in %s matching %q", cfg.TargetPath, cfg.BenchFilter)
	}
	return workDir, targetPath, nil
}

func collectProfiles(report *Report, cfg Config, workDir, targetPath, tmpDir string) error {
	files, err := runProfileBenchmarks(report, cfg, workDir, targetPath, tmpDir)
	if err != nil {
		return err
	}
	return parseProfiles(report, cfg.Threshold, files)
}

type profileFiles struct{ cpu, mem, mutex, block string }

func runProfileBenchmarks(report *Report, cfg Config, workDir, targetPath, tmpDir string) (profileFiles, error) {
	files := profileFiles{
		cpu:   filepath.Join(tmpDir, "cpu.prof"),
		mem:   filepath.Join(tmpDir, "mem.prof"),
		mutex: filepath.Join(tmpDir, "mutex.prof"),
		block: filepath.Join(tmpDir, "block.prof"),
	}
	benchOut, err := runBenchmarks(workDir, targetPath, cfg.BenchFilter, []string{"-cpuprofile", files.cpu, "-memprofile", files.mem})
	if err != nil {
		return files, fmt.Errorf("benchmarks (cpu/mem): %w\noutput: %s", err, benchOut)
	}
	report.BenchmarkOutput = benchOut
	_, err = runBenchmarks(workDir, targetPath, cfg.BenchFilter, []string{"-mutexprofile", files.mutex, "-mutexprofilefraction", "1", "-blockprofile", files.block, "-blockprofilerate", "1"})
	if err != nil {
		return files, fmt.Errorf("benchmarks (mutex/block): %w", err)
	}
	return files, nil
}

func parseProfiles(report *Report, threshold float64, files profileFiles) error {
	profiles := []struct {
		file     string
		findings *[]Finding
		raw      *string
	}{
		{files.cpu, &report.CPUFindings, &report.RawCPU},
		{files.mem, &report.MemFindings, &report.RawMem},
		{files.mutex, &report.MutexFindings, &report.RawMutex},
		{files.block, &report.BlockFindings, &report.RawBlock},
	}
	for _, p := range profiles {
		raw, findings, err := parseProfile(p.file, threshold)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", p.file, err)
		}
		*p.findings = findings
		*p.raw = raw
	}
	return nil
}

func collectEscapeAnalysis(report *Report, workDir, targetPath string) {
	escRaw, escFindings, err := runEscapeAnalysis(workDir, targetPath)
	if err != nil {
		report.RawEscape = fmt.Sprintf("escape analysis failed: %v", err)
		return
	}
	report.RawEscape = escRaw
	report.EscapeFindings = escFindings
}

// Compare loads two JSON report files and produces a diff.
func Compare(beforePath, afterPath string) (*CompareReport, error) {
	before, err := LoadReport(beforePath)
	if err != nil {
		return nil, fmt.Errorf("loading before report: %w", err)
	}
	after, err := LoadReport(afterPath)
	if err != nil {
		return nil, fmt.Errorf("loading after report: %w", err)
	}

	return CompareReports(before, after), nil
}

// LoadReport reads a JSON report from disk.
func LoadReport(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// CompareReports diffs two reports.
func CompareReports(before, after *Report) *CompareReport {
	cr := &CompareReport{}

	cr.CPUImproved, cr.CPURegressed = diffFindings(before.CPUFindings, after.CPUFindings)
	cr.MemImproved, cr.MemRegressed = diffFindings(before.MemFindings, after.MemFindings)
	cr.MutexImproved, cr.MutexRegressed = diffFindings(before.MutexFindings, after.MutexFindings)
	cr.BlockImproved, cr.BlockRegressed = diffFindings(before.BlockFindings, after.BlockFindings)
	cr.EscapesAdded, cr.EscapesRemoved = diffEscapes(before.EscapeFindings, after.EscapeFindings)

	return cr
}

// WriteCompareReport writes comparison results to disk.
func WriteCompareReport(path string, cr *CompareReport, format OutputFormat) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	switch format {
	case FormatJSON:
		data, err := json.MarshalIndent(cr, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(path, data, 0644)
	default:
		return writeCompareText(path, cr)
	}
}

func diffFindings(before, after []Finding) (improved, regressed []ChangedFinding) {
	improved = make([]ChangedFinding, 0, len(before))
	regressed = make([]ChangedFinding, 0, len(after))
	beforeIndex := make(map[string]int, len(before))
	for i := range before {
		beforeIndex[before[i].Function] = i
	}

	seenAfter := make(map[string]struct{}, len(after))

	// check everything in after against before
	for _, a := range after {
		seenAfter[a.Function] = struct{}{}
		idx, existed := beforeIndex[a.Function]
		if !existed {
			// new hotspot
			regressed = append(regressed, ChangedFinding{
				Function:  a.Function,
				File:      a.File,
				Line:      a.Line,
				OldCumPct: 0,
				NewCumPct: a.CumPct,
				DeltaPct:  a.CumPct,
			})
			continue
		}

		b := before[idx]
		delta := a.CumPct - b.CumPct
		// only report meaningful changes (>0.5% shift)
		if math.Abs(delta) < 0.5 {
			continue
		}
		cf := ChangedFinding{
			Function:  a.Function,
			File:      a.File,
			Line:      a.Line,
			OldCumPct: b.CumPct,
			NewCumPct: a.CumPct,
			DeltaPct:  delta,
		}
		if delta < 0 {
			improved = append(improved, cf)
		} else {
			regressed = append(regressed, cf)
		}
	}

	// check for hotspots that disappeared
	for _, b := range before {
		if _, exists := seenAfter[b.Function]; !exists {
			improved = append(improved, ChangedFinding{
				Function:  b.Function,
				File:      b.File,
				Line:      b.Line,
				OldCumPct: b.CumPct,
				NewCumPct: 0,
				DeltaPct:  -b.CumPct,
			})
		}
	}

	// sort by magnitude of change
	sort.Slice(improved, func(i, j int) bool {
		return improved[i].DeltaPct < improved[j].DeltaPct
	})
	sort.Slice(regressed, func(i, j int) bool {
		return regressed[i].DeltaPct > regressed[j].DeltaPct
	})

	return improved, regressed
}

func diffEscapes(before, after []EscapeFinding) (added, removed []EscapeFinding) {
	added = make([]EscapeFinding, 0, len(after))
	removed = make([]EscapeFinding, 0, len(before))
	type escKey struct {
		File string
		Line int
	}

	beforeSet := make(map[escKey]struct{}, len(before))
	for _, e := range before {
		beforeSet[escKey{e.File, e.Line}] = struct{}{}
	}

	afterSet := make(map[escKey]struct{}, len(after))
	for _, e := range after {
		key := escKey{e.File, e.Line}
		afterSet[key] = struct{}{}
		if _, exists := beforeSet[key]; !exists {
			added = append(added, e)
		}
	}

	for _, e := range before {
		if _, exists := afterSet[escKey{e.File, e.Line}]; !exists {
			removed = append(removed, e)
		}
	}

	return added, removed
}

func buildSummary(report *Report) []SummaryItem {
	type candidate struct {
		profileType string
		f           Finding
	}

	var all []candidate
	for _, f := range userFindings(report.CPUFindings) {
		all = append(all, candidate{"cpu", f})
	}
	for _, f := range userFindings(report.MemFindings) {
		all = append(all, candidate{"memory", f})
	}
	for _, f := range userFindings(report.MutexFindings) {
		all = append(all, candidate{"mutex", f})
	}
	for _, f := range userFindings(report.BlockFindings) {
		all = append(all, candidate{"block", f})
	}

	// sort by cumulative percentage descending
	sort.Slice(all, func(i, j int) bool {
		return all[i].f.CumPct > all[j].f.CumPct
	})

	// deduplicate by function, keep highest
	seen := make(map[string]bool)
	var summary []SummaryItem
	for _, c := range all {
		if seen[c.f.Function] {
			continue
		}
		seen[c.f.Function] = true
		summary = append(summary, SummaryItem{
			ProfileType: c.profileType,
			Function:    c.f.Function,
			File:        c.f.File,
			Line:        c.f.Line,
			CumPct:      c.f.CumPct,
		})
		if len(summary) >= 5 {
			break
		}
	}

	return summary
}

// userFindings filters out runtime, testing, benchmark harness, and stdlib noise.
func userFindings(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if isNoiseFinding(f) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func isNoiseFinding(f Finding) bool {
	if strings.Contains(f.Function, ".Benchmark") {
		return true
	}
	if f.File != "" && (strings.Contains(f.File, "/go/src/") || strings.HasSuffix(f.File, "_testmain.go")) {
		return true
	}

	for _, prefix := range []string{
		"runtime.",
		"testing.",
		"sync.",
		"internal/",
		"strings.",
		"strconv.",
		"math/",
		"main.",
		"regexp.",
		"bufio.",
		"bytes.",
		"sort.",
		"os.",
		"path/filepath.",
		"encoding/json.",
	} {
		if strings.HasPrefix(f.Function, prefix) {
			return true
		}
	}
	return false
}

func resolveTarget(target string) (workDir, packagePath string, err error) {
	if target == "" || !filepath.IsAbs(target) {
		return "", target, nil
	}

	target, recursive := trimRecursiveTarget(target)
	targetDir, err := targetDirectory(target)
	if err != nil {
		return "", "", err
	}
	moduleRoot, err := findModuleRoot(targetDir)
	if err != nil {
		return "", "", err
	}
	packagePath, err = moduleRelativePackage(moduleRoot, targetDir, recursive)
	return moduleRoot, packagePath, err
}

func trimRecursiveTarget(target string) (string, bool) {
	suffix := string(filepath.Separator) + "..."
	if strings.HasSuffix(target, suffix) {
		return strings.TrimSuffix(target, suffix), true
	}
	return target, false
}

func targetDirectory(target string) (string, error) {
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stat target: %w", err)
	}
	if !info.IsDir() {
		return filepath.Dir(target), nil
	}
	return target, nil
}

func moduleRelativePackage(moduleRoot, target string, recursive bool) (string, error) {
	rel, err := filepath.Rel(moduleRoot, target)
	if err != nil {
		return "", fmt.Errorf("rel target: %w", err)
	}
	if rel == "." {
		if recursive {
			return "./...", nil
		}
		return ".", nil
	}
	pkg := "./" + filepath.ToSlash(rel)
	if recursive {
		pkg += "/..."
	}
	return pkg, nil
}

func findModuleRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func hasBenchmarks(workDir, target, filter string) (bool, error) {
	cmd := exec.Command("go", "test", "-list", filter, "-run", "^$", target)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return false, nil
	}

	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Benchmark") {
			return true, nil
		}
	}
	return false, nil
}

func runBenchmarks(workDir, target, filter string, extraArgs []string) (string, error) {
	args := []string{
		"test",
		"-run", "^$",
		"-bench", filter,
		"-benchmem",
		"-count", "1",
		"-timeout", "60s",
	}
	args = append(args, extraArgs...)
	args = append(args, target)

	cmd := exec.Command("go", args...)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	return out.String(), err
}

func parseProfile(profilePath string, threshold float64) (string, []Finding, error) {
	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		return "", nil, nil
	}

	cmd := exec.Command("go", "tool", "pprof", "-text", "-lines", "-cum", "-nodecount", "40", profilePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return out.String(), nil, fmt.Errorf("pprof: %w\noutput: %s", err, out.String())
	}

	raw := out.String()
	findings := ParsePProfText(raw, threshold)
	return raw, findings, nil
}

// ParsePProfText extracts findings from pprof -text -lines output.
// Exported for testing.
func ParsePProfText(text string, threshold float64) []Finding {
	var findings []Finding

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()

		flatPct, cumPct, nameField, ok := parsePProfLine(line)
		if !ok || cumPct < threshold {
			continue
		}

		f := Finding{
			FlatPct:  flatPct,
			CumPct:   cumPct,
			Function: nameField,
		}

		// try to extract file:line from the name field
		if fn, file, lineNum, ok := splitLocation(nameField); ok {
			f.Function = fn
			f.File = file
			f.Line = lineNum
		}

		findings = append(findings, f)
	}

	return findings
}

func parsePProfLine(line string) (flatPct, cumPct float64, name string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 || !strings.HasSuffix(fields[1], "%") || !strings.HasSuffix(fields[4], "%") {
		return 0, 0, "", false
	}

	flatPct, err := strconv.ParseFloat(strings.TrimSuffix(fields[1], "%"), 64)
	if err != nil {
		return 0, 0, "", false
	}
	cumPct, err = strconv.ParseFloat(strings.TrimSuffix(fields[4], "%"), 64)
	if err != nil {
		return 0, 0, "", false
	}

	name = strings.Join(fields[5:], " ")
	return flatPct, cumPct, name, true
}

func splitLocation(nameField string) (function, file string, line int, ok bool) {
	colon := strings.LastIndexByte(nameField, ':')
	if colon < 0 || colon == len(nameField)-1 {
		return "", "", 0, false
	}

	lineNum, err := strconv.Atoi(nameField[colon+1:])
	if err != nil {
		return "", "", 0, false
	}

	prefix := nameField[:colon]
	space := strings.LastIndexByte(prefix, ' ')
	if space < 0 || space == len(prefix)-1 {
		return "", "", 0, false
	}

	function = strings.TrimSpace(prefix[:space])
	file = prefix[space+1:]
	if function == "" || file == "" {
		return "", "", 0, false
	}
	return function, file, lineNum, true
}

func runEscapeAnalysis(workDir, target string) (string, []EscapeFinding, error) {
	cmd := exec.Command("go", "build", "-gcflags=-m", target)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	raw := out.String()
	findings := ParseEscapeOutput(raw)
	return raw, findings, err
}

// ParseEscapeOutput extracts heap escapes from go build -gcflags=-m output.
// Exported for testing.
func ParseEscapeOutput(text string) []EscapeFinding {
	var findings []EscapeFinding

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.Contains(line, "escapes to heap") &&
			!strings.Contains(line, "moved to heap") {
			continue
		}

		file, lineNum, detail, ok := parseEscapeLine(line)
		if !ok {
			continue
		}

		findings = append(findings, EscapeFinding{
			File:   file,
			Line:   lineNum,
			Detail: detail,
		})
	}

	return findings
}

func parseEscapeLine(line string) (file string, lineNum int, detail string, ok bool) {
	file, rest, ok := strings.Cut(line, ":")
	if !ok || file == "" {
		return "", 0, "", false
	}

	lineText, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return "", 0, "", false
	}
	lineNum, err := strconv.Atoi(lineText)
	if err != nil {
		return "", 0, "", false
	}

	_, detail, ok = strings.Cut(rest, ":")
	if !ok {
		return "", 0, "", false
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "", 0, "", false
	}
	return file, lineNum, detail, true
}

func writeOutput(path string, report *Report, format OutputFormat) error {
	switch format {
	case FormatJSON:
		return writeJSON(path, report)
	default:
		return writeText(path, report)
	}
}

func ensureOutputDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0755)
}

func writeJSON(path string, report *Report) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func writeText(path string, report *Report) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "=== diago findings ===\n")
	fmt.Fprintf(&buf, "package: %s\n", report.Package)
	fmt.Fprintf(&buf, "benchmark filter: %s\n", report.BenchFilter)
	fmt.Fprintf(&buf, "threshold: %.1f%%\n\n", report.Threshold)

	// summary first
	fmt.Fprintf(&buf, "--- summary: top hotspots in your code ---\n")
	if len(report.Summary) == 0 {
		fmt.Fprintf(&buf, "no significant hotspots in user code.\n")
	} else {
		for i, s := range report.Summary {
			loc := ""
			if s.File != "" {
				loc = fmt.Sprintf(" (%s:%d)", s.File, s.Line)
			}
			fmt.Fprintf(&buf, "  %d. [%s] %s%s — %.2f%% cumulative\n", i+1, s.ProfileType, s.Function, loc, s.CumPct)
		}
	}

	fmt.Fprintf(&buf, "\n--- benchmark output ---\n")
	fmt.Fprintf(&buf, "%s\n", report.BenchmarkOutput)

	writeFindingsSection(&buf, "cpu hotspots", report.CPUFindings, report.Threshold)
	writeFindingsSection(&buf, "memory hotspots", report.MemFindings, report.Threshold)
	writeFindingsSection(&buf, "mutex contention hotspots", report.MutexFindings, report.Threshold)
	writeFindingsSection(&buf, "block/wait hotspots", report.BlockFindings, report.Threshold)

	fmt.Fprintf(&buf, "\n--- escape analysis (heap escapes) ---\n")
	if len(report.EscapeFindings) == 0 {
		fmt.Fprintf(&buf, "no heap escapes detected.\n")
	} else {
		for _, e := range report.EscapeFindings {
			fmt.Fprintf(&buf, "%s:%d: %s\n", e.File, e.Line, e.Detail)
		}
	}

	fmt.Fprintf(&buf, "\n--- raw cpu pprof output ---\n%s\n", report.RawCPU)
	fmt.Fprintf(&buf, "\n--- raw memory pprof output ---\n%s\n", report.RawMem)
	fmt.Fprintf(&buf, "\n--- raw mutex pprof output ---\n%s\n", report.RawMutex)
	fmt.Fprintf(&buf, "\n--- raw block pprof output ---\n%s\n", report.RawBlock)
	fmt.Fprintf(&buf, "\n--- raw escape analysis output ---\n%s\n", report.RawEscape)

	return os.WriteFile(path, buf.Bytes(), 0644)
}

func writeFindingsSection(buf *bytes.Buffer, title string, findings []Finding, threshold float64) {
	fmt.Fprintf(buf, "\n--- %s (>%.1f%% cumulative) ---\n", title, threshold)
	if len(findings) == 0 {
		fmt.Fprintf(buf, "no significant hotspots detected.\n")
		return
	}
	fmt.Fprintf(buf, "%-50s %-40s %8s %8s\n", "function", "location", "flat%", "cum%")
	fmt.Fprintf(buf, "%s\n", strings.Repeat("-", 108))
	for _, f := range findings {
		loc := ""
		if f.File != "" {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(buf, "%-50s %-40s %7.2f%% %7.2f%%\n", f.Function, loc, f.FlatPct, f.CumPct)
	}
}

func writeCompareText(path string, cr *CompareReport) error {
	if err := ensureOutputDir(path); err != nil {
		return err
	}
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "=== comparison report ===\n\n")

	writeChangedSection(&buf, "cpu improvements", cr.CPUImproved)
	writeChangedSection(&buf, "cpu regressions", cr.CPURegressed)
	writeChangedSection(&buf, "memory improvements", cr.MemImproved)
	writeChangedSection(&buf, "memory regressions", cr.MemRegressed)
	writeChangedSection(&buf, "mutex improvements", cr.MutexImproved)
	writeChangedSection(&buf, "mutex regressions", cr.MutexRegressed)
	writeChangedSection(&buf, "block improvements", cr.BlockImproved)
	writeChangedSection(&buf, "block regressions", cr.BlockRegressed)

	fmt.Fprintf(&buf, "\n--- new heap escapes ---\n")
	if len(cr.EscapesAdded) == 0 {
		fmt.Fprintf(&buf, "none.\n")
	} else {
		for _, e := range cr.EscapesAdded {
			fmt.Fprintf(&buf, "  + %s:%d: %s\n", e.File, e.Line, e.Detail)
		}
	}

	fmt.Fprintf(&buf, "\n--- removed heap escapes ---\n")
	if len(cr.EscapesRemoved) == 0 {
		fmt.Fprintf(&buf, "none.\n")
	} else {
		for _, e := range cr.EscapesRemoved {
			fmt.Fprintf(&buf, "  - %s:%d: %s\n", e.File, e.Line, e.Detail)
		}
	}

	return os.WriteFile(path, buf.Bytes(), 0644)
}

func writeChangedSection(buf *bytes.Buffer, title string, changes []ChangedFinding) {
	fmt.Fprintf(buf, "--- %s ---\n", title)
	if len(changes) == 0 {
		fmt.Fprintf(buf, "none.\n\n")
		return
	}
	for _, c := range changes {
		loc := ""
		if c.File != "" {
			loc = fmt.Sprintf(" (%s:%d)", c.File, c.Line)
		}
		fmt.Fprintf(buf, "  %s%s: %.2f%% -> %.2f%% (%+.2f%%)\n", c.Function, loc, c.OldCumPct, c.NewCumPct, c.DeltaPct)
	}
	fmt.Fprintf(buf, "\n")
}
