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
