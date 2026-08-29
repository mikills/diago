# Go 1.27 tooling and modern-Go guidance — video extraction

- Source: [Go 1.27 Explained by the Go Team | Release Party](https://www.youtube.com/watch?v=UkswvuLfUMQ)
- Publisher: JetBrains
- Length: approximately 74 minutes
- Transcript source: local Pi `youtube_transcript` extension, English captions, `full_text` format
Related repository: [JetBrains/go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)

> The source is an automatically generated YouTube transcript. This document corrects obvious caption errors (for example, “Go Please” to `gopls`, “Static check” to Staticcheck, and speaker names), removes filler, and condenses repeated discussion. It is an engineering extraction, not an authoritative release specification or a verbatim transcript.

## Why this matters to diago

The strongest message in the video is that diagnostics, modernization, and agent guidance should share a common, version-aware foundation:

- Go's `analysis` framework lets checks operate on type-annotated package syntax and run under different drivers.
- `go vet`, `go fix`, `gopls`, and Staticcheck represent complementary delivery mechanisms for analyzers.
- Modernizers are deterministic transformations, not vague style advice.
- Coding agents work best with command-line tools and compact, machine-readable guidance.
- Modern recommendations must be gated by the Go version declared by the target module.

This aligns closely with diago, but also exposes gaps: diago has its own AST/type analysis, invokes an internal `gopls` modernizer command at `@latest`, and only enables Staticcheck's `U1000` check.

## Executive summary

1. **Adopt the analyzer model, not necessarily one implementation.** Treat a diagnostic as a reusable rule whose execution can be native, command-backed, or sourced from an upstream analyzer.
2. **Detect the target module's Go version.** Never recommend syntax or standard-library APIs newer than the target's `go.mod` declaration.
3. **Separate correctness checks from modernization.** Mistakes belong in a default audit track; optional, safe rewrites belong in a modernization/fix track.
4. **Expand Staticcheck deliberately.** Diago currently runs only `U1000`; broader Staticcheck coverage should be normalized and deduplicated against `go vet`, `gopls`, and native findings.
5. **Add the high-signal checks demonstrated in the video.** Missing `sql.Rows.Err`/`bufio.Scanner.Err`, quadratic string concatenation, and old untyped atomic operations are concrete candidates.
6. **Use the JetBrains guideline catalog as a research inventory.** Its current JSON contains 54 version-gated idioms from Go 1.0 through 1.27. It is guidance for agents, not itself a static analyzer.
7. **Make diago agent-friendly.** Stable CLI output, rule metadata, safe fixes, and concise explanations are more useful to agents than requiring them to speak LSP.

---

## Cleaned, relevant transcript

### 26:22–27:39 — keeping tooling compatible with language releases

Alan Donovan explains that each Go release creates two kinds of work for tooling:

1. Keep existing tools working as language and library behavior changes.
2. Add or extend features so tools can take advantage of the new capabilities.

Some changes are localized, such as generic methods primarily affecting the type checker. Others, including generics and forward compatibility, have a wide blast radius across syntax trees, scanners, type checking, and other public tooling APIs. These changes require auditing, risk mitigation, and clear compatibility communication.

**Diago takeaway:** upstream language evolution can invalidate assumptions throughout a custom AST/type analyzer. Compatibility tests should run against multiple supported Go versions, and version-sensitive rules should be explicit.

### 27:39–28:38 — what `gopls` provides

`gopls` is the Go language server. Editors ask it for Go-specific information and operations such as completion, API documentation, assembly views, test generation, analysis, and refactoring. The editor itself does not need to implement Go semantics.

`gopls` releases independently, roughly four times per year, with releases prepared ahead of Go toolchain releases so new language features have editor support immediately.

**Diago takeaway:** diago should consume stable `gopls` capabilities where practical instead of duplicating them, but it should not couple core behavior to an undocumented internal command without pinning or compatibility tests.

### 29:07–30:22 — one language change can cross many tool features

Promoted fields in struct literals affect several `gopls` features:

- completion candidates;
- the `fillstruct` quick fix;
- inlay hints that expose implicit selections; and
- a modernizer that rewrites older explicit embedded-field literals to the concise form.

The modernizer is also available through `go fix`, allowing a codebase-wide cleanup.

**Diago takeaway:** model modernization rules once, then allow different front ends—reporting, interactive use, and batch fixes—to use the same rule and metadata.

### 30:25–31:13 — the Go analyzer framework

The Go analyzer framework is an interface for writing analyzers, linters, or checkers. At its core, an analyzer has a run function that receives a package's type-annotated syntax trees and reports problems. The same analyzer can run in multiple driver programs.

The video distinguishes two broad destinations:

- analyzers that identify mistakes run through `go vet`;
- analyzers that identify a better modern expression and may provide a fix run through `go fix`.

The relevant library is [`golang.org/x/tools/go/analysis`](https://pkg.go.dev/golang.org/x/tools/go/analysis).

**Diago takeaway:** diago's native findings can retain their current representation, but analyzer-backed rules would provide standard prerequisites, facts, diagnostics, and suggested fixes.

### 31:13–31:45 — Staticcheck and `gopls`

Staticcheck, created by Dominik Honnef, contains hundreds of high-quality analyzers. The talk states that `gopls` incorporates the Staticcheck analyzers, with many enabled by default. Around 150 analyzers run interactively in the described configuration, roughly half originating from Staticcheck.

Relevant project: [dominikh/go-tools](https://github.com/dominikh/go-tools).

**Diago takeaway:** `-u1000` uses only a narrow part of a much larger available suite. Expansion should be intentional, configurable, and deduplicated rather than simply turning on everything without a finding policy.

### 31:45–32:54 — concrete new diagnostics and modernizers

The talk demonstrates four useful checks:

1. **Promoted struct fields:** simplify explicit embedded struct construction when the target Go version supports direct promoted fields.
2. **Typed atomics:** replace old function-style atomic operations such as `atomic.AddInt32` with methods on typed atomic values such as `atomic.Int32`. Typed atomics prevent accidental non-atomic access.
3. **Repeated string concatenation:** detect loops using `s += item`. This can be accidentally quadratic and, for attacker-controlled input, may become a denial-of-service vector. Rewrite suitable cases to a linear implementation using `strings.Builder`.
4. **Terminal iterator errors:** after iterating `*sql.Rows`, check `rows.Err()`. The same error pattern exists for `bufio.Scanner`, which requires checking `scanner.Err()` after scanning.

**Diago takeaway:** terminal iterator-error checks are correctness diagnostics and should be high priority. Typed atomics and string-builder rewrites belong in the modernization track and should be version-aware and fix-safety tested.

### 33:25–35:32 — refactoring and a CLI designed for agents

The Go team is working in two areas:

- richer interactive refactoring through LSP; and
- a command-line interface exposing `gopls` functionality efficiently to humans and coding agents.

The motivation for the CLI is explicit: agents are effective users of shell commands and scripts, while LSP is a comparatively complex protocol for them to implement.

**Diago takeaway:** this validates diago's command-line-first design. JSON output, stable rule IDs, deterministic ordering, baselines, compact summaries, and safe fixes are core agent features—not secondary presentation concerns.

### Q&A — modernizers as ecosystem migration infrastructure

The speakers describe modernizers as a way to keep a codebase and the wider ecosystem coherent as Go gains new ways to express logic. The Go team is also considering modernization during language and library proposal review: when a new feature is superior to an older pattern, its proposal may include a recommendation to build a modernizer.

They also note that safe source transformations are difficult to implement correctly and that the infrastructure needs to make breakage harder.

**Diago takeaway:** every fix needs behavior-focused tests, version gates, and explicit safety classification. Reporting can be broad; automatic fixing should remain narrow.

### 58:01–1:04:20 — goroutine leaks and pre-commit checks

The GoLand demonstration uses Go's goroutine leak profile to identify a goroutine blocked forever after the HTTP request that spawned it has already timed out. The IDE connects a leaked stack directly back to source.

GoLand also exposes `go fix` inspections and can run checks during commit.

**Diago takeaway:** future performance work could ingest goroutine-leak profile data and map stacks to project symbols, complementing current CPU, memory, mutex, block, and escape-analysis findings. A documented pre-commit mode could also make diago's safe checks easier to adopt.

### 1:06:28–1:07:42 — Modern Go Guidelines for coding agents

JetBrains presents the Modern Go Guidelines plugin for coding agents. Its purpose is to compensate for training-data lag and frequency bias: an agent may know an old pattern better simply because more old code exists in its training set.

The demonstrated example replaces a manual slice membership loop with `slices.Contains`. The guidance is designed for Claude, Cursor, Junie, Codex, and other agents.

Repository: [JetBrains/go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines).

**Diago takeaway:** deterministic diagnostics should be the executable source of truth where possible, while concise version-gated guidance helps an agent produce modern code before a diagnostic is needed.

---

## Tool and library inventory

| Component | Role described in the video | Potential diago use |
| --- | --- | --- |
| `golang.org/x/tools/go/analysis` | Framework for type-aware analyzers and suggested fixes | Standardize new correctness and modernization rules |
| `go vet` | Driver for analyzers that identify mistakes | Already run by default; preserve and normalize important diagnostics |
| `go fix` / modernizers | Batch-safe migration to newer Go idioms | Existing `-modernize -fix` track; improve stability and rule detail |
| `gopls` | Language server providing analysis and refactoring | Continue integration, but prefer stable interfaces and pinned/tested behavior |
| Staticcheck | Large suite of third-party analyzers | Expand beyond U1000 under a configurable profile |
| `strings.Builder` | Linear construction for suitable repeated-concatenation loops | Modernization/performance diagnostic with cautious fix support |
| typed `sync/atomic` values | Safer alternative to old function-style atomics | Version-aware modernizer |
| `database/sql.Rows.Err` | Reports iteration failures after row scanning | Native or analyzer-backed correctness check |
| `bufio.Scanner.Err` | Reports scanning failures after the loop | Native or analyzer-backed correctness check |
| goroutine leak profile | Identifies goroutines blocked after their calling path disappears | Candidate performance/reliability profile ingestion |
| JetBrains Modern Go Guidelines | Version-aware agent instructions | Research catalog and possible rule metadata input |

---

## Extraction from `JetBrains/go-modern-guidelines`

The repository was inspected at commit `40781f167719913666fe2a7dc1c77ea6f256df0a` (2026-08-19).

### What it is

The project is an agent plugin plus a small Go CLI. It:

- detects a project's Go version from `go.mod`;
- lists only guidance supported by that version;
- can explain selected rules with details and before/after examples; and
- stores the canonical guideline data in `internal/guidelines/guidelines.json`.

The repository's `FEATURES.md` is explicitly marked work in progress. For research, the JSON catalog and tested CLI behavior should be treated as more authoritative than the feature table.

### Catalog shape

At the inspected commit, the JSON catalog contains **54 guidelines** spanning Go 1.0 through Go 1.27:

| Go version | Guideline count |
| --- | ---: |
| 1.27 | 6 |
| 1.26 | 2 |
| 1.25 | 1 |
| 1.24 | 4 |
| 1.23 | 4 |
| 1.22 | 5 |
| 1.21 | 19 |
| 1.20 | 5 |
| 1.19 | 2 |
| 1.18 | 3 |
| 1.13 | 1 |
| 1.8 | 1 |
| 1.0 | 1 |

Each entry contains:

- `id`;
- `since_version`;
- a one-line guideline;
- implementation details; and
- one or more before/after examples.

For diago's current `go 1.22.2` module declaration, **37 of the 54** entries are in-version. This does not mean all 37 should become native diago checks: many are already covered by `gopls` modernizers, some are style choices, and some need type/data-flow precision to avoid noise.

### Useful rule families

- Collections: `slices.Contains`, `slices.Index`, `slices.Sort`, `maps.Clone`, `maps.Copy`, `clear`, and related helpers.
- Iteration: integer ranges and removal of obsolete loop-variable capture copies.
- Errors: `errors.Is`, `errors.Join`, and newer typed matching APIs.
- Context: cancellation causes, `context.AfterFunc`, and test contexts.
- Concurrency: typed atomics, `sync.OnceFunc`/`OnceValue`, and `WaitGroup.Go`.
- Allocation-sensitive APIs: `strings.SplitSeq`, `bytes.FieldsSeq`, and `fmt.Appendf`.
- Testing: `b.Loop`.
- JSON: `omitzero` and, for Go 1.27+, explicit guidance around `encoding/json/v2` migration.
- Go 1.27 additions: generic methods, promoted-field literals, `CutLast`, standard UUID support, and URL cloning.

### Licensing note

The guideline repository is Apache-2.0 licensed. Diago may study or reuse appropriately attributed material, but copying catalog content into this repository would require retaining the license and applicable notices. Linking to upstream rules or independently implementing equivalent diagnostics avoids unnecessary catalog duplication and drift.

---

## Current diago fit and gaps

### What diago already does well

- Runs `go test` and `go vet` by default.
- Performs native AST and `go/types` analysis without requiring an external service.
- Normalizes native and external diagnostics into `ASTFinding`.
- Provides stable sorting, compact recommendations, JSON output, generated-file filtering, suppression directives, and baselines.
- Offers optional `gopls` modernization and fixes.
- Offers optional Staticcheck `U1000` diagnostics.
- Already presents a command-line and JSON interface suitable for coding agents.

### Gaps exposed by the video

1. **No target Go-version model.** Recommendations are not centrally gated by the target module's `go`/`toolchain` declarations.
2. **Unstable modernizer invocation.** `diago/modernize.go` invokes `golang.org/x/tools/gopls/internal/analysis/modernize/cmd/modernize@latest`, coupling behavior to an internal path and an unpinned version.
3. **Very narrow Staticcheck use.** `diago/staticcheck.go` filters exclusively to `U1000`.
4. **No analyzer abstraction.** Native rules are direct AST walks; external tools use separate parsers. There is no shared rule descriptor or analyzer adapter.
5. **Coarse modernization identity.** All modernizer findings use the rule `modernize`; the upstream category is stored as `Symbol`, limiting rule-level configuration, baselines, and guidance.
6. **No fix-safety metadata.** The product separates report-only and fix modes operationally, but rule metadata does not describe whether a fix is mechanical, review-required, or unavailable.
7. **No iterator terminal-error checks identified in this review.** `sql.Rows.Err()` and `bufio.Scanner.Err()` are high-signal correctness opportunities.
8. **No goroutine leak-profile ingestion.** Performance mode covers several profiles, but not the new leak profile demonstrated in the video.

---

## Recommended implementation sequence

### P0 — make modernization reproducible

1. Detect the target module's Go version, including workspace/module resolution and the `toolchain` directive where relevant.
2. Record tool versions in `AuditCheck` output or report metadata.
3. Replace `@latest` execution with a pinned, tested version or a stable installed-tool contract.
4. Preserve the upstream analyzer/category as a stable sub-rule, for example `modernize/rangeint`, rather than collapsing every finding into one rule.
5. Add compatibility fixtures for the minimum supported Go version and the newest supported release.

### P1 — add high-signal correctness diagnostics

Implement or consume analyzers for:

- `sql.Rows` iteration without a reachable terminal `Err()` check;
- `bufio.Scanner` loops without a reachable terminal `Err()` check; and
- broader Staticcheck correctness checks under an opt-in profile.

Start report-only. Add suppressions through the existing rule-scoped `//diago:ignore` mechanism. Measure duplicate and false-positive rates on diago and representative external repositories before enabling checks by default.

### P1 — introduce a rule catalog

Add internal metadata along these lines:

```go
type RuleDescriptor struct {
    ID             string
    Kind           RuleKind // correctness, modernization, performance, maintainability
    SinceGo        string
    DefaultEnabled bool
    Severity       string
    Confidence     string
    FixSafety      FixSafety // none, mechanical, review-required
    Source         string    // native, go-vet, gopls, staticcheck
    Summary        string
}
```

Use it to drive:

- version gating;
- CLI rule discovery;
- output severity/confidence;
- fix eligibility;
- recommendation text; and
- deduplication across providers.

### P2 — modernizer coverage and fix policy

Evaluate typed atomics and repeated string concatenation through the upstream modernizer first. Only implement native equivalents if upstream output is unavailable, insufficiently stable, or cannot be normalized.

For automatic fixes:

- require type-aware matching;
- verify imports and formatting;
- test idempotence;
- compile and run relevant tests after applying the fix;
- refuse ambiguous transformations; and
- expose the exact rule and tool version that produced the edit.

### P2 — agent-facing explanations

Add commands such as:

```text
diago rules
diago rules --supported-by ./...
diago explain sql-rows-err
diago explain modernize/rangeint
```

Keep the default audit output compact. Detailed explanations can include why the rule matters, the minimum Go version, examples, whether a fix exists, and the upstream source.

### P3 — goroutine leak diagnostics

Research the Go 1.27 goroutine leak profile as a new performance/reliability input. A useful diago result would:

- collect the profile from a benchmark or test workload;
- identify leaked project-owned stacks;
- map the first owned frame to a file and symbol;
- group equivalent stacks; and
- explain that a reported blocked goroutine is evidence from the observed workload, not proof that every execution leaks.

---

## Decisions to preserve

- Keep correctness audits useful without requiring an IDE or LSP client.
- Prefer stable upstream analyzers over duplicate heuristic implementations.
- Keep automatic fixes narrower than diagnostics.
- Gate every modernization by the target project's Go version.
- Avoid turning the entire JetBrains guidance catalog into default warnings; usefulness and precision matter more than rule count.
- Preserve deterministic JSON and baseline behavior for agents and CI.

## Suggested first issue

**Title:** Add version-aware analyzer metadata and terminal iterator-error checks

**Scope:**

1. Resolve the target module's Go version.
2. Introduce a minimal `RuleDescriptor` registry.
3. Add report-only `sql-rows-err` and `scanner-err` checks with type-aware tests.
4. Include `since_go`, `source`, and `fix_safety` in JSON rule metadata without breaking existing finding fields.
5. Add self-audit and external-fixture tests for true positives, handled errors, aliasing, helper methods, early returns, generated files, and suppressions.

This creates the foundation needed for broader Staticcheck and modernizer integration without immediately expanding scope to all 54 guideline entries.
