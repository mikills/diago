package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
)

func loopLiveTestScanner(t testing.TB, body string, typed bool) (*loopScanner, *ast.BlockStmt) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "live.go", loopTestPrelude+"func work(){"+body+"}", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var info *types.Info
	if typed {
		info = &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
			Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		conf := types.Config{}
		if _, err := conf.Check("live", fset, []*ast.File{file}, info); err != nil {
			t.Fatal(err)
		}
	}
	fn := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	ctx := loopContext{fset: fset, types: info, file: file, decl: fn, goVersion: "go1.22"}
	x := &loopIndex{ctx: ctx, fresh: true,
		ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
		parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{}}
	x.pushScope()
	x.node(fn.Body)
	return &loopScanner{index: x, rootNode: fn.Body, env: loopEnv{ctx: ctx}}, fn.Body
}

// This is the production worklist/replay with the proposed integration points.
// Keep it here so helpers can be tested before the parent wires refine itself.
// Transfers, values, expansions, and the adapter are NOT reimplemented.
func loopLiveTestRun(s *loopScanner, body *ast.BlockStmt, prune bool) (map[*ast.GoStmt]loopValue, int, int) {
	launches := map[*ast.GoStmt]loopValue{}
	maxState, totalState := 0, 0
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
	var refine func(*ast.BlockStmt, map[*loopVariable]loopValue)
	refine = func(body *ast.BlockStmt, seed map[*loopVariable]loopValue) {
		a := loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}}
		ast.Inspect(body, func(n ast.Node) bool {
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			if rng, ok := n.(*ast.RangeStmt); ok {
				f.rangeTargetTypes(rng)
			}
			return true
		})
		graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
		var live []map[*loopVariable]bool
		if prune {
			live = f.loopFlowLiveness(graph, &a, seed)
		}
		snapshot := func(state map[*loopVariable]loopValue, block *cfg.Block) map[*loopVariable]loopValue {
			if prune {
				return loopPruneBindings(state, live[block.Index], f.aliases)
			}
			return loopCloneValues(state)
		}
		inputs := make([]map[*loopVariable]loopValue, len(graph.Blocks))
		inputs[0] = snapshot(seed, graph.Blocks[0])
		queue := []*cfg.Block{graph.Blocks[0]}
		queued := make([]bool, len(inputs))
		queued[0] = true
		for len(queue) != 0 {
			block := queue[0]
			queue = queue[1:]
			queued[block.Index] = false
			w := &loopWalk{values: snapshot(inputs[block.Index], block)}
			for _, node := range block.Nodes {
				f.transfer(node, w, &a)
			}
			for _, next := range block.Succs {
				first := inputs[next.Index] == nil
				if first {
					inputs[next.Index] = map[*loopVariable]loopValue{}
				}
				out := w.values
				if prune {
					out = loopPruneBindings(out, live[next.Index], f.aliases)
				}
				changed := loopJoinBindings(inputs[next.Index], out)
				if (first || changed) && !queued[next.Index] {
					queue = append(queue, next)
					queued[next.Index] = true
				}
			}
		}
		literals := map[*ast.FuncLit]map[*loopVariable]loopValue{}
		for _, block := range graph.Blocks {
			state := inputs[block.Index]
			if state == nil {
				continue
			}
			totalState += len(state)
			if len(state) > maxState {
				maxState = len(state)
			}
			w := &loopWalk{values: snapshot(state, block)}
			for _, node := range block.Nodes {
				if goStmt, ok := node.(*ast.GoStmt); ok {
					value := s.invokeValue(goStmt.Call.Fun, w)
					for _, arg := range goStmt.Call.Args {
						loopMerge(value, s.invokeValue(arg, w))
					}
					launches[goStmt] = s.expand(value, w)
				}
				ast.Inspect(node, func(n ast.Node) bool {
					if lit, ok := n.(*ast.FuncLit); ok {
						if literals[lit] == nil {
							literals[lit] = map[*loopVariable]loopValue{}
						}
						loopJoinBindings(literals[lit], w.values)
						return false
					}
					return true
				})
				f.transfer(node, w, &a)
			}
		}
		for lit, entry := range literals {
			refine(lit.Body, entry)
		}
	}
	refine(body, nil)
	return launches, maxState, totalState
}

func TestLoopFlowLiveLaunchEquivalence(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"closure pointer dependency", `p:=&i; f:=func(){use(p)}; if i==0 {use(i)}; go f()`},
		{"pointer chain", `p:=&i; q:=&p; f:=func(){use(q)}; if i==0 {use(i)}; go f()`},
		{"current capture overwritten", `p:=&i; f:=func(){use(p)}; if i==0 {p=nil} else {p=nil}; go f()`},
		{"different join contributors", `var p *int; f:=func(){}; if i==0 {f=func(){use(p)}} else {p=&i}; go f()`},
		{"copied closure join contributors", `var p *int; f:=func(){}; var g func(); if i==0 {f=func(){use(p)}; g=f} else {p=&i}; go g()`},
		{"transitive copied closure join contributors", `var p *int; f:=func(){}; var h func(); if i==0 {f=func(){use(p)}; g:=f; h=g} else {p=&i}; go h()`},
		{"copy before split closure contributors", `var p *int; f:=func(){use(p)}; g:=f; if i==0 {f=func(){}} else {p=&i}; go g()`},
		{"copied pointer join contributors", `var p *int; var q **int; if i==0 {r:=&p; q=r} else {p=&i}; go use(q)`},
		{"copied closure field join contributors", `var p *int; var a struct{F func()}; if i==0 {f:=func(){use(p)}; g:=f; a.F=g} else {p=&i}; go a.F()`},
		{"copied closure alias store join contributors", `var p *int; var a struct{F func()}; q:=&a; if i==0 {f:=func(){use(p)}; g:=f; q.F=g} else {p=&i}; go a.F()`},
		{"copy cycle join contributors", `var p *int; f:=func(){}; g:=f; if i==0 {f=func(){use(p)}; g=f} else {f=g; p=&i}; go g()`},
		{"inherited copied closure contributors", `var p *int; f:=func(){use(p)}; go func(){g:=f; if i==0 {f=func(){}} else {p=&i}; go g()}()`},
		{"inherited copied address contributors", `var p *int; q:=&p; go func(){r:=q; if i==0 {} else {p=&i}; go use(r)}()`},
		{"range copied closure contributors", `var p *int; fs:=[]func(){func(){use(p)}}; for _,f:=range fs {g:=f; if i==0 {fs=nil} else {p=&i}; go g()}`},
		{"range copied pointer array contributors", `var p *int; a:=[1]func(){func(){use(p)}}; q:=&a; for _,f:=range q {g:=f; if i==0 {a=[1]func(){}} else {p=&i}; go g()}`},
		{"different join pointer contributors", `var p *int; var q **int; if i==0 {q=&p} else {p=&i}; go use(q)`},
		{"different join aggregate contributors", `var p *int; var a struct{F func()}; if i==0 {a.F=func(){use(p)}} else {p=&i}; go a.F()`},
		{"different join alias store contributors", `var p *int; var a struct{F func()}; q:=&a; if i==0 {q.F=func(){use(p)}} else {p=&i}; go a.F()`},
		{"different join alias copy store contributors", `var p *int; var a struct{F func()}; q:=&a; r:=q; if i==0 {r.F=func(){use(p)}} else {p=&i}; go a.F()`},
		{"parallel swap", `f:=func(){use(i)}; g:=func(){}; if i==0 {use(i)}; f,g=g,f; if i==1 {use(i)}; go g(); _=f`},
		{"parallel destination address", `var a,b struct{F func()}; p:=&a; if i==0 {use(i)}; p,p.F=&b,func(){use(i)}; if i==1 {use(i)}; go a.F(); _=p`},
		{"parallel alias store join contributors", `var p *int; var a,b struct{F func()}; q:=&a; if i==0 {q,q.F=&b,func(){use(p)}} else {p=&i}; go a.F(); _=q`},
		{"field lhs reads", `var a struct{F func()}; p:=&a; if i==0 {use(i)}; p.F=func(){use(i)}; if i==1 {use(i)}; go a.F()`},
		{"index lhs reads", `fs:=make([]func(),1); p:=&fs; k:=0; if i==0 {use(i)}; (*p)[k]=func(){use(i)}; if i==1 {use(i)}; go fs[0]()`},
		{"map lhs keys", `m:=make(map[*int]func()); p:=&i; if i==0 {use(i)}; m[p]=func(){}; if i==1 {use(i)}; go use(m)`},
		{"go function and arguments", `p:=&i; f:=func(...any){}; if i==0 {use(i)}; go f(p,func(){use(i)})`},
		{"call result barrier", `f:=func(){use(i)}; if i==0 {use(i)}; go use(eval(f))`},
		{"literal nested frame", `p:=&i; if i==0 {use(i)}; go func(){q:=p; if i==1 {use(i)}; go use(q)}()`},
		{"inherited closure join contributors", `var p *int; f:=func(){use(p)}; go func(){if i==0 {} else {f=func(){}; p=&i}; go f()}()`},
		{"inherited address join contributors", `var p *int; var a struct{F func()}; q:=&a; go func(){if i==0 {q.F=func(){use(p)}} else {p=&i}; go a.F()}()`},
		{"inherited copied address join contributors", `var p *int; var a struct{F func()}; q:=&a; go func(){r:=q; if i==0 {r.F=func(){use(p)}} else {p=&i}; go a.F()}()`},
		{"literal local excluded", `if i==0 {use(i)}; go func(){p:=&i; if i==1 {use(i)}; go use(p)}()`},
		{"forward goto", `f:=func(){use(i)}; if i==0 {goto launch}; f=func(){}; launch: go f()`},
		{"backward goto", `f:=func(){}; launch: go f(); if i==0 {f=func(){use(i)}; goto launch}`},
		{"loop backedge", `f:=func(){}; for j:=0;j<3;j++ {go f(); f=func(){use(i)}}`},
		{"case fallthrough", `f:=func(){}; switch i {case 0: f=func(){use(i)}; fallthrough; default: go f()}`},
		{"type switch", `f:=func(){}; switch any(i).(type) {case int: f=func(){use(i)}; default: f=func(){}}; go f()`},
		{"break edge", `f:=func(){}; for {f=func(){use(i)}; if i==0 {break}; f=func(){}}; go f()`},
		{"continue edge", `f:=func(){}; for j:=0;j<3;j++ {go f(); f=func(){use(i)}; if i==0 {continue}; f=func(){}}`},
		{"select chosen edge", `f:=func(){use(i)}; ch:=make(chan func()); select {case f=<-ch: go f(); default: go f()}`},
		{"exit edge", `f:=func(){use(i)}; if i==0 {return}; go f()`},
		{"unreachable", `f:=func(){use(i)}; return; go f()`},
		{"identity shadow", `f:=func(){use(i)}; if i==0 {f:=func(){}; go f()}; go f()`},
		{"range private snapshots", `fs:=[]func(){func(){use(i)}}; for _,f:=range fs {fs=nil; if i==0 {use(i)}; go f()}`},
		{"range alias snapshot", `a:=[1]func(){func(){use(i)}}; p:=&a; if i==0 {use(i)}; for _,f:=range p {a=[1]func(){}; if i==1 {use(i)}; go f()}`},
		{"range complex target", `var a struct{F func()}; p:=&a; for _,p.F=range []func(){func(){use(i)}} {if i==0 {use(i)}; go a.F()}`},
		{"channel summary", `ch:=make(chan func(),1); ch<-func(){use(i)}; if i==0 {use(i)}; for f:=range ch {if i==1 {use(i)}; go f()}`},
	}
	for _, tc := range cases {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%v", tc.name, typed), func(t *testing.T) {
				s, body := loopLiveTestScanner(t, "var i int; for i=0;i<3;i++ {"+tc.body+"}", typed)
				want, _, _ := loopLiveTestRun(s, body, false)
				got, _, _ := loopLiveTestRun(s, body, true)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("pruning changed launch summaries:\ngot  %v\nwant %v", got, want)
				}
			})
		}
	}
}

func TestLoopPruneBindingsTransitiveAndImmutable(t *testing.T) {
	f, p, v, dead, slot := &loopVariable{}, &loopVariable{}, &loopVariable{}, &loopVariable{}, &loopVariable{}
	state := map[*loopVariable]loopValue{
		f: {p: loopRead}, p: {v: loopRead}, v: {p: loopIndirectWrite}, dead: {v: loopRead}, slot: {v: loopRead},
	}
	needed := map[*loopVariable]bool{f: true, dead: false}
	got := loopPruneBindings(state, needed, map[*loopVariable]*loopVariable{p: slot})
	if len(got) != 4 || got[dead] != nil || got[slot] == nil {
		t.Fatalf("lost a transitive/alias dependency or retained dead binding: %v", got)
	}
	delete(got, p)
	got[f] = nil
	if len(state) != 5 || state[p] == nil || state[f][p] != loopRead || len(needed) != 2 {
		t.Fatal("pruning mutated its inputs")
	}
	if got := loopPruneBindings(state, nil); len(got) != 0 || got == nil {
		t.Fatalf("empty live roots should produce a writable empty state: %v", got)
	}
}

func TestLoopFlowLiveLiteralFrameAndSimultaneousKills(t *testing.T) {
	s, body := loopLiveTestScanner(t, `var i int; p:=&i; q:=p; f:=func(){local:=p; use(local)}; p,q=q,p; go f(); _=q`, true)
	var literal *ast.FuncLit
	var swap *ast.AssignStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if n, ok := n.(*ast.FuncLit); ok {
			literal = n
		}
		if n, ok := n.(*ast.AssignStmt); ok && len(n.Lhs) == 2 {
			swap = n
		}
		return true
	})
	use, def, _, _ := s.loopFlowNodeBindings(literal)
	if len(use) != 2 || len(def) != 0 { // p and external use, never literal-local local
		t.Fatalf("literal construction should use only captures: use=%v def=%v", use, def)
	}
	use, def, _, _ = s.loopFlowNodeBindings(swap)
	if len(use) != 2 || len(def) != 2 {
		t.Fatalf("parallel assignment must read both old slots before killing either: use=%v def=%v", use, def)
	}
}

func TestLoopFlowLiveCopiedContentsAndRetainedSlots(t *testing.T) {
	for _, tc := range []struct {
		name, copy, launch string
		retainP            bool
	}{
		{"value", `q:=p`, `go use(q)`, false},
		{"closure", `f:=func(){use(p)}; q:=f`, `go q()`, true},
		{"address", `r:=&p; q:=r`, `go use(q)`, true},
	} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%v", tc.name, typed), func(t *testing.T) {
				s, body := loopLiveTestScanner(t, `var i int; p:=&i; `+tc.copy+`; if i==0 {use(i)}; `+tc.launch, typed)
				vars := map[string]*loopVariable{}
				for id, v := range s.index.ids {
					vars[id.Name] = v
				}
				p, i, slot := vars["p"], vars["i"], &loopVariable{}
				seed := map[*loopVariable]loopValue{p: {i: loopRead}, slot: {i: loopRead}}
				flow := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{p: slot}}
				a := loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}}
				graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
				live := flow.loopFlowLiveness(graph, &a, seed)
				found := false
				for _, block := range graph.Blocks {
					for _, node := range block.Nodes {
						if _, ok := node.(*ast.GoStmt); !ok {
							continue
						}
						found = true
						roots := live[block.Index]
						if !roots[vars["q"]] || !roots[i] || roots[p] != tc.retainP || roots[slot] != tc.retainP {
							t.Fatalf("copied contents confused with retained slot: roots=%v retainP=%v", roots, tc.retainP)
						}
						if roots[vars["f"]] || roots[vars["r"]] {
							t.Fatal("closure/address copy retained its source slot")
						}
					}
				}
				if !found {
					t.Fatal("missing launch block")
				}
			})
		}
	}
}

func loopLiveDeadAliasBody(n int) string {
	var body strings.Builder
	body.WriteString("var i int; for i=0;i<3;i++ {")
	for j := 0; j < n; j++ {
		fmt.Fprintf(&body, "v%d:=&i; if i==0 {use(v%d)};", j, j)
	}
	body.WriteString("go func(){use(i)}()}")
	return body.String()
}

func TestLoopFlowLiveDeadAliasScaling(t *testing.T) {
	for _, n := range []int{200, 400, 800} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, body := loopLiveTestScanner(t, loopLiveDeadAliasBody(n), true)
			got, maxState, totalState := loopLiveTestRun(s, body, true)
			if len(got) != 1 {
				t.Fatalf("got %d launches, want one", len(got))
			}
			for _, value := range got {
				found := false
				for v, mode := range value {
					if v.writes[nil] != token.NoPos && mode&loopRead != 0 {
						found = true // the outer i is still read by the launch
					}
				}
				if !found {
					t.Fatal("dead-alias pruning lost the loop capture")
				}
			}
			if maxState > 4 || totalState > 8*n+16 {
				t.Fatalf("dead bindings accumulate: max=%d total=%d n=%d", maxState, totalState, n)
			}
			t.Logf("n=%d max bindings/block=%d total bindings=%d", n, maxState, totalState)
		})
	}
}

func TestLoopFlowLiveDeadStoreAliases(t *testing.T) {
	// Evaluating p.F uses p, but the weak store's payload does not retain p.
	// Otherwise a live aggregate would keep every temporary store address alive.
	var body strings.Builder
	body.WriteString("var i int; var a struct{F func()}; for i=0;i<3;i++ {")
	for j := 0; j < 200; j++ {
		fmt.Fprintf(&body, "p%d:=&a; if i==0 {use(p%d)}; p%d.F=func(){use(i)};", j, j, j)
	}
	body.WriteString("go a.F()}")
	s, block := loopLiveTestScanner(t, body.String(), true)
	got, maxState, _ := loopLiveTestRun(s, block, true)
	if len(got) != 1 || maxState > 6 {
		t.Fatalf("dead store addresses accumulate: launches=%d max bindings=%d", len(got), maxState)
	}
}

// Each snapshot copies the retained &i contents, not the preceding pointer
// slot. The ifs split the CFG so historical-slot retention becomes quadratic.
func loopLiveCopiedAliasBody(n int) string {
	var body strings.Builder
	body.WriteString("var i int; for i=0;i<3;i++ {\np0:=&i\n")
	for j := 1; j <= n; j++ {
		fmt.Fprintf(&body, "p%d:=p%d;if i==0{use(p%d)}\n", j, j-1, j)
	}
	fmt.Fprintf(&body, "go use(p%d)\n}", n)
	return body.String()
}

func loopLiveCheckCopiedLaunch(t testing.TB, s *loopScanner, got map[*ast.GoStmt]loopValue) {
	t.Helper()
	var i *loopVariable
	for id, v := range s.index.ids {
		if id.Name == "i" {
			i = v
			break
		}
	}
	if len(got) != 1 || i == nil {
		t.Fatalf("got %d launches, want one retaining i", len(got))
	}
	for _, value := range got {
		if len(value) != 1 || value[i]&loopRead == 0 {
			t.Fatalf("copy changed retained contents: %v, want only i", value)
		}
	}
}

func loopLiveScanCopiedAliases(t testing.TB, s *loopScanner, body *ast.BlockStmt) {
	t.Helper()
	var findings []ASTFinding
	scanLoops(loopEnv{ctx: s.index.ctx, findings: &findings, shade: map[string]bool{}}, body)
	if len(findings) != 1 || findings[0].Rule != "goroutine-in-loop" {
		t.Fatalf("scanLoops lost copied shared pointer: %v", findings)
	}
}

func TestLoopFlowLiveCopiedAliasScaling(t *testing.T) {
	for _, n := range []int{800, 1600, 3200} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/typed=%v", n, typed), func(t *testing.T) {
				s, body := loopLiveTestScanner(t, loopLiveCopiedAliasBody(n), typed)
				got, maxState, totalState := loopLiveTestRun(s, body, true)
				loopLiveCheckCopiedLaunch(t, s, got)
				if maxState > 4 || totalState > 12*n+32 {
					t.Fatalf("historical copies accumulate: max=%d total=%d n=%d", maxState, totalState, n)
				}
				a := loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}}
				graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
				live := s.loopFlowLiveness(graph, &a)
				maxLive, totalLive := 0, 0
				for _, roots := range live {
					totalLive += len(roots)
					if len(roots) > maxLive {
						maxLive = len(roots)
					}
				}
				if maxLive > 6 || totalLive > 16*n+32 {
					t.Fatalf("historical liveness accumulates: max=%d total=%d n=%d", maxLive, totalLive, n)
				}
				loopLiveScanCopiedAliases(t, s, body)
				t.Logf("n=%d state max/total=%d/%d live max/total=%d/%d", n, maxState, totalState, maxLive, totalLive)
			})
		}
	}
}

// Exercise the actual production scan, including discovery, CFG refinement,
// effect replay, and reporting. Parsing/index setup here is outside the timer;
// scanLoops builds its own production identity index on every iteration.
func BenchmarkLoopFlowLiveCopiedAliases(b *testing.B) {
	for _, n := range []int{800, 1600, 3200} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s, body := loopLiveTestScanner(b, loopLiveCopiedAliasBody(n), false)
			b.ReportAllocs()
			b.ResetTimer()
			for j := 0; j < b.N; j++ {
				loopLiveScanCopiedAliases(b, s, body)
			}
		})
	}
}

func BenchmarkLoopFlowLiveDeadAliases(b *testing.B) {
	for _, n := range []int{200, 400, 800} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			s, body := loopLiveTestScanner(b, loopLiveDeadAliasBody(n), false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, maxState, totalState := loopLiveTestRun(s, body, true)
				if maxState > 4 || totalState > 8*n+16 {
					b.Fatal("dead bindings accumulate")
				}
			}
		})
	}
}
