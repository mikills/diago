package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestLoopFlowPreservesASTAndLaunchIdentity(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "flow.go", loopTestPrelude+`func work(){
		var i int
		for i=0;i<3;i++ {
			f:=func(){use(i)}
			if i==0 {goto first}
			goto second
		first: f=func(){}
		second: go f()
		}
	}`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	body := file.Decls[len(file.Decls)-1].(*ast.FuncDecl).Body
	x := &loopIndex{
		ctx: loopContext{fset: fset, goVersion: "go1.22"}, fresh: true,
		ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
		parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	x.node(body)
	var findings []ASTFinding
	s := &loopScanner{index: x, rootNode: body, env: loopEnv{findings: &findings}}
	s.walk(body, &loopWalk{values: map[*loopVariable]loopValue{}})
	if len(s.launches) != 1 {
		t.Fatalf("got %d launches", len(s.launches))
	}
	before := s.launches[0]
	ids := len(x.ids)
	for pass := 0; pass < 2; pass++ {
		s.refineLaunchBindings(body)
		if len(s.launches) != 1 || s.launches[0].stmt != before.stmt || s.launches[0].owner != before.owner ||
			len(s.launches[0].loops) != 1 || s.launches[0].loops[0] != before.loops[0] || len(x.ids) != ids {
			t.Fatal("refinement changed launch or index identity")
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj != nil {
			t.Fatalf("refinement mutated identifier %s", id.Name)
		}
		return true
	})
	s.report()
	if counts := countRules(findings); counts["goroutine-in-loop"] != 1 {
		t.Fatalf("got %v, want one hazard", counts)
	}
}

// These exercise scanLoops end to end. Integration requires exactly one call to
// s.refineLaunchBindings(n), between s.walk(n, ...) and s.report().
func TestLoopFlowBindings(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"fallthrough into default", `var i int; for i=0;i<3;i++ { f:=func(){}; switch i {case 0: f=func(){use(i)}; fallthrough; default: go f()} }`, 1},
		{"fallthrough into middle default", `var i int; for i=0;i<3;i++ { f:=func(){}; switch i {case 0: f=func(){use(i)}; fallthrough; default: go f(); case 2: use(i)} }`, 1},
		{"fallthrough chain", `var i int; for i=0;i<3;i++ { f:=func(){}; switch i {case 0: f=func(){use(i)}; fallthrough; case 1: fallthrough; default: go f()} }`, 1},
		{"fallthrough safe overwrite", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; switch i {case 0: fallthrough; default: f=func(){}; go f()} }`, 0},
		{"fallthrough through default", `var i int; for i=0;i<3;i++ { f:=func(){}; switch i {default: f=func(){use(i)}; fallthrough; case 0: go f()} }`, 1},
		{"forward goto skips overwrite", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; if i%2==0 {goto launch}; f=func(){}; launch: go f() }`, 1},
		{"forward goto reaches overwrite", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; if i%2==0 {goto launch}; use(i); launch: f=func(){}; go f() }`, 0},
		{"forward goto skips capture", `var i int; for i=0;i<3;i++ { f:=func(){}; goto launch; f=func(){use(i)}; launch: go f() }`, 0},
		{"multiple lexical labels", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; if i==0 {goto first}; goto second; first: f=func(){}; second: go f() }`, 1},
		{"backward goto carries capture", `var i int; for i=0;i<3;i++ { f:=func(){}; launch: go f(); if i==0 {f=func(){use(i)}; goto launch} }`, 1},
		{"backward goto strong overwrite", `var i int; for i=0;i<3;i++ { f:=func(){}; launch: f=func(){}; go f(); if i==0 {f=func(){use(i)}; goto launch} }`, 0},
		{"backward alias chain", `var i int; for i=0;i<3;i++ { f:=func(){}; g:=f; launch: go g(); g=f; f=func(){use(i)}; if i==0 {goto launch} }`, 1},
		{"return removes capturing branch", `var i int; for i=0;i<3;i++ { f:=func(){}; if i==0 {f=func(){use(i)}; return}; go f() }`, 0},
		{"return removes safe branch", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; if i==0 {f=func(){}; return}; go f() }`, 1},
		{"unreachable launch after return", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; return; go f() }`, 0},
		{"break removes capturing branch", `var i int; for i=0;i<3;i++ { f:=func(){}; if i==0 {f=func(){use(i)}; break}; go f() }`, 0},
		{"continue removes capturing branch", `var i int; for i=0;i<3;i++ { f:=func(){}; if i==0 {f=func(){use(i)}; continue}; go f() }`, 0},
		{"continue carries outer binding", `var i int; f:=func(){}; for i=0;i<3;i++ { if i==0 {f=func(){use(i)}; continue}; go f() }`, 1},
		{"break prevents back edge capture", `var i int; f:=func(){}; for i=0;i<3;i++ { go f(); f=func(){use(i)}; break }`, 0},
		{"return prevents back edge capture", `var i int; f:=func(){}; for i=0;i<3;i++ { go f(); f=func(){use(i)}; return }`, 0},
		{"continue skips safe overwrite", `var i int; f:=func(){}; for i=0;i<3;i++ { go f(); f=func(){use(i)}; if i==0 {continue}; f=func(){} }`, 1},
		{"loop carried capture", `var i int; f:=func(){}; for i=0;i<3;i++ { go f(); f=func(){use(i)} }`, 1},
		{"loop carried alias", `var i int; f:=func(){}; g:=f; for i=0;i<3;i++ {go g(); g=f; f=func(){use(i)}}`, 1},
		{"loop carried safe overwrite", `var i int; f:=func(){}; for i=0;i<3;i++ { f=func(){}; go f(); f=func(){use(i)} }`, 0},
		{"parallel assignment snapshots", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; g:=func(){}; f,g=g,f; go g(); _=f }`, 1},
		{"parallel assignment safe target", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; g:=func(){}; f,g=g,f; go f(); _=g }`, 0},
		{"branch current binding", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; g:=func(){f()}; if i==0 {f=func(){}} else {f=func(){}}; go g()}`, 0},
		{"goto current binding", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; g:=func(){f()}; if i==0 {goto launch}; f=func(){}; launch: go g()}`, 1},
		{"all switch paths overwrite", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; switch i {case 0: f=func(){}; default: f=func(){}}; go f()}`, 0},
		{"type switch join", `var i int; for i=0;i<3;i++ {f:=func(){}; switch any(i).(type) {case int: f=func(){use(i)}; default: f=func(){}}; go f()}`, 1},
		{"select default preserves binding", `var i int; ch:=make(chan func()); for i=0;i<3;i++ {f:=func(){use(i)}; select {case f=<-ch: default: go f()}; _=f}`, 1},
		{"select chosen receive overwrites", `var i int; ch:=make(chan func()); for i=0;i<3;i++ {f:=func(){use(i)}; select {case f=<-ch: go f(); default: use(f)}}`, 0},
		{"select all paths overwrite", `var i int; ch:=make(chan int); for i=0;i<3;i++ {f:=func(){use(i)}; select {case <-ch: f=func(){}; default: f=func(){}}; go f()}`, 0},
		{"range resets each iteration", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; for _,f=range []func(){func(){}} {go f(); f=func(){use(i)}}}`, 0},
		{"range labeled continue resets binding", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; again: for _,f=range []func(){func(){}} {go f(); f=func(){use(i)}; continue again}}`, 0},
		{"labeled break carries binding", `var i int; for i=0;i<3;i++ {f:=func(){}; inner: for {f=func(){use(i)}; break inner}; go f()}`, 1},
		{"labeled continue skips overwrite", `var i int; f:=func(){}; outer: for i=0;i<3;i++ {go f(); for {f=func(){use(i)}; continue outer}; f=func(){}}`, 1},
		{"synchronous evaluation stays synchronous", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; if i==0 {goto launch}; f=func(){}; launch: go use(eval(f))}`, 0},
		{"synchronous literal argument stays synchronous", `var i int; for i=0;i<3;i++ {go use(eval(func(){use(i)}))}`, 0},
		{"escaping closure argument", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; if i==0 {goto launch}; f=func(){}; launch: go use(f)}`, 1},
		{"nested literal has independent labels", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; if i==0 {goto launch}; f=func(){}; launch: go func(){if i==1 {goto launch}; return; launch: go f()}()}`, 2},
		{"unreachable nested literal", `var i int; for i=0;i<3;i++ {return; go func(){go func(){use(i)}()}()}`, 0},
		{"defer not duplicated", `var i int; for i=0;i<3;i++ {defer use(); f:=func(){use(i)}; go f()}`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					findings := analyzeLoopRegression(t, loopTestPrelude+"func work(){"+tc.body+"}", "go1.22", mode)
					counts := countRules(findings)
					if counts["goroutine-in-loop"] != tc.want {
						t.Fatalf("got %v, want %d goroutine findings: %+v", counts, tc.want, findings)
					}
					wantDefer := 0
					if tc.name == "defer not duplicated" {
						wantDefer = 1
					}
					if counts["defer-in-loop"] != wantDefer {
						t.Fatalf("got %d defer findings, want %d", counts["defer-in-loop"], wantDefer)
					}
				})
			}
		})
	}
}
