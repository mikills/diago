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
Audit does native AST checks for complexity, error handling, resource handling, context use, dead-code hints, size checks, and maintainability smells.

Generated files (oapi-codegen `*.gen.go`, sqlc `*.sql.go`, `*.generated.*`, or any file with a `// Code generated ... DO NOT EDIT.` header) are skipped across all checks — AST, `-modernize`, and `-u1000` — since they can only be changed via codegen config, not hand edits. Pass `-include-generated` to audit them anyway.

Opt into extra checks:
```sh
diago -target ./... -coverage -deps -modernize
diago -target ./... -modernize -fix
diago -target ./... -deadcode
diago -target ./... -deadcode -fix
diago -target ./... -u1000
```

Format code and enforce line length with `golines`:
```sh
diago format -target . -max-len 120
# alias: diago fmt
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
-modernize       run gopls modernize diagnostics (default false)
-deadcode        report dead-code hints. With -fix, removes narrow unexported dead functions
-u1000           run Staticcheck U1000 unused-code diagnostics
-fix             apply fixes for -modernize or -deadcode
-include-generated  include findings from generated files (skipped by default)
-summary-limit   max critical/high findings in summary. Use -1 for all (default 25)
```

Format:

```txt
-target          source directory to format (default .)
-max-len         maximum line length passed to golines (default 120)
-golines         golines binary to use; falls back to go run github.com/segmentio/golines@latest
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

## Skills

Install bundled agent docs so AI coding agents know how and when to run diago. The docs are embedded in the binary, so `diago skills` always installs the version that matches your installed diago.

Claude Code (default) — installs a skill:

```sh
diago skills            # install into ~/.claude/skills (user-level)
diago skills -project   # install into ./.claude/skills (project-local)
```

Any other agent (Codex, Cursor, Copilot, …) — writes the cross-agent `AGENTS.md` convention:

```sh
diago skills -agent agents          # write ./.agents/diago.md + reference block in ./AGENTS.md
diago skills -agent agents -dir DIR # use DIR as the project root
```

The `agents` target writes the full doc to `.agents/diago.md` and adds a managed `<!-- BEGIN diago -->`…`<!-- END diago -->` block to `AGENTS.md`. Re-running replaces that block in place and leaves the rest of your `AGENTS.md` untouched.

Common flags:

```txt
-agent     target: claude (default) or agents
-project   claude target only: install into ./.claude/skills instead of ~/.claude/skills
-dir       destination base directory (overrides the default location)
-force     overwrite existing files
-list      list bundled skills without installing
```

After installing, restart Claude Code (or your agent) to pick up the new docs.

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
