# diago

Go diagnostics from one command. `diago` runs built-in Go checks plus native source analysis. It can also collect pprof data from benchmarks.


## Installation
```sh
go install github.com/mikills/diago/cmd/diago@latest
diago --version
```

## Usage

### Audit (default)

```sh
diago -target ./...
diago -target ./... -format json -output diago-audit.json
diago -target ./... -race
```
Audit does native AST checks for complexity, error handling, resource handling, context use, dead-code hints, generated-file-aware size checks, and maintainability smells

Opt into extra checks:
```sh
diago -target ./... -coverage -deps
```

Disable AST checks:
```sh
diago -target ./... -ast=false
```

Example output:

```txt
=== diago audit summary ===
target: ./...
overall: FAIL
checks: 2 passed, 1 failed

ast findings: 711

by severity:
  critical                 7
  high                     98
  medium                   327
  low                      279

by rule:
  cyclomatic-complexity    130
  function-length          24
  resource-not-closed      28
  ignored-call-result      66

critical/high findings:
  - [high] function-length at cmd/server/main.go:68 (main) — function has 156 lines
  - [high] resource-not-closed at internal/app/users_http.go:56 (usersHTTP.handleListUsers) — query is opened/created but Close is not called in the function

recommendations:
  - [critical/high] cyclomatic-complexity: Split complex branching into smaller functions, guard clauses, or table-driven logic. (130 findings)
    symbols: main, handleCreateUser, handleUpdateUser
  - [high/medium] resource-not-closed: Close opened resources, usually with defer immediately after the error check. (28 findings)
    symbols: handleListUsers, requestOptionsFromRequest

full report: .diago/audit.txt
```

### Performance profiling

Requires benchmarks.

```sh
diago --perf -target ./... -bench .
diago --perf -target ./pkg -bench BenchmarkHot -format json -output perf.json
```

Perf mode captures CPU, memory, mutex, block, and escape-analysis findings.

Example output:

```txt
top findings:
  - [memory] 14.30% pkg.ParseEscapeOutput at file.go:724
    > findings = append(findings, EscapeFinding{...})
    vars: findings
    calls: append
    allocs: EscapeFinding
    recommendation: inspect detected source signals before changing behavior.

full report: .diago/perf.txt
```

### Compare perf reports

```sh
diago compare before.json after.json
diago compare -format json -output comparison.json before.json after.json
```

## Flags

Audit:

```txt
-target          package path (default ./...)
-output          report path (default .diago/audit.txt)
-format          text or json (default text)
-race            run go test -race
-coverage        collect coverage (default false)
-deps            list dependencies (default false)
-ast             run native AST checks (default true)
-summary-limit   max critical/high findings in summary. Use -1 for all (default 25)
```

Perf:

```txt
--perf           enable performance mode
-target          package path (default ./...)
-bench           benchmark regex (default .)
-threshold       minimum cumulative percentage (default 1.0)
-output          report path (default .diago/perf.txt)
-format          text or json (default text)
```

## Upgrade

```sh
diago upgrade           # installs latest with go install
diago upgrade v0.1.0    # installs a specific version
```

## Notes

- Reports are written to `.diago/` by default. Add `.diago/` to `.gitignore` or pass `-output`.
- `go install` writes to `$(go env GOPATH)/bin`. Make sure that directory is on `PATH`.
- Audit mode does not require benchmarks.
- Perf mode requires `func BenchmarkX(b *testing.B)` benchmarks.
- Perf mode profiles benchmark runs, not live pprof endpoints. Live pprof endpoint support is coming soon.
