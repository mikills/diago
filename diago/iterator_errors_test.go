package diago

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestIteratorErrorSignals(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   map[string]int
	}{
		{
			name: "reports unchecked rows and scanner",
			source: `package sample
import ("bufio"; "database/sql"; "strings")
func rows(db *sql.DB) error {
	r, err := db.Query("select 1")
	if err != nil { return err }
	defer r.Close()
	for r.Next() {}
	return nil
}
func scanner() {
	s := bufio.NewScanner(strings.NewReader("x"))
	for s.Scan() {}
}`,
			want: map[string]int{"sql-rows-err": 1, "scanner-err": 1},
		},
		{
			name: "accepts direct terminal checks",
			source: `package sample
import ("bufio"; "database/sql"; "strings")
func rows(db *sql.DB) error {
	r, err := db.Query("select 1")
	if err != nil { return err }
	defer r.Close()
	for r.Next() {}
	if err := r.Err(); err != nil { return err }
	return nil
}
func scanner() error {
	s := bufio.NewScanner(strings.NewReader("x"))
	for s.Scan() {}
	return s.Err()
}`,
			want: map[string]int{},
		},
		{
			name: "accepts a check after an enclosing branch",
			source: `package sample
import "bufio"
func scanner(s *bufio.Scanner, enabled bool) error {
	if enabled {
		for s.Scan() {}
	}
	if err := s.Err(); err != nil { return err }
	return nil
}`,
			want: map[string]int{},
		},
		{
			name: "does not accept a different iterator error",
			source: `package sample
import ("bufio"; "strings")
func scanner() error {
	first := bufio.NewScanner(strings.NewReader("first"))
	second := bufio.NewScanner(strings.NewReader("second"))
	for first.Scan() {}
	return second.Err()
}`,
			want: map[string]int{"scanner-err": 1},
		},
		{
			name: "does not treat an ignored Err result as checked",
			source: `package sample
import ("bufio"; "strings")
func scanner() {
	s := bufio.NewScanner(strings.NewReader("x"))
	for s.Scan() {}
	_ = s.Err()
}`,
			want: map[string]int{"scanner-err": 1},
		},
		{
			name: "supports an adjacent rule-scoped suppression",
			source: `package sample
import ("bufio"; "strings")
func scanner() {
	s := bufio.NewScanner(strings.NewReader("x"))
	//diago:ignore scanner-err input is known to be in-memory
	for s.Scan() {}
}`,
			want: map[string]int{},
		},
		{
			name: "finds loops nested inside another loop",
			source: `package sample
import "bufio"
func scanner(s *bufio.Scanner, repeat bool) error {
	for repeat {
		for s.Scan() {}
		break
	}
	return nil
}`,
			want: map[string]int{"scanner-err": 1},
		},
		{
			name: "does not accept an unreachable terminal check",
			source: `package sample
import "bufio"
func scanner(s *bufio.Scanner) error {
	for s.Scan() {}
	return nil
	if err := s.Err(); err != nil { return err }
	return nil
}`,
			want: map[string]int{"scanner-err": 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := iteratorErrorFindings(t, test.source)
			got := map[string]int{}
			for _, finding := range findings {
				got[finding.Rule]++
			}
			if len(got) != len(test.want) {
				t.Fatalf("findings = %+v, want %v", findings, test.want)
			}
			for rule, count := range test.want {
				if got[rule] != count {
					t.Fatalf("%s count = %d, want %d; findings=%+v", rule, got[rule], count, findings)
				}
			}
		})
	}
}

func iteratorErrorFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
		Types:      make(map[ast.Expr]types.TypeAndValue),
	}
	conf := types.Config{Importer: importer.Default()}
	if _, err := conf.Check("example.com/sample", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample.go", file: file, types: info}
	var findings []ASTFinding
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Body != nil {
			findIteratorErrorSignals(&findings, ctx, fn, fn.Name.Name)
		}
	}
	return findings
}
