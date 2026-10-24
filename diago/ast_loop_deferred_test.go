package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"testing"

	"golang.org/x/tools/go/cfg"
)

// These are intentionally public regression tests: the parent's CFG wiring is
// required, not a test-only invocation of a different analyzer implementation.
func TestLoopDeferredPublic(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"worker deferred writer", `func work(xs []int){v:=0; for range xs {go func(){defer func(){v++}()}()}}`, 1},
		{"named result deferred writer", `func work(xs []int)(v int){defer func(){v++}(); for range xs {go func(){use(v)}()}; return}`, 1},
		{"deferred writer after launch", `func work(xs []int){v:=0; for range xs {go func(){use(v)}();defer func(){v++}()}}`, 1},
		{"fresh local deferred writer after launch", `func work(xs []int){for range xs {v:=0;go func(){use(v)}();defer func(){v++}()}}`, 1},
		{"saved function value", `func work(xs []int){v:=0;f:=func(){v++};defer f();f=func(){};for range xs {go func(){use(v)}()}}`, 1},
		{"current captured function slot", `func work(xs []int){v:=0;f:=func(){v++};defer func(){f()}();f=func(){};for range xs {go func(){use(v)}()}}`, 0},
		{"current captured function slot becomes writer", `func work(xs []int){v:=0;f:=func(){};defer func(){f()}();f=func(){v++};for range xs {go func(){use(v)}()}}`, 1},
		{"deferred argument snapshot", `func work(xs []int){v,u:=0,0;p:=&v;defer func(p *int){(*p)++}(p);p=&u;for range xs {go func(){use(v)}()}}`, 1},
		{"saved pointer method receiver", `type deferredReadItem struct{N int};func(p *deferredReadItem) Read(){use(p.N)};func work(xs []int){for range xs {v,u:=deferredReadItem{},deferredReadItem{};p:=&v;defer p.Read();p=&u;go func(){v.N++}()}}`, 1},
		{"saved method receiver does not reload pointer", `type deferredReadItem struct{N int};func(p *deferredReadItem) Read(){use(p.N)};func work(xs []int){for range xs {v,u:=deferredReadItem{},deferredReadItem{};p:=&u;defer p.Read();p=&v;go func(){v.N++}()}}`, 0},
		{"deferred address only", `func work(xs []int){v:=0;defer func(){_=&v}();for range xs {go func(){use(v)}()}}`, 0},
		{"fresh deferred address only", `func work(xs []int){for range xs {v:=0;go func(){use(v)}();defer func(){_=&v}()}}`, 0},
		{"empty defer", `func work(xs []int){for range xs {v:=0;go func(){use(v)}();defer func(){}()}}`, 0},
		{"unreachable registration", `func work(xs []int){v:=0;goto launch;defer func(){v++}();launch:for range xs {go func(){use(v)}()}}`, 0},
		{"returning registration branch", `func work(xs []int){v:=0;if len(xs)==0 {defer func(){v++}();return};for range xs {go func(){use(v)}()}}`, 0},
		{"exclusive registration branch", `func work(xs []int){v:=0;if len(xs)==1 {defer func(){v++}()}else{for range xs {go func(){use(v)}()}}}`, 0},
		{"callback comparison stays latent", `func work(xs []int){v:=0;f:=func(){v++};defer func(){_=f==nil}();for range xs {go func(){use(v)}()}}`, 0},
		{"deferred constructor stays latent", `func work(xs []int){v:=0;defer func(){_=func(){v++}}();for range xs {go func(){use(v)}()}}`, 0},
		{"deferred method binding stays latent", `func work(xs []int){for range xs {v:=item{};defer func(){_=v.Use}();go func(){v.N++}()}}`, 0},
		{"LIFO binding update", `func work(xs []int){v:=0;f:=func(){v++};defer func(){f()}();defer func(){f=func(){}}();for range xs {go func(){use(v)}()}}`, 0},
		{"fresh worker deferred writer", `func work(xs []int){for range xs {v:=0;go func(){defer func(){v++}()}()}}`, 0},
		{"iterator exit before launch", `func work(xs []int){for range xs {v:=0;for range func(yield func()bool){defer func(){v++}();yield()}{};go func(){use(v)}()}}`, 0},
		{"iterator exit after yield launch", `func work(xs []int){for range xs {v:=0;for range func(yield func()bool){defer func(){v++}();yield()}{go func(){use(v)}()}}}`, 1},
		{"pending return unwinds iterator first", `func work(xs []int)(f func()){v:=0;defer func(){f()}();for range xs{go func(){use(v)}()};for range func(yield func()bool){defer func(){f=func(){}}();yield()}{return func(){v++}};return}`, 0},
		{"deferred escaping Go", `func work(xs []int){v:=0;for range xs {func(){defer func(){go func(){use(v)}()}()}();v++}}`, 1},
		{"deferred saved callback parameter", `func work(xs []int){v:=0;f:=func(){v++};defer func(cb func()){cb()}(f);f=func(){};for range xs {go func(){use(v)}()}}`, 1},
		{"deferred branching callee origins", `func work(xs []int){v:=0;f:=func(){};if len(xs)>1 {f=func(){v++}};defer f();for range xs {go func(){use(v)}()}}`, 1},
		{"older defer conflicts with deferred launch", `func work(xs []int){for range xs {v:=0;func(){defer func(){v++}();defer func(){go func(){use(v)}()}()}()}}`, 1},
		{"newer defer precedes deferred launch", `func work(xs []int){for range xs {v:=0;func(){defer func(){go func(){use(v)}()}();defer func(){v++}()}()}}`, 0},
		{"exclusive deferred workers", `func work(xs []int){for range xs {v:=0;func(){if len(xs)==1 {defer func(){go func(){use(v)}()}()}else{defer func(){v++}()}}()}}`, 0},
		{"nested iterator return unwinds defers", `func work(xs []int){for range xs {v:=0;func(){for range func(yield func()bool){defer func(){v++}();yield()}{for range func(yield func()bool){defer func(){go func(){use(v)}()}();yield()}{return}}}()}}`, 1},
		{"nested iterator exit before launch", `func work(xs []int){for range xs {v:=0;for range func(yield func()bool){defer func(){v++}();for range func(yield func()bool){defer func(){v++}();yield()}{yield()}}{};go func(){use(v)}()}}`, 0},
		{"deferred synchronous binding update", `func work(xs []int){v:=0;f:=func(){v++};defer func(){g:=func(){f=func(){}};g();f()}();for range xs{go func(){use(v)}()}}`, 0},
		{"deferred synchronous binding becomes writer", `func work(xs []int){v:=0;f:=func(){};defer func(){g:=func(){f=func(){v++}};g();f()}();for range xs{go func(){use(v)}()}}`, 1},
		{"saved callback Go escapes at deferred invocation", `func work(xs []int){for range xs{v:=0;func(){defer func(cb func()){cb()}(func(){go func(){use(v)}()})}();v++}}`, 1},
		{"uninvoked deferred constructor cannot escape", `func work(xs []int){for range xs{v:=0;f:=func(){defer func(){go func(){use(v)}()}()};_=f;v++}}`, 0},
		{"deferred callback alternatives escape", `func work(xs []int){for range xs{v:=0;func(){cb:=func(){};if len(xs)>1 {cb=func(){go func(){use(v)}()}};defer func(){cb()}()}();v++}}`, 1},
		{"nested return runs false cleanup", `func work(xs []int){for range xs{v:=0;func(){for range func(yield func()bool){defer func(){v++}();yield()}{for range func(yield func()bool){go func(){use(v)}();yield()}{return}}}()}}`, 1},
		{"fresh deferred callback preceding final launch", `func work(xs []int){for range xs{v:=0;func(){defer func(cb func()){cb()}(func(){v++})}();go func(){use(v)}()}}`, 0},
		{"nonreturning defer blocks older writer", `func work(xs []int){v:=0;defer func(){v++}();defer func(){for{}}();for range xs{go func(){use(v)}()}}`, 0},
		{"newer writer precedes nonreturning defer", `func work(xs []int){v:=0;defer func(){for{}}();defer func(){v++}();for range xs{go func(){use(v)}()}}`, 1},
		{"nonreturning deferred call blocks following writer", `func work(xs []int){v:=0;defer func(){spin:=func(){for{}};spin();v++}();for range xs{go func(){use(v)}()}}`, 0},
		{"deferred operand call ordering clears writer", `func work(xs []int){v:=0;f:=func(){v++};defer func(){g:=func()int{f=func(){};return 0};use(g(),func()int{f();return 0}())}();for range xs{go func(){use(v)}()}}`, 0},
		{"deferred operand call ordering installs writer", `func work(xs []int){v:=0;f:=func(){};defer func(){g:=func()int{f=func(){v++};return 0};use(g(),func()int{f();return 0}())}();for range xs{go func(){use(v)}()}}`, 1},
		{"deferred execution freezes callee before arguments", `func work(xs []int){v:=0;f:=func(int){v++};defer func(){g:=func()int{f=func(int){};return 0};f(g())}();for range xs{go func(){use(v)}()}}`, 1},
		{"iterator callback return unwinds inner before outer", `func work(xs []int){for range xs{v:=0;for range func(yield func()bool){defer func(){v++}();for range func(yield func()bool){defer func(){go func(){use(v)}()}();yield()}{return}}{}}}`, 1},
		{"nested defer saves callback before later operand mutation", `func work(xs []int){v:=0;f:=func(){v++};defer func(){g:=func()int{f=func(){};return 0};defer func(cb func(),_ int){cb()}(f,g())}();for range xs{go func(){use(v)}()}}`, 1},
		{"deferred callee snapshot drives LIFO binding updates", `func work(xs []int){v:=0;f:=func(){v++};defer func(){f()}();defer func(){set:=func(int){f=func(){}};g:=func()int{set=func(int){};return 0};set(g())}();for range xs{go func(){use(v)}()}}`, 0},
		{"recursive deferred binding fixed point", `func work(xs []int){v:=0;f:=func(){};n:=len(xs);var recur func();recur=func(){if n>0 {n--;recur();f=func(){v++}}};defer func(){f()}();defer recur();for range xs{go func(){use(v)}()}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+tc.source, "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

type deferredFixture struct {
	s     *loopScanner
	f     *loopFlow
	d     *loopDeferredFlow
	p     *loopDeferredPlan
	w     *loopWalk
	body  *ast.BlockStmt
	graph *cfg.CFG
	vars  map[string]*loopVariable
	exit  ast.Node
}

func newDeferredFixture(t testing.TB, source string) *deferredFixture {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "deferred.go", loopTestPrelude+source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	decl := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	ctx := loopContext{fset: fset, file: file, decl: decl, goVersion: "go1.23"}
	x := &loopIndex{ctx: ctx, fresh: true, ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{}, parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{}, regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{}}
	x.pushScope()
	x.fields(decl.Recv)
	x.fields(decl.Type.Params)
	x.fields(decl.Type.Results)
	for _, stmt := range decl.Body.List {
		x.node(stmt)
	}
	s := &loopScanner{index: x, rootNode: decl, env: loopEnv{ctx: ctx}}
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
	d := newLoopDeferredFlow(s, f.aliases)
	graph := cfg.New(decl.Body, func(*ast.CallExpr) bool { return true })
	p := d.plan(graph, nil)
	z := &deferredFixture{s: s, f: f, d: d, p: p, w: &loopWalk{values: map[*loopVariable]loopValue{}, aliases: f.aliases}, body: decl.Body, graph: graph, vars: map[string]*loopVariable{}}
	for id, v := range x.ids {
		if v.owner == nil {
			z.vars[id.Name] = v
		}
	}
	for _, block := range graph.Blocks {
		if ret := block.Return(); ret != nil && block.Live {
			z.exit = ret
		}
	}
	d.enter(nil, z.w)
	return z
}

func (z *deferredFixture) step(node ast.Node) {
	z.d.transfer(node, z.w)
	z.p.transfer(node, z.w, z.f.addressAliases)
	z.f.transfer(node, z.w, &loopFlowAST{})
}
func (z *deferredFixture) steps() {
	for _, node := range z.body.List {
		if _, ok := node.(*ast.ReturnStmt); !ok {
			z.step(node)
		}
	}
}
func deepDeferredBindings(src map[*loopVariable]loopValue) map[*loopVariable]loopValue {
	out := map[*loopVariable]loopValue{}
	for v, value := range src {
		if value == nil {
			out[v] = nil
			continue
		}
		out[v] = loopValue{}
		loopMerge(out[v], value)
	}
	return out
}

func TestLoopDeferredSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		write        bool
	}{
		{"saved function", `func work(){v:=0;f:=func(){v++};defer f();f=func(){};return}`, true},
		{"saved callable pointer", `func work(){v:=0;f:=func(){v++};p:=&f;defer (*p)();f=func(){};return}`, true},
		{"empty callback", `func work(){v:=0;_=v;defer func(){}();return}`, false},
		{"current function cleared", `func work(){v:=0;f:=func(){v++};defer func(){f()}();f=func(){};return}`, false},
		{"current function becomes writer", `func work(){v:=0;f:=func(){};defer func(){f()}();f=func(){v++};return}`, true},
		{"address only", `func work(){v:=0;defer func(){_=&v}();return}`, false},
		{"comparison", `func work(){v:=0;f:=func(){v++};defer func(){_=f==nil}();return}`, false},
		{"method binding", `func work(){v:=item{};defer func(){_=v.Use}();return}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := newDeferredFixture(t, tc.source)
			z.steps()
			before := deepDeferredBindings(z.w.values)
			seen, writes := 0, false
			z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
				if replay {
					seen++
					writes = writes || inv.effects[z.vars["v"]]&loopWrite != 0
				}
				return state
			}, true)
			if seen != 1 || writes != tc.write {
				t.Fatalf("seen=%d writes=%v want %v", seen, writes, tc.write)
			}
			if !reflect.DeepEqual(before, z.w.values) {
				t.Fatal("exit execution mutated a saved input")
			}
		})
	}
}

func TestLoopDeferredUnregisteredDoesNotExecute(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;goto done;defer func(){v++}();done:return}`)
	if len(z.d.slots) != 1 {
		t.Fatal("fixture lacks its unreachable registration")
	}
	out := z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(loopDeferredInvocation, map[*loopVariable]loopValue, bool) map[*loopVariable]loopValue {
		t.Fatal("unexecuted defer became active")
		return nil
	}, true)
	if out == nil || !z.p.events(z.exit, z.w).empty {
		t.Fatal("empty stack lost its completed-exit edge")
	}
}

func TestLoopDeferredArgumentsAndReceiverSnapshots(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v,u:=0,0;p:=&v;defer func(arg *int){(*arg)++}(p);p=&u;return}`)
	z.steps()
	var inv loopDeferredInvocation
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(i loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			inv = i
		}
		return state
	}, true)
	if inv.literal == nil {
		t.Fatal("lost literal identity")
	}
	parameter := z.s.index.ids[inv.literal.Type.Params.List[0].Names[0]]
	if inv.seed[parameter][z.vars["v"]]&loopRead == 0 || inv.seed[parameter][z.vars["u"]] != 0 {
		t.Fatal("parameter did not keep its registered pointer value")
	}
	if inv.seed[z.f.aliasSlot(parameter)][z.vars["v"]]&loopRead == 0 {
		t.Fatal("lost saved argument address proof")
	}
	if inv.seed[z.vars["p"]][z.vars["u"]]&loopRead == 0 {
		t.Fatal("captured variables must still have current bindings")
	}

	z = newDeferredFixture(t, `func work(){v,u:=item{},item{};p:=&v;defer p.Use();p=&u;return}`)
	z.steps()
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(i loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay && (i.effects[z.vars["v"]]&loopRead == 0 || i.effects[z.vars["u"]]&loopRead != 0) {
			t.Fatal("method receiver was reloaded at exit")
		}
		return state
	}, true)

	z = newDeferredFixture(t, `func work(){v,u:=0,0;p,q:=&v,&u;defer func(args ...*int){_=args}(p,q);return}`)
	z.steps()
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(i loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			parameter := z.s.index.ids[i.literal.Type.Params.List[0].Names[0]]
			if len(i.seed[parameter]) != 2 {
				t.Fatal("variadic parameter lost saved argument values")
			}
			if len(i.seed[z.f.aliasSlot(parameter)]) != 0 {
				t.Fatal("variadic slice incorrectly aliases its first element's storage")
			}
		}
		return state
	}, true)
}

func TestLoopDeferredSavedCallableParameterOrigins(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};defer func(cb func()){cb()}(f);f=func(){};return}`)
	z.steps()
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			call := inv.literal.Body.List[0].(*ast.ExprStmt).X.(*ast.CallExpr)
			targets, opaque := z.d.callees(call.Fun, &loopWalk{values: inv.seed})
			if opaque || len(targets) != 1 || len(targets[0].Body.List) != 1 {
				t.Fatal("saved callable parameter lost its original target")
			}
			if _, ok := targets[0].Body.List[0].(*ast.IncDecStmt); !ok {
				t.Fatal("callable argument was reloaded from overwritten f")
			}
			current, opaque := z.d.callees(z.vars["f"].ident, &loopWalk{values: inv.seed})
			if opaque || len(current) != 1 || len(current[0].Body.List) != 0 {
				t.Fatal("captured variable did not keep its current callable target")
			}
		}
		return state
	}, true)
}

func TestLoopDeferredLIFOCurrentBindings(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};defer func(){f()}();defer func(){f=func(){}}();return}`)
	z.steps()
	seen := 0
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			seen++
			if inv.effects[z.vars["v"]]&loopWrite != 0 {
				t.Fatal("older defer did not see newer defer's binding update")
			}
		}
		w := &loopWalk{values: state, aliases: z.f.aliases}
		for _, node := range inv.literal.Body.List {
			z.d.transfer(node, w)
			z.f.transfer(node, w, &loopFlowAST{})
		}
		return w.values
	}, true)
	if seen != 2 {
		t.Fatalf("got %d invocations, want 2", seen)
	}
	events := z.p.events(z.exit, z.w)
	older, newer := z.body.List[2].(*ast.DeferStmt), z.body.List[3].(*ast.DeferStmt)
	if events.empty || !reflect.DeepEqual(events.entries, []*ast.DeferStmt{newer}) || !reflect.DeepEqual(events.next[newer], []*ast.DeferStmt{older}) || !events.done[older] {
		t.Fatal("physical event graph is not LIFO")
	}
}

func TestLoopDeferredLoopAccumulationAndEpochs(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;defer func(){v++}();return}`)
	z.step(z.body.List[0])
	stmt := z.body.List[1].(*ast.DeferStmt)
	z.step(stmt)
	saved := deepDeferredBindings(z.w.values)
	v := z.vars["v"]
	for slot, value := range z.w.values {
		z.w.values[slot] = loopEpochAgeValue(value, map[*loopVariable]bool{v: true})
	}
	z.step(stmt)
	slot := z.d.slots[stmt]
	mode := z.w.values[slot.callee][v]
	if loopEpochPriorMode(mode)&loopCallableLatentMask == 0 || loopEpochCurrentMode(mode)&loopCallableLatentMask == 0 {
		t.Fatalf("lost accumulated epochs: %#x", mode)
	}
	if saved[slot.callee][v]&(loopPriorRef|loopEpochMixedRef) != 0 {
		t.Fatal("aging/registration mutated old snapshot")
	}
	calls := 0
	result := z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			calls++
		}
		return state
	}, true)
	if result == nil || calls != 1 {
		t.Fatalf("cyclic stack did not converge: calls=%d", calls)
	}
	if len(z.d.slots) != 1 {
		t.Fatal("loop registration grew unbounded slots")
	}
}

func TestLoopDeferredCapturedEpochProjection(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};defer func(){f()}();return}`)
	z.steps()
	stmt := z.body.List[2].(*ast.DeferStmt)
	slot := z.d.slots[stmt]
	f := z.vars["f"]
	z.w.values[slot.callee] = loopEpochAgeValue(z.w.values[slot.callee], map[*loopVariable]bool{f: true})
	seen := 0
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if !replay {
			return state
		}
		seen++
		if len(inv.seed[f]) != 0 || len(inv.seed[z.d.companion(f)]) != 0 {
			t.Fatal("old captured slot loaded the current allocation's contents")
		}
		if inv.effects[z.vars["v"]]&loopWrite != 0 {
			t.Fatal("old callable capture invoked current contents")
		}
		if inv.referenceMode(f, loopRead|loopWrite) != loopRead|loopWrite|loopPriorRef {
			t.Fatal("actual callee access lost captured epoch")
		}
		if !reflect.DeepEqual(inv.writeback(f, state[f], nil), state[f]) {
			t.Fatal("old captured assignment overwrote the current binding")
		}
		return state
	}, true)
	if seen != 1 {
		t.Fatal("lost old deferred invocation")
	}
	inv := loopDeferredInvocation{callee: loopValue{f: loopRetain | loopRead<<loopCallableShift | loopPriorRef | loopEpochMixedRef}}
	if mode := inv.referenceMode(f, loopWrite); loopEpochCurrentMode(mode)&loopWrite == 0 || loopEpochPriorMode(mode)&loopWrite == 0 {
		t.Fatal("mixed captured access lost an epoch")
	}
	before, after := loopValue{f: loopRead}, loopValue{z.vars["v"]: loopRead}
	if merged := inv.writeback(f, before, after); len(merged) != 2 || len(before) != 1 {
		t.Fatal("mixed writeback was strong or mutated its input")
	}
}

func TestLoopDeferredLivenessCopiesVersusCapturedSlots(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};defer f();defer func(){f()}();return}`)
	f := z.vars["f"]
	symbol := &loopVariable{}
	symbolic := &loopWalk{values: map[*loopVariable]loopValue{f: {symbol: loopRead}}}
	copyStmt := z.body.List[2].(*ast.DeferStmt)
	captureStmt := z.body.List[3].(*ast.DeferStmt)
	_, _, copies := z.p.live(copyStmt, symbolic, nil)
	_, _, captures := z.p.live(captureStmt, symbolic, nil)
	if copies[z.d.slots[copyStmt].callee][symbol] == 0 || copies[z.d.slots[copyStmt].callee][f] != 0 {
		t.Fatal("saved callee became a reference to the overwritten slot")
	}
	if captures[z.d.slots[captureStmt].callee][f] == 0 {
		t.Fatal("captured variable lost exit-time dependency")
	}
	uses, _, _ := z.p.live(z.exit, symbolic, nil)
	if !uses[z.d.slots[copyStmt].callee] || !uses[z.d.slots[captureStmt].callee] {
		t.Fatal("matching exit does not keep snapshots live")
	}
	z.steps()
	kept := loopPruneBindings(z.w.values, uses, z.f.aliases, z.d.companions)
	if kept[z.d.companion(f)] == nil {
		t.Fatal("captured function origin companion was pruned")
	}
}

func TestLoopDeferredFunctionBoundariesAndIteratorMarkers(t *testing.T) {
	z := newDeferredFixture(t, `func work()(v int){defer func(){v++}();f:=func(){};_=f;return}`)
	z.step(z.body.List[0])
	inner := z.body.List[1].(*ast.AssignStmt).Rhs[0].(*ast.FuncLit)
	innerGraph := cfg.New(inner.Body, func(*ast.CallExpr) bool { return true })
	innerPlan := z.d.plan(innerGraph, inner)
	z.d.enter(inner, z.w)
	ret := innerGraph.Blocks[0].Return()
	_, results, ok := innerPlan.returnInfo(ret)
	if !ok || len(results) != 0 {
		t.Fatal("nested void implicit return inherited ancestor named results")
	}
	count := 0
	innerPlan.executeExit(ret, z.w, z.f.aliasSlot, func(loopDeferredInvocation, map[*loopVariable]loopValue, bool) map[*loopVariable]loopValue {
		count++
		return nil
	}, true)
	if count != 0 {
		t.Fatal("nested return executed ancestor defers")
	}
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(_ loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			count++
		}
		return state
	}, true)
	if count != 1 {
		t.Fatal("nested entry cleared ancestor's deferred calls")
	}

	z = newDeferredFixture(t, `func work(){v:=0;it:=func(yield func()bool){defer func(){v++}()};_=it;return}`)
	it := z.body.List[1].(*ast.AssignStmt).Rhs[0].(*ast.FuncLit)
	stmt := it.Body.List[0].(*ast.DeferStmt)
	entry := z.d.iteratorEntry(it, it.Pos())
	exit := z.d.iteratorExit(&ast.ReturnStmt{Return: it.End()}, it)
	graph := &cfg.CFG{Blocks: []*cfg.Block{{Nodes: []ast.Node{entry, stmt, exit}}}}
	plan := z.d.plan(graph, nil)
	plan.transfer(entry, z.w, z.f.addressAliases)
	plan.transfer(stmt, z.w, z.f.addressAliases)
	count = 0
	plan.executeExit(exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			count++
			if inv.owner != it {
				t.Fatal("iterator defer assigned caller owner")
			}
		}
		return state
	}, true)
	if count != 1 {
		t.Fatal("iterator exit lost deferred callback")
	}
	plan.transfer(entry, z.w, z.f.addressAliases)
	plan.executeExit(exit, z.w, z.f.aliasSlot, func(loopDeferredInvocation, map[*loopVariable]loopValue, bool) map[*loopVariable]loopValue {
		t.Fatal("fresh iterator entry retained old registrations")
		return nil
	}, true)
}

func TestLoopDeferredNamedResultsOrdering(t *testing.T) {
	z := newDeferredFixture(t, `func work()(f func()){v:=0;defer func(){f()}();return func(){v++}}`)
	z.steps()
	z.f.transferDeferredReturn(z.p, z.exit, z.w)
	seen := false
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			seen = inv.effects[z.vars["v"]]&loopWrite != 0
		}
		return state
	}, true)
	if !seen {
		t.Fatal("defer did not observe explicit named-result assignment")
	}
	e := z.s.beginFlowEffects()
	var modes []loopAccess
	e.physical = func(a loopDirectAccess) {
		if a.variable == z.vars["f"] {
			modes = append(modes, a.mode)
		}
	}
	z.p.recordReturnBefore(z.exit, z.w, e)
	z.p.recordReturnAfter(z.exit, z.w, e)
	if !reflect.DeepEqual(modes, []loopAccess{loopWrite, loopRead}) {
		t.Fatalf("named result effects are not separated: %v", modes)
	}
}

func TestLoopDeferredPendingReturnCompletion(t *testing.T) {
	z := newDeferredFixture(t, `func work()(f func()){noop:=func(){};_=noop;v:=0;defer func(){f()}();return func(){v++}}`)
	z.steps()
	ret := z.exit.(*ast.ReturnStmt)
	finish := z.d.pendingExit(ret, nil)
	completion := &cfg.Block{Index: 1, Nodes: []ast.Node{finish}}
	pending := &cfg.Block{Nodes: []ast.Node{ret}, Succs: []*cfg.Block{completion}}
	plan := z.d.plan(&cfg.CFG{Blocks: []*cfg.Block{pending, completion}}, nil)
	if _, ok := plan.exits[ret]; ok {
		t.Fatal("pending return is not a function exit")
	}
	z.f.transferDeferredReturn(plan, ret, z.w)
	plan.executeExit(ret, z.w, z.f.aliasSlot, func(loopDeferredInvocation, map[*loopVariable]loopValue, bool) map[*loopVariable]loopValue {
		t.Fatal("root defer ran before iterator unwound")
		return nil
	}, true)
	// A stopped iterator then replaces the named result. Completion must not
	// evaluate/assign the return expression a second time.
	z.w.values[z.vars["f"]] = z.w.values[z.vars["noop"]]
	z.w.values[z.d.companion(z.vars["f"])] = z.w.values[z.d.companion(z.vars["noop"])]
	z.f.transferDeferredReturn(plan, finish, z.w)
	count := 0
	plan.executeExit(finish, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if replay {
			count++
			if inv.effects[z.vars["v"]]&loopWrite != 0 {
				t.Fatal("return expression was reassigned after iterator unwinding")
			}
		}
		return state
	}, true)
	if count != 1 {
		t.Fatal("final continuation did not execute root defer")
	}
	// Iterator markers retain operand syntax for the ordinary event recorder.
	marker := z.d.iteratorExit(ret, nil)
	c := &loopFlowConflicts{s: z.s}
	g := &loopConflictFrame{nodes: map[ast.Node][]int{}, births: map[*loopVariable][]int{}}
	c.recordSyncNode(g, marker, g.point())
	if len(g.nodes[ret]) == 0 || len(g.nodes[marker]) == 0 {
		t.Fatal("iterator return marker hid its operand events")
	}
}

func TestLoopDeferredGuardedExitReachability(t *testing.T) {
	// entry -> registration OR launch -> common exit. A naive exit access
	// races with launch, but that registration was never reached on its path.
	g := &loopConflictFrame{nodes: map[ast.Node][]int{}, effects: map[int][]loopDirectAccess{}}
	for i := 0; i < 4; i++ {
		g.point()
	}
	g.edge(0, 1)
	g.edge(0, 2)
	g.edge(1, 3)
	g.edge(2, 3)
	stmt := &ast.GoStmt{}
	frame, mapping := loopDeferredGuardGraph(loopSyncEscapeFrame{graph: g, launches: []loopSyncLaunch{{stmt: stmt, point: 2}}}, []int{1}, nil)
	if mapping[3][1] < 0 || mapping[3][0] < 0 {
		t.Fatal("exit should have active and inactive contexts")
	}
	if len(frame.launches) != 1 {
		t.Fatal("unexpected guard launch duplication")
	}
	reaches := func(from, to int) bool {
		seen := map[int]bool{}
		queue := []int{from}
		for len(queue) != 0 {
			p := queue[0]
			queue = queue[1:]
			if p == to {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, frame.graph.points[p].next...)
			}
		}
		return false
	}
	if reaches(frame.launches[0].point, mapping[3][1]) {
		t.Fatal("exclusive branch reached an active deferred exit")
	}
	if !reaches(frame.launches[0].point, mapping[3][0]) {
		t.Fatal("guard removed the actual launch path")
	}
	// A registration after the launch really does reach an active exit.
	g.edge(2, 1)
	frame, mapping = loopDeferredGuardGraph(loopSyncEscapeFrame{graph: g, launches: []loopSyncLaunch{{stmt: stmt, point: 2}}}, []int{1}, nil)
	if !reaches(frame.launches[0].point, mapping[3][1]) {
		t.Fatal("post-launch deferred writer was incorrectly suppressed")
	}
	// The next iterator invocation resets the guard before its exit.
	g.point()
	g.edge(3, 4)
	frame, mapping = loopDeferredGuardGraph(loopSyncEscapeFrame{graph: g}, []int{1}, []int{3})
	if mapping[4][1] >= 0 || mapping[4][0] < 0 {
		t.Fatal("fresh invocation did not reset the active registration")
	}
}
