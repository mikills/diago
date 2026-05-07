package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestIgnoredCallSeverity(t *testing.T) {
	t.Run("cleanup calls are low severity", func(t *testing.T) {
		findings := ignoredCallFindings(t, `package sample

import "os"

func cleanup(f *os.File, timer interface{ Stop() bool }) {
	_ = f.Close()
	_ = os.Remove("tmp")
	_ = timer.Stop()
}
`)
		if len(findings) != 3 {
			t.Fatalf("got %d findings, want 3", len(findings))
		}
		for _, finding := range findings {
			if finding.Severity != "low" {
				t.Fatalf("%s severity = %s, want low", finding.Message, finding.Severity)
			}
		}
	})

	t.Run("non cleanup calls stay medium severity", func(t *testing.T) {
		findings := ignoredCallFindings(t, `package sample

func save() error { return nil }
func run() { _ = save() }
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Severity != "medium" {
			t.Fatalf("severity = %s, want medium", findings[0].Severity)
		}
	})
}

func ignoredCallFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample.go"}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findErrorHandlingSignals(&findings, ctx, fn, fn.Name.Name)
	}
	return findings
}

func TestCollectLiteralSignals(t *testing.T) {
	t.Run("skips imports and struct tags", func(t *testing.T) {
		file := parseLiteralTestFile(t, `package sample

import (
	"context"
	"errors"
	"log/slog"
	"github.com/mikills/minnow/kb"
)

type record struct {
	ID string `+"`json:\"kb_id\"`"+`
}

const a = "real duplicate"
const b = "real duplicate"
const c = "real duplicate"
const d = "real duplicate"
const e = "real duplicate"
const f = "real duplicate"
`)
		signals := newPackageSignals(goListPackage{})
		collectLiteralSignals(signals, astContext{fset: token.NewFileSet(), path: "sample.go"}, file)

		for _, literal := range []string{"context", "errors", "log/slog", "github.com/mikills/minnow/kb", `json:"kb_id"`} {
			if got := len(signals.strings[literal]); got != 0 {
				t.Fatalf("literal %q tracked %d times, want 0", literal, got)
			}
		}
		if got := len(signals.strings["real duplicate"]); got != 6 {
			t.Fatalf("real duplicate tracked %d times, want 6", got)
		}
	})
}

func parseLiteralTestFile(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
