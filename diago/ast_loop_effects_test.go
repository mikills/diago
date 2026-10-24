package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"testing"
)

// Integration tests: the parent wires the recorder into the converged replay,
// not into transfer or the lexical walk. Keep these on the public scan path.
func TestLoopFlowEffects(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"fallthrough synchronous writer", `for _,n:=range xs {v:=0; f:=func(){}; switch n {case 0: f=func(){v++}; fallthrough; default: go func(){_=v}(); f()}}`, 1},
		{"goto skips safe overwrite", `for range xs {v:=0; f:=func(){v++}; goto call; f=func(){}; call: go func(){_=v}(); f()}`, 1},
		{"goto skips writer binding", `for range xs {v:=0; f:=func(){}; goto call; f=func(){v++}; call: go func(){_=v}(); f()}`, 0},
		{"goto reaches safe overwrite", `for _,n:=range xs {v:=0; f:=func(){v++}; if n==0 {goto call}; f=func(){}; call: f=func(){}; go func(){_=v}(); f()}`, 0},
		{"return removes writer binding", `for _,n:=range xs {v:=0; f:=func(){}; if n==0 {f=func(){v++}; return}; go func(){_=v}(); f()}`, 0},
		{"break removes writer binding", `for _,n:=range xs {v:=0; f:=func(){}; if n==0 {f=func(){v++}; break}; go func(){_=v}(); f()}`, 0},
		{"continue removes writer binding", `for _,n:=range xs {v:=0; f:=func(){}; if n==0 {f=func(){v++}; continue}; go func(){_=v}(); f()}`, 0},
		{"unreachable call after return", `v:=0; for range xs {go func(){_=v}(); return; (func(){v++})()}`, 0},
		{"unreachable call after break", `v:=0; for range xs {go func(){_=v}(); break; (func(){v++})()}`, 0},
		{"unreachable call after continue", `v:=0; for range xs {go func(){_=v}(); continue; (func(){v++})()}`, 0},
		{"unreachable call after goto", `v:=0; for range xs {go func(){_=v}(); goto done; (func(){v++})(); done: use()}`, 0},
		{"shared capture safe replacement", `v:=0; for range xs {f:=func(){}; goto call; f=func(){v++}; call: go func(){_=v}(); f()}`, 0},
		{"shared capture writer before launch", `v:=0; for range xs {f:=func(){v++}; goto call; f=func(){}; call: f(); go func(){_=v}()}`, 1},
		{"fresh writer before launch", `for range xs {v:=0; f:=func(){v++}; goto call; f=func(){}; call: f(); go func(){_=v}()}`, 0},
		{"current callback binding", `for _,n:=range xs {v:=0; f:=func(){}; g:=func(){f()}; switch n {case 0: f=func(){v++}; fallthrough; default: go func(){_=v}(); g()}}`, 1},
		{"current callback safe overwrite", `for range xs {v:=0; f:=func(){v++}; g:=func(){f()}; f=func(){}; go func(){_=v}(); g()}`, 0},
		{"recursive callback expansion terminates", `for range xs {v:=0; var f func(); f=func(){if v<0 {f()}; v++}; go func(){_=v}(); f()}`, 1},
		{"outer go call is not synchronous", `for range xs {v:=0; f:=func(){v++}; go f()}`, 0},
		// Not synchronous at registration, but the exit-time writer races with the worker.
		{"outer defer call is not synchronous", `for range xs {v:=0; f:=func(){v++}; go func(){_=v}(); defer f()}`, 1},
		{"nested go argument call is synchronous", `for _,n:=range xs {v:=0; f:=func()int{return 0}; switch n {case 0: f=func()int{v++;return 0}; fallthrough; default: go func(){_=v}(); go use(f())}}`, 1},
		{"nested defer argument call is synchronous", `for _,n:=range xs {v:=0; f:=func()int{return 0}; switch n {case 0: f=func()int{v++;return 0}; fallthrough; default: go func(){_=v}(); defer use(f())}}`, 1},
		{"nested go function evaluation is synchronous", `for range xs {v:=0; f:=func()func(){v++;return func(){}}; go func(){_=v}(); go f()()}`, 1},
		{"nested defer function evaluation is synchronous", `for range xs {v:=0; f:=func()func(){v++;return func(){}}; go func(){_=v}(); defer f()()}`, 1},
		{"synchronous read conflicts with asynchronous writer", `for range xs {v:=0; f:=func(){}; goto call; f=func(){v++}; call: f=func(){_=v}; go func(){v++}(); f()}`, 1},
		{"uninvoked literal body separate frame", `v:=0; for range xs {go func(){_=v}(); f:=func(){v++}; _=f}`, 0},
		{"nested frame replay", `for range xs {(func(){v:=0; f:=func(){v++}; goto call; f=func(){}; call: go func(){_=v}(); f()})()}`, 1},
		{"nested frame effects not parent effects", `for range xs {v:=0; go func(){v++}()}`, 0},
		{"pointer fallthrough increment", `for _,n:=range xs {v,u:=0,0; p:=&u; switch n {case 0: p=&v; fallthrough; default: go func(){_=v}(); (*p)++}}`, 1},
		{"pointer fallthrough assignment", `for _,n:=range xs {v,u:=0,0; p:=&u; switch n {case 0: p=&v; fallthrough; default: go func(){_=v}(); *p=1}}`, 1},
		{"pointer goto skips overwrite", `for range xs {v,u:=0,0; p:=&v; goto store; p=&u; store: go func(){_=v}(); *p=1}`, 1},
		{"pointer goto skips alias", `for range xs {v,u:=0,0; p:=&u; goto store; p=&v; store: go func(){_=v}(); *p=1}`, 0},
		{"pointer return removes alias", `for _,n:=range xs {v,u:=0,0; p:=&u; if n==0 {p=&v; return}; go func(){_=v}(); *p=1}`, 0},
		{"pointer safe strong overwrite", `for range xs {v,u:=0,0; p:=&v; p=&u; go func(){_=v}(); *p=1}`, 0},
		{"parallel store uses old pointer", `for range xs {v,u:=0,0; p:=&v; go func(){_=v}(); p,*p=&u,1; _=p}`, 1},
		{"parallel store does not use new pointer", `for range xs {v,u:=0,0; p:=&u; go func(){_=v}(); p,*p=&v,1; _=p}`, 0},
		{"unreachable indirect store", `v:=0; for range xs {p:=&v; go func(){_=v}(); break; *p=1}`, 0},
		{"pointer field store", `for range xs {v,u:=item{},item{}; p:=&v; goto store; p=&u; store: go func(){_=v.N}(); p.N++}`, 1},
		{"pointer array store", `for range xs {v,u:=[1]int{},[1]int{}; p:=&v; goto store; p=&u; store: go func(){_=v[0]}(); p[0]=1}`, 1},
		{"closure indirect writer current pointer", `for _,n:=range xs {v,u:=0,0; p:=&u; f:=func(){(*p)++}; switch n {case 0: p=&v; fallthrough; default: go func(){_=v}(); f()}}`, 1},
		{"range definitions retain implicit writes", `for _,v:=range xs {go func(){_=v}()}`, 0},
		{"reused range retains writes", `var v int; for _,v=range xs {go func(){_=v}()}`, 1},
		{"synthetic range assignment through pointer", `v:=0; p:=&v; for _,*p=range xs {go func(){_=v}()}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "go1.22", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

// Exercise the recorder independently of the parent's CFG wiring. An explicit
// converged state differs from the lexical state, including a current callback
// binding and a pointer destination on a parallel assignment.
func TestLoopFlowEffectsRecorder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "effects.go", `package sample
func work(xs []int) { for range xs {
	v,u:=0,0
	f:=func(){}
	g:=func(){f()}
	writer:=func(){v++}
	p:=&u
	go func(){_=v}()
	g()
	p,*p=&u,1
	} }`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	body := file.Decls[0].(*ast.FuncDecl).Body
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
	vars := map[string]*loopVariable{}
	var call *ast.CallExpr
	var store *ast.AssignStmt
	var writer *ast.FuncLit
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && x.ids[id] != nil {
			vars[id.Name] = x.ids[id]
		}
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "g" {
				call = c
			}
		}
		if a, ok := n.(*ast.AssignStmt); ok {
			if len(a.Rhs) == 1 {
				if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name == "writer" {
					writer = a.Rhs[0].(*ast.FuncLit)
				}
			}
			if len(a.Lhs) == 2 {
				if _, ok := a.Lhs[1].(*ast.StarExpr); ok {
					store = a
				}
			}
		}
		return true
	})
	if call == nil || store == nil || writer == nil {
		t.Fatal("missing test nodes")
	}
	state := &loopWalk{values: map[*loopVariable]loopValue{
		vars["f"]: x.captures[writer],
		vars["g"]: {vars["f"]: loopRead | loopInvoke},
		vars["p"]: {vars["v"]: loopRead},
	}}
	pAlias := &loopVariable{}
	state.values[pAlias] = loopValue{vars["v"]: loopRead}
	before := loopCloneValues(state.values)
	ids := len(x.ids)
	for pass := 0; pass < 2; pass++ {
		effects := s.beginFlowEffects()
		effects.aliases = map[*loopVariable]*loopVariable{vars["p"]: pAlias}
		effects.recordFlowEffects(call, state)
		effects.recordFlowEffects(store, state)
		// Duplicated CFG expression nodes must not multiply synthetic accesses.
		effects.recordFlowEffects(call, state)
		effects.finishFlowEffects()
		var calls, stores int
		for _, access := range x.direct {
			if access.effect && access.variable == vars["v"] && access.mode&loopWrite != 0 {
				switch access.node {
				case call:
					calls++
				case store.Lhs[1]:
					stores++
				}
				if access.owner != nil {
					t.Fatal("replay changed the function owner")
				}
			}
		}
		if calls != 1 || stores != 1 {
			t.Fatalf("got %d call writes and %d pointer writes, want one each", calls, stores)
		}
		for _, region := range x.regions {
			if !region.writes[vars["v"]] || region.writes[vars["u"]] {
				t.Fatal("region writes retained lexical pointer effects or missed refined writes")
			}
		}
		if !reflect.DeepEqual(state.values, before) || len(x.ids) != ids {
			t.Fatal("effect replay mutated bindings or identities")
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj != nil {
			t.Fatal("effect replay mutated AST object identities")
		}
		return true
	})
}

func TestLoopFlowEffectsEmptyReplayRemovesLexicalEffects(t *testing.T) {
	v := &loopVariable{}
	body := &ast.BlockStmt{}
	region := &loopRegion{node: body, writes: map[*loopVariable]bool{v: true}}
	id := &ast.Ident{Name: "v"}
	call := &ast.CallExpr{Fun: &ast.Ident{Name: "f"}}
	actual := loopDirectAccess{node: id, variable: v, mode: loopRead}
	x := &loopIndex{
		regions: map[ast.Node]*loopRegion{body: region},
		direct:  []loopDirectAccess{actual, {node: call, variable: v, mode: loopWrite, effect: true}},
	}
	s := &loopScanner{index: x}
	effects := s.beginFlowEffects()
	effects.finishFlowEffects()
	if len(x.direct) != 1 || x.direct[0] != actual || len(region.writes) != 0 {
		t.Fatalf("lexical synthetic effects survived empty reachable replay: %+v, %+v", x.direct, region.writes)
	}
}
