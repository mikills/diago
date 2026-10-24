package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
)

// Run discovery and the stable binding/effects engine, then feed identical CFG
// adapters to the conflict recorder. This tests the new API before its parent
// wires the hooks into refine/report. All fixtures are type-checked, even in
// nil/partial/SkipObjectResolution analysis modes: gotos must be legal Go.
func loopConflictTestScanner(t testing.TB, body, version, mode string) (*loopScanner, *loopConflictHazards) {
	t.Helper()
	fset := token.NewFileSet()
	flags := parser.Mode(0)
	if strings.Contains(mode, "lexical") {
		flags = parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(fset, "conflicts.go", loopTestPrelude+"func work(xs []int,b bool){"+body+"}", flags)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{GoVersion: "go" + strings.TrimPrefix(version, "go")}
	if _, err := conf.Check("example.com/conflicts", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	switch {
	case strings.HasPrefix(mode, "partial"):
		info.Defs, info.Selections = nil, nil
		for id := range info.Uses {
			if id.Name != "use" {
				delete(info.Uses, id)
			}
		}
	case mode != "typed":
		info = nil
	}
	fn := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	x := &loopIndex{
		ctx:   loopContext{fset: fset, types: info, file: file, decl: fn, goVersion: version},
		fresh: freshLoopVariables(version),
		ids:   map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
		parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	x.fields(fn.Type.Params)
	for _, stmt := range fn.Body.List {
		x.node(stmt)
	}
	s := &loopScanner{index: x, rootNode: fn.Body, env: loopEnv{ctx: x.ctx}}
	s.discover(fn.Body, &loopWalk{})
	recorder := s.beginFlowConflicts()
	s.refineLaunchBindings(fn.Body)
	record := func(body *ast.BlockStmt) {
		a := &loopFlowAST{
			labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{},
			steps: map[ast.Node]*loopFlowRange{}, index: x, copySteps: map[ast.Node]*loopRegion{},
		}
		recorder.recordGraph(cfg.New(a.block(body), func(*ast.CallExpr) bool { return true }), a)
	}
	record(fn.Body)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok && x.reachable[lit] {
			record(lit.Body)
		}
		return true
	})
	return s, recorder.finishFlowConflicts()
}

func TestLoopCFGConflictReachability(t *testing.T) {
	for _, tc := range []struct {
		name, body, direct, siblings, copies string
	}{
		{"backward direct write", `for range xs {v:=0; again: v++; go func(){_=v}(); if v<3 {goto again}}`, "1", "0", "0"},
		{"launch on first pass only", `for range xs {v:=0; again: v++; if v==1 {go func(){_=v}(); goto again}}`, "1", "0", "0"},
		{"backward direct read", `for range xs {v:=0; again: use(v); go func(){v++}(); if b {goto again}}`, "1", "1", "0"},
		{"same writer site cycles", `for range xs {v:=0; again: go func(){v++}(); if b {goto again}}`, "0", "1", "0"},
		{"same reader site cycles", `for range xs {v:=0; again: go func(){_=v}(); if b {goto again}}`, "0", "0", "0"},
		{"fresh declaration stops backward edge", `for range xs {again: v:=0; go func(){v++}(); if b {goto again}}`, "0", "0", "0"},
		{"fresh var declaration stops backward edge", `for range xs {again: var v int; go func(){v++}(); if b {goto again}}`, "0", "0", "0"},
		{"fresh range stops cycle", `for _,v:=range xs {go func(){v++}()}`, "0", "0", "0"},
		{"inner loop does not replace outer storage", `for _,v:=range xs {for range xs {go func(){v++}()}}`, "0", "1", "0"},
		{"same-scope redeclaration is a write", `for range xs {v:=0; again: go func(){_=v}(); v,w:=1,2; _=w; if b {goto again}; _=v}`, "1", "0", "0"},
		{"opposing direct arms", `for _,v:=range xs {if b {go func(){_=v}()} else {v++}}`, "0", "0", "0"},
		{"shared read exits before mutation", `v:=0; for _,n:=range xs {if n==0 {go func(){_=v}(); break}; v++}`, "0", "0", "0"},
		{"shared writer exits before parent read", `v:=0; for _,n:=range xs {if n==0 {go func(){v++}(); break}; use(v)}`, "0", "0", "0"},
		{"opposing sibling arms", `for _,v:=range xs {if b {go func(){v++}()} else {go func(){_=v}()}}`, "00", "00", "00"},
		{"back edge joins opposing arms", `for range xs {v:=0; again: if b {go func(){v++}()} else {go func(){_=v}()}; if b {goto again}}`, "00", "11", "00"},
		{"goto skips lexical successor", `for _,v:=range xs {go func(){_=v}(); goto done; v++; done: use()}`, "0", "0", "0"},
		{"return skips lexical sibling", `for _,v:=range xs {go func(){v++}(); return; go func(){_=v}()}`, "00", "00", "00"},
		{"fallthrough reaches another arm's write", `for _,v:=range xs {switch {case b: go func(){_=v}(); fallthrough; default: v++}}`, "1", "0", "0"},
		{"copy after normal launch", `for i:=0;i<3;i++ {go func(){i++}()}`, "0", "0", "1"},
		{"conditional break cannot reach copy", `for i:=0;i<3;i++ {if b {go func(){i++}(); break}}`, "0", "0", "0"},
		{"conditional return cannot reach copy", `for i:=0;i<3;i++ {if b {go func(){i++}(); return}}`, "0", "0", "0"},
		{"conditional continue reaches copy", `for i:=0;i<3;i++ {if b {go func(){i++}(); continue}}`, "0", "0", "1"},
		{"only one launch reaches copy", `for i:=0;i<3;i++ {if b {go func(){i++}(); break}; go func(){i++}()}`, "00", "00", "01"},
		{"copy cuts off post write", `for i:=0;i<3;i++ {go func(){_=i}()}`, "0", "0", "0"},
		{"goto before copy shares old slot", `for i:=0;i<3;i++ {again: go func(){i++}(); if b {goto again}}`, "0", "1", "1"},
		{"indirect direct write", `for range xs {s:=[]int{0}; go func(){s[0]++}(); s[0]++}`, "1", "0", "0"},
		{"indirect direct reader", `for range xs {s:=[]int{0}; go func(){s[0]++}(); use(s[0])}`, "1", "0", "0"},
		{"indirect parent writer", `for range xs {s:=[]int{0}; go func(){use(s[0])}(); s[0]++}`, "1", "0", "0"},
		{"indirect siblings", `for range xs {s:=[]int{0}; go func(){s[0]++}(); go func(){use(s[0])}()}`, "00", "11", "00"},
		{"indirect siblings opposing arms", `for range xs {s:=[]int{0}; if b {go func(){s[0]++}()} else {go func(){use(s[0])}()}}`, "00", "00", "00"},
		{"fresh pointer copy is not pointee mutation", `for p,n:=new(item),0;n<3;p,n=new(item),n+1 {go func(){p.N++}()}`, "0", "0", "0"},
		{"pointer slot writer conflicts with copy", `for p,n:=new(item),0;n<3;p,n=new(item),n+1 {go func(){p=new(item); _=p}()}`, "0", "0", "1"},
		{"nested function frames", `for _,v:=range xs {go func(){go func(){v++}()}()}`, "00", "00", "00"},
		{"nested siblings share invocation", `for _,v:=range xs {go func(){go func(){v++}(); go func(){_=v}()}()}`, "000", "011", "000"},
		{"synchronous operands precede launch", `for _,v:=range xs {go func(int){_=v}(eval(func(){v++}))}`, "0", "0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s, got := loopConflictTestScanner(t, tc.body, "1.22", mode)
					for _, check := range []struct {
						name, want string
						got        map[*ast.GoStmt]bool
					}{{"direct", tc.direct, got.direct}, {"siblings", tc.siblings, got.siblings}, {"copies", tc.copies, got.copies}} {
						var bits strings.Builder
						for _, launch := range s.launches {
							if check.got[launch.stmt] {
								bits.WriteByte('1')
							} else {
								bits.WriteByte('0')
							}
						}
						if bits.String() != check.want {
							t.Errorf("%s: got %s, want %s", check.name, bits.String(), check.want)
						}
					}
				})
			}
		})
	}
}

func TestLoopCFGConflictLegacyRangeStorage(t *testing.T) {
	for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
		s, hazards := loopConflictTestScanner(t, `for _,v:=range xs {go func(){_=v}()}`, "1.21", mode)
		if !hazards.direct[s.launches[0].stmt] {
			t.Errorf("%s: legacy range assignment must remain a reachable write", mode)
		}
	}
}

func TestLoopCFGConflictManyWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("large launch-site scale regression")
	}
	// A reader and 9,999 writers all share one fresh allocation. Every site
	// conflicts, but identifying them must not enumerate 100 million pairs.
	body := `for _,v:=range xs {go func(){_=v}();` + strings.Repeat(`go func(){v++}();`, 9999) + `}`
	s, hazards := loopConflictTestScanner(t, body, "1.22", "lexical")
	if len(s.launches) != 10000 || len(hazards.siblings) != 10000 || len(hazards.direct) != 0 {
		t.Fatalf("got launches=%d siblings=%d direct=%d", len(s.launches), len(hazards.siblings), len(hazards.direct))
	}
}

func BenchmarkLoopCFGConflictWorkers(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			// Time the graph query only, not parsing/type checking/discovery.
			s, _ := loopConflictTestScanner(b, `for _,v:=range xs {`+strings.Repeat(`go func(){v++}();`, n)+`}`, "1.22", "lexical")
			recorder := s.beginFlowConflicts()
			a := &loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}, index: s.index, copySteps: map[ast.Node]*loopRegion{}}
			recorder.recordGraph(cfg.New(a.block(s.rootNode.(*ast.BlockStmt)), func(*ast.CallExpr) bool { return true }), a)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				recorder.finishFlowConflicts()
			}
		})
	}
}
