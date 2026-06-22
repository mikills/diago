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

func TestLongTestNameFinding(t *testing.T) {
	t.Run("flags long test names", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestParseEscapeOutputValidOutputWithMultipleHeapEscapes(t *testing.T) {}
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Rule != "long-test-name" || findings[0].Severity != "low" {
			t.Fatalf("unexpected finding: %#v", findings[0])
		}
	})

	t.Run("ignores short test names", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestParseEscapeOutput(t *testing.T) {}
`)
		if len(findings) != 0 {
			t.Fatalf("got %d findings, want 0", len(findings))
		}
	})

	t.Run("matches the requested rg threshold", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestABCDEFGHIJKLMNOPQRSTUVWXY1234567(t *testing.T) {}
func TestABCDEFGHIJKLMNOPQRSTUVWXY123456(t *testing.T) {}
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Symbol != "TestABCDEFGHIJKLMNOPQRSTUVWXY1234567" {
			t.Fatalf("symbol = %q", findings[0].Symbol)
		}
	})
}

func longTestNameFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample_test.go", isTest: true}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		analyzeFunc(&findings, ctx, fn)
	}
	return findings
}

func resourceCloseFindings(t *testing.T, source string) []ASTFinding {
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
		findResourceCloseSignals(&findings, ctx, fn, fn.Name.Name)
	}
	return findings
}

func TestResourceCloseSignals(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   int
	}{
		{
			name: "defer close direct",
			source: `package sample
import "os"
func read(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}`,
			want: 0,
		},
		{
			name: "returned ownership is not a leak",
			source: `package sample
import "os"
func openThing(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}`,
			want: 0,
		},
		{
			name: "returned wrapped ownership is not a leak",
			source: `package sample
import "os"
type holder struct{ f *os.File }
func openWrapped(p string) (*holder, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &holder{f: f}, nil
}`,
			want: 0,
		},
		{
			name: "http body close resolves the response",
			source: `package sample
import ("io"; "net/http")
func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}`,
			want: 0,
		},
		{
			name: "genuine leak still reported",
			source: `package sample
import "os"
func leak(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	_ = f
	return nil
}`,
			want: 1,
		},
		{
			name: "one returned one leaked",
			source: `package sample
import "os"
func mixed(p, q string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	g, err := os.Open(q)
	if err != nil {
		return nil, err
	}
	_ = g
	return f, nil
}`,
			want: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := resourceCloseFindings(t, tc.source)
			if len(findings) != tc.want {
				t.Fatalf("got %d resource-not-closed findings, want %d: %+v", len(findings), tc.want, findings)
			}
		})
	}
}
