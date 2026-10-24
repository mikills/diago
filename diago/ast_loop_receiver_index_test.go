package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestLoopReceiverConstructionIndex(t *testing.T) {
	for _, tc := range []struct {
		name, declarations, body string
		want                     int
	}{
		{"value method comparison copies receiver", `type valueItem struct{N int}; func (valueItem) Use(){}`, `var s valueItem; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 1},
		{"short declaration pointer method comparison", ``, `s:=item{}; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 0},
		{"generic pointer method comparison", `type generic[T any] struct{N T}; func (*generic[T]) Use(){}`, `var s generic[int]; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 0},
		{"generic value method comparison", `type generic[T any] struct{N T}; func (generic[T]) Use(){}`, `var s generic[int]; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 1},
		{"generic promoted pointer comparison", `type generic[T any] struct{N T}; func (*generic[T]) Use(){}; type wrapper struct{generic[int]}`, `var s wrapper; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 0},
		{"embedded pointer comparison reads wrapper", `type wrapper struct{*item}`, `s:=wrapper{&item{}}; for range xs {go func(){_=s.Use==nil}(); s=wrapper{&item{}}}`, 1},
		{"named field callback invocation", `type callbacks struct{F func()}`, `v:=0; s:=callbacks{func(){v++}}; p:=&s; for range xs {go func(){p.F()}()}`, 1},
		{"value receiver callback stays opaque", `type callbacks struct{F func()}; func (callbacks) Touch(){}`, `v:=0; s:=callbacks{func(){v++}}; for range xs {go func(){s.Touch()}()}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := loopTestPrelude + tc.declarations + "\nfunc work(xs []int){" + tc.body + "}"
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, source, "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopReceiverIndexBindingModes(t *testing.T) {
	for _, flags := range []parser.Mode{0, parser.SkipObjectResolution} {
		file, err := parser.ParseFile(token.NewFileSet(), "bindings.go", loopTestPrelude+`
 type valueItem struct{N int}; func (valueItem) Use(){}
 func work(p *item, s item, v valueItem){_=p.Use; _=s.Use; _=v.Use}
 `, flags)
		if err != nil {
			t.Fatal(err)
		}
		fn := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
		x := &loopIndex{
			ctx: loopContext{file: file},
			ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
			parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		}
		x.pushScope()
		x.fields(fn.Type.Params)
		x.node(fn.Body)
		want := map[string]loopAccess{"p": loopHeaderRead, "s": loopRetain, "v": loopRead}
		seen := map[string]bool{}
		for _, access := range x.direct {
			name := access.variable.ident.Name
			if access.mode != want[name] {
				t.Errorf("flags %v: %s binding mode = %v, want %v", flags, name, access.mode, want[name])
			}
			seen[name] = true
		}
		if len(seen) != len(want) {
			t.Fatalf("missing receiver bindings: %v", seen)
		}
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if x.receiverSyntax == nil || probe.receiverDeclarations() != x.receiverSyntax {
			t.Fatal("selector probes did not share their receiver declaration cache")
		}
	}
}

func TestLoopReceiverIndexPackageSelectorIsNotInvocationRoot(t *testing.T) {
	// Even incomplete type information must not mark an imported package as
	// a callable variable. Imported selectors have no Selection entry.
	for _, mode := range []string{"nil", "partial", "lexical"} {
		t.Run(mode, func(t *testing.T) {
			fset := token.NewFileSet()
			flags := parser.Mode(0)
			if mode == "lexical" {
				flags = parser.SkipObjectResolution
			}
			file, err := parser.ParseFile(fset, "package.go", `package p; import "pkg"; func work(){pkg.F()}`, flags)
			if err != nil {
				t.Fatal(err)
			}
			var info *types.Info
			if mode == "partial" {
				info = &types.Info{}
			}
			x := &loopIndex{
				ctx: loopContext{file: file, types: info},
				ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
				parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
			}
			x.pushScope()
			x.node(file.Decls[1].(*ast.FuncDecl).Body)
			for _, access := range x.direct {
				if access.mode&(loopInvoke|loopFieldInvoke) != 0 {
					t.Fatalf("package selector was indexed as a variable invocation: %+v", access)
				}
			}
		})
	}
}
