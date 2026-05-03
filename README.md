# diago

Go diagnostics from one command.

```sh
go install github.com/mikills/diago/cmd/diago@latest
diago -target ./...
diago --version
diago upgrade
```

`diago` runs built-in Go checks plus native source analysis. It can also collect pprof data from benchmarks.

## Usage

### Audit (default)

```sh
diago -target ./...
diago -target ./... -format json -output diago-audit.json
diago -target ./... -race
```

Audit runs:

- `go test`
- `go vet`
- coverage summary
- dependency count
- native AST checks for complexity, error handling, resource handling, context use, dead-code hints, generated-file-aware size checks, and maintainability smells

Disable expensive/noisy parts:

```sh
diago -target ./... -coverage=false -deps=false
diago -target ./... -ast=false
```

### Performance profiling

Requires benchmarks.

```sh
diago --perf -target ./... -bench .
diago --perf -target ./pkg -bench BenchmarkHot -format json -output perf.json
```

Perf mode captures CPU, memory, mutex, block, and escape-analysis findings.

### Compare perf reports

```sh
diago compare before.json after.json
diago compare -format json -output comparison.json before.json after.json
```

## Flags

Audit:

```txt
-target          package path (default ./...)
-output          report path (default diago_audit.txt)
-format          text or json (default text)
-race            run go test -race
-coverage        collect coverage (default true)
-deps            list dependencies (default true)
-ast             run native AST checks (default true)
-summary-limit   max critical/high findings in summary; -1 for all (default 25)
```

Perf:

```txt
--perf           enable performance mode
-target          package path (default ./...)
-bench           benchmark regex (default .)
-threshold       minimum cumulative percentage (default 1.0)
-output          report path (default diago_findings.txt)
-format          text or json (default text)
```

## Upgrade

```sh
diago upgrade           # installs latest with go install
diago upgrade v0.1.0    # installs a specific version
```

## Notes

- `go install` writes to `$(go env GOPATH)/bin`; make sure that directory is on `PATH`.
- Audit mode does not require benchmarks.
- Perf mode requires `func BenchmarkX(b *testing.B)` benchmarks.
- Perf mode profiles benchmark runs, not live pprof endpoints.
