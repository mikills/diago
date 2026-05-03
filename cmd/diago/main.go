package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/mikills/diago/diago"
)

const installPackage = "github.com/mikills/diago/cmd/diago"

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version", "version":
			printVersion()
			return
		case "compare":
			runCompare(os.Args[2:])
			return
		case "audit":
			runAudit(os.Args[2:])
			return
		case "perf", "profile":
			runProfile(os.Args[2:])
			return
		case "upgrade":
			runUpgrade(os.Args[2:])
			return
		}
	}

	args, perf := stripPerfFlag(os.Args[1:])
	if perf {
		runProfile(args)
		return
	}
	runAudit(args)
}

func printVersion() {
	v, c, d := versionInfo()
	fmt.Printf("diago %s\ncommit: %s\nbuilt: %s\n", v, c, d)
}

func versionInfo() (string, string, string) {
	v, c, d := version, commit, date
	if info, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "unknown" {
					c = setting.Value
				}
			case "vcs.time":
				if d == "unknown" {
					d = setting.Value
				}
			}
		}
	}
	return v, c, d
}

func runUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "print the go install command without running it")
	fs.Parse(args)

	versionArg := "latest"
	remaining := fs.Args()
	if len(remaining) > 1 {
		fmt.Fprintf(os.Stderr, "usage: diago upgrade [--dry-run] [latest|vX.Y.Z]\n")
		os.Exit(1)
	}
	if len(remaining) == 1 {
		versionArg = remaining[0]
	}

	installTarget := installPackage + "@" + versionArg
	cmdArgs := []string{"install", installTarget}
	fmt.Printf("go %s\n", strings.Join(cmdArgs, " "))
	if *dryRun {
		return
	}

	cmd := exec.Command("go", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "upgrade failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("upgrade complete")
	fmt.Println("note: go install writes to $(go env GOPATH)/bin; make sure that directory is on PATH")
}

func stripPerfFlag(args []string) ([]string, bool) {
	out := args[:0]
	perf := false
	for _, arg := range args {
		if arg == "--perf" || arg == "-perf" {
			perf = true
			continue
		}
		out = append(out, arg)
	}
	return out, perf
}

func runProfile(args []string) {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	target := fs.String("target", "./...", "package path to profile")
	output := fs.String("output", "diago_findings.txt", "output file for findings")
	bench := fs.String("bench", ".", "benchmark filter regex")
	threshold := fs.Float64("threshold", 1.0, "minimum cumulative percentage to report")
	format := fs.String("format", "text", "output format: text or json")
	fs.Parse(args)

	cfg := diago.Config{
		TargetPath:  *target,
		OutputFile:  *output,
		BenchFilter: *bench,
		Threshold:   *threshold,
		Format:      parseFormat(*format),
	}

	fmt.Printf("profiling %s (bench=%s, threshold=%.1f%%, format=%s)\n",
		cfg.TargetPath, cfg.BenchFilter, cfg.Threshold, cfg.Format)

	report, err := diago.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("cpu hotspots:   %d\n", len(report.CPUFindings))
	fmt.Printf("mem hotspots:   %d\n", len(report.MemFindings))
	fmt.Printf("mutex hotspots: %d\n", len(report.MutexFindings))
	fmt.Printf("block hotspots: %d\n", len(report.BlockFindings))
	fmt.Printf("heap escapes:   %d\n", len(report.EscapeFindings))
	fmt.Printf("findings written to %s\n", cfg.OutputFile)
}

func runCompare(args []string) {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	output := fs.String("output", "comparison.txt", "output file for comparison")
	format := fs.String("format", "text", "output format: text or json")
	fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) != 2 {
		fmt.Fprintf(os.Stderr, "usage: diago compare [-output FILE] [-format text|json] <before.json> <after.json>\n")
		os.Exit(1)
	}

	cr, err := diago.Compare(remaining[0], remaining[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := diago.WriteCompareReport(*output, cr, parseFormat(*format)); err != nil {
		fmt.Fprintf(os.Stderr, "error writing comparison: %v\n", err)
		os.Exit(1)
	}

	total := len(cr.CPUImproved) + len(cr.MemImproved) + len(cr.MutexImproved) + len(cr.BlockImproved)
	regTotal := len(cr.CPURegressed) + len(cr.MemRegressed) + len(cr.MutexRegressed) + len(cr.BlockRegressed)

	fmt.Printf("improvements: %d, regressions: %d\n", total, regTotal)
	fmt.Printf("escapes added: %d, removed: %d\n", len(cr.EscapesAdded), len(cr.EscapesRemoved))
	fmt.Printf("comparison written to %s\n", *output)
}

func runAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	target := fs.String("target", "./...", "package path to audit")
	output := fs.String("output", "diago_audit.txt", "output file for audit")
	format := fs.String("format", "text", "output format: text or json")
	race := fs.Bool("race", false, "run go test -race")
	coverage := fs.Bool("coverage", true, "run go test -coverprofile and summarize coverage")
	deps := fs.Bool("deps", true, "run go list -deps")
	astChecks := fs.Bool("ast", true, "run native AST checks")
	summaryLimit := fs.Int("summary-limit", 25, "maximum critical/high AST findings in the summary; use -1 for all")
	fs.Parse(args)

	report, err := diago.RunAudit(diago.AuditConfig{
		TargetPath:   *target,
		OutputFile:   *output,
		Format:       parseFormat(*format),
		Race:         *race,
		Coverage:     *coverage,
		Deps:         *deps,
		AST:          *astChecks,
		SummaryLimit: *summaryLimit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	printAuditSummary(report, *output)
	if !report.OverallPass {
		os.Exit(1)
	}
}

func printAuditSummary(report *diago.AuditReport, output string) {
	s := report.Summary
	fmt.Println("\n=== diago audit summary ===")
	fmt.Printf("target: %s\n", report.Target)
	fmt.Printf("overall: %s\n", passFail(report.OverallPass))
	fmt.Printf("checks: %d passed, %d failed\n", s.ChecksPassed, s.ChecksFailed)
	if report.Coverage != nil {
		fmt.Printf("coverage: %.1f%%\n", s.CoverageTotalPct)
	}
	if s.DependencyCount > 0 {
		fmt.Printf("dependencies: %d\n", s.DependencyCount)
	}
	printASTSummary(s)
	fmt.Printf("\nfull report: %s\n", output)
}

func printASTSummary(s diago.AuditSummary) {
	if s.ASTTotal == 0 {
		return
	}
	fmt.Printf("\nast findings: %d\n", s.ASTTotal)
	printCountTable("by severity", []string{"critical", "high", "medium", "low"}, s.ASTBySeverity)
	printCountTable("by rule", []string{
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
		"large-file",
		"large-package",
	}, s.ASTByRule)
	if len(s.CriticalHigh) > 0 {
		fmt.Println("\ncritical/high findings:")
		for _, f := range s.CriticalHigh {
			fmt.Printf("  - [%s] %s at %s:%d", f.Severity, f.Rule, f.File, f.Line)
			if f.Symbol != "" {
				fmt.Printf(" (%s)", f.Symbol)
			}
			fmt.Printf(" — %s\n", f.Message)
		}
	}
}

func printCountTable(title string, order []string, counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", title)
	seen := map[string]bool{}
	for _, key := range order {
		if count := counts[key]; count > 0 {
			fmt.Printf("  %-24s %d\n", key, count)
			seen[key] = true
		}
	}
	for key, count := range counts {
		if !seen[key] {
			fmt.Printf("  %-24s %d\n", key, count)
		}
	}
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func parseFormat(s string) diago.OutputFormat {
	if s == "json" {
		return diago.FormatJSON
	}
	return diago.FormatText
}
