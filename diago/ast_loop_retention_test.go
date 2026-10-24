package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestRetainedTypeDAG(t *testing.T) {
	var typ types.Type = types.NewStruct(nil, nil)
	for i := 0; i < 40; i++ {
		typ = types.NewStruct([]*types.Var{types.NewField(token.NoPos, nil, "A", typ, false), types.NewField(token.NoPos, nil, "B", typ, false)}, nil)
	}
	memo := map[types.Type]bool{}
	if loopRetainedTypeMemo(typ, memo) {
		t.Fatal("reference-free DAG reported retention")
	}
	if len(memo) != 41 {
		t.Fatalf("visited %d types, want 41 unique nodes", len(memo))
	}
}

func TestRetainedSyntaxDAG(t *testing.T) {
	var source strings.Builder
	source.WriteString("package p\ntype T0 struct{}\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&source, "type T%d struct{A,B T%d}\n", i, i-1)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", source.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	typ := file.Decls[len(file.Decls)-1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type
	scanner := &loopScanner{index: &loopIndex{ids: map[*ast.Ident]*loopVariable{}}}
	if scanner.retainedSyntax(typ, map[ast.Expr]bool{}) {
		t.Fatal("reference-free syntax DAG reported retention")
	}
	if len(scanner.syntaxRetention) != 31 {
		t.Fatalf("visited %d syntax types, want 31", len(scanner.syntaxRetention))
	}
}
