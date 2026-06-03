---
name: diago
description: Run Go diagnostics, audits, and performance profiling with the diago CLI. Use when the user wants to audit a Go codebase for complexity/error-handling/resource/dead-code/maintainability issues, check coverage or run -race tests, modernize code with gopls, find unused code (Staticcheck U1000), format Go with gofmt + golines, or profile benchmarks (CPU/memory/mutex/block/escape) and compare perf reports. Triggers include "audit this Go code", "run diago", "check code complexity", "find dead code", "profile this benchmark", "modernize my Go".
---

# diago

`diago` is a Go diagnostics CLI. One command runs built-in Go checks plus native AST source analysis, and it can also collect pprof data from benchmarks.

## Prerequisites

The `diago` binary must be on `PATH`. Check with `diago --version`. If it is missing:

```sh
go install github.com/mikills/diago/cmd/diago@latest
```

`go install` writes to `$(go env GOPATH)/bin` — make sure that directory is on `PATH`. Upgrade later with `diago upgrade` (or `diago upgrade vX.Y.Z` for a specific version).

## When to use which command

- **Code quality / review** → `diago audit` (the default command)
- **Formatting** → `diago format`
- **Benchmark performance** → `diago --perf`
- **Comparing two perf runs** → `diago compare`

## Audit (default command)

Runs native AST checks for cyclomatic complexity, function length, error handling, resource handling (unclosed resources), context use, dead-code hints, generated-file-aware size checks, and maintainability smells. `audit` is the default, so `diago` with no subcommand runs it.

```sh
diago -target ./...                                   # audit the whole module
diago -target ./... -format json -output audit.json   # machine-readable output
diago -target ./... -race                             # also run go test -race
```

Opt into extra checks:

```sh
diago -target ./... -coverage -deps   # coverage summary + dependency list
diago -target ./... -modernize        # gopls modernize diagnostics
diago -target ./... -modernize -fix   # apply the modernize fixes
diago -target ./... -deadcode         # report dead-code hints
diago -target ./... -deadcode -fix    # remove narrow unexported dead functions
diago -target ./... -u1000            # Staticcheck U1000 unused-code diagnostics
diago -target ./... -ast=false        # disable native AST checks
```

`-fix` applies fixes only for `-modernize` and/or `-deadcode`. Always review the diff afterward — `-deadcode -fix` deletes code.

The command exits non-zero when the audit fails, so it can gate CI. A summary prints to stdout and the full report is written to `-output` (default `.diago/audit.txt`).

### Audit flags

```txt
-target          package path (default ./...)
-output          report path (default .diago/audit.txt)
-format          text or json (default text)
-race            run go test -race
-coverage        collect coverage (default false)
-deps            list dependencies (default false)
-ast             run native AST checks (default true)
-modernize       run gopls modernize diagnostics (default false)
-deadcode        report dead-code hints. With -fix, removes narrow unexported dead functions
-u1000           run Staticcheck U1000 unused-code diagnostics
-fix             apply fixes for -modernize or -deadcode
-summary-limit   max critical/high findings in summary. Use -1 for all (default 25)
```

## Format

Formats with `gofmt` and enforces line length with `golines` (falls back to `go run github.com/segmentio/golines@latest` when the binary is missing).

```sh
diago format -target . -max-len 120
diago fmt -target .            # fmt is an alias for format
```

```txt
-target    source directory to format (default .)
-max-len   maximum line length passed to golines (default 120)
-golines   golines binary to use
```

## Performance profiling

Requires `func BenchmarkX(b *testing.B)` benchmarks. Profiles benchmark runs (not live pprof endpoints). Captures CPU, memory, mutex, block, and escape-analysis findings.

```sh
diago --perf -target ./... -bench .
diago --perf -target ./pkg -bench BenchmarkHot -format json -output perf.json
```

```txt
--perf       enable performance mode
-target      package path (default ./...)
-bench       benchmark regex (default .)
-threshold   minimum cumulative percentage (default 1.0)
-output      report path (default .diago/perf.txt)
-format      text or json (default text)
```

## Compare perf reports

Diff two JSON perf reports to spot improvements and regressions (run `--perf -format json` before and after a change):

```sh
diago compare before.json after.json
diago compare -format json -output comparison.json before.json after.json
```

## Notes

- Reports are written to `.diago/` by default. Add `.diago/` to `.gitignore` or pass `-output`.
- Audit mode does not require benchmarks; perf mode does.
- When reporting findings to the user, lead with the critical/high AST findings and the recommendations block — they name the specific symbols to fix.
