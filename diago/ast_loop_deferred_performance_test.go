package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
	"testing"
)

func TestLoopDeferredExitFactoredCardinality(t *testing.T) {
	for _, n := range []int{120, 240, 480} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			// Deliberately use the unfactored operand plan: even arbitrary saved
			// operands must not enter the per-event binding fixed point.
			z := newDeferredFixture(t, "func work(){v:=0;"+strings.Repeat("defer func(){v++}();", n)+"return}")
			z.steps()
			other := z.d.owner(&ast.FuncLit{})
			z.w.values[other.top] = loopValue{other.bottom: loopRetain}
			before := deepDeferredBindings(z.w.values)
			seen := 0
			run := func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
				for _, bindings := range []map[*loopVariable]loopValue{state, inv.seed} {
					for slot := range bindings {
						if z.d.savedSlots[slot] {
							t.Fatal("event retained private stack storage")
						}
					}
				}
				if replay {
					seen++
				}
				return state
			}
			out := z.p.executeExit(z.exit, z.w, z.f.aliasSlot, run, true)
			if seen != n || out[other.top][other.bottom] == 0 {
				t.Fatal("lost invocation or enclosing owner's pending stack")
			}
			if !reflect.DeepEqual(before, z.w.values) {
				t.Fatal("factoring mutated saved operands")
			}
			// This rejects the former quadratic copies without timing/RSS
			// assumptions. Setup, parsing, and type checking are outside it.
			allocs := testing.AllocsPerRun(2, func() {
				z.p.executeExit(z.exit, z.w, z.f.aliasSlot, nil, false)
			})
			if allocs > float64(80*n+100) {
				t.Fatalf("exit allocates whole operand maps per event: %.0f allocations for %d events", allocs, n)
			}
		})
	}
}

func TestLoopDeferredInvariantOperandsAcrossCFGBlocks(t *testing.T) {
	for _, n := range []int{120, 240, 480} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, body := loopLiveTestScanner(t, "v:=0;b:=true;"+strings.Repeat("defer func(){v++}();if b{use(v)};", n), true)
			f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
			f.deferred = newLoopDeferredFlow(s, f.aliases)
			prepared := f.prepareDeferredCFG(body, nil)
			if len(prepared.graph.Blocks) < n {
				t.Fatal("fixture did not split the CFG")
			}
			if len(prepared.plan.static) != n {
				t.Fatal("lost source registrations")
			}
			for slot, fact := range prepared.plan.static {
				if fact.previous == nil || fact.literal == nil || len(prepared.plan.storage(slot)) != 0 {
					t.Fatal("invariant operands still occupy CFG binding slots")
				}
			}
			live := f.loopFlowLiveness(prepared.graph, prepared.adapter, nil)
			for _, roots := range live {
				if len(roots) > 12 {
					t.Fatalf("block retains saved operand history: %d roots", len(roots))
				}
			}
		})
	}
}

func TestLoopDeferredInvariantOperandBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, source           string
		literals, predecessors int
	}{
		{"straight", `v:=0;defer func(){v++}();defer func(){v++}()`, 2, 2},
		{"exclusive join", `v:=0;b:=true;if b{defer func(){v++}()}else{defer func(){v--}()};defer func(){v++}()`, 3, 2},
		{"loop predecessor varies", `v:=0;b:=true;for b{defer func(){v++}()}`, 1, 0},
		{"fresh cell ages", `b:=true;for b{v:=0;defer func(){v++}()}`, 0, 0},
		{"arguments are dynamic", `v:=0;defer func(p *int){(*p)++}(&v)`, 0, 1},
		{"function slot is dynamic", `v:=0;f:=func(){v++};defer f()`, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, body := loopLiveTestScanner(t, tc.source, true)
			f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
			f.deferred = newLoopDeferredFlow(s, f.aliases)
			p := f.prepareDeferredCFG(body, nil).plan
			literals, predecessors := 0, 0
			for _, fact := range p.static {
				if fact.literal != nil {
					literals++
				}
				if fact.previous != nil {
					predecessors++
				}
			}
			if literals != tc.literals || predecessors != tc.predecessors {
				t.Fatalf("facts = (%d,%d), want (%d,%d)", literals, predecessors, tc.literals, tc.predecessors)
			}
		})
	}
}

func TestLoopDeferredFactoredPublicSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"free cell reloaded", `func work(xs []int){v:=0;f:=func(){};defer func(){f()}();defer func(){f=func(){v++}}();for range xs{go func(){use(v)}()}}`, 1},
		{"branched unconditional stack", `func work(xs []int){for range xs{v:=0;func(){defer func(){go func(){v++}()}();if len(xs)>1{use(v)};defer func(){go func(){v++}()}()}()}}`, 2},
		{"loop snapshots age", `func work(xs []int){for range xs{v:=0;go func(){use(v)}();defer func(){v++}()}}`, 1},
		{"conditional predecessor", `func work(xs []int){for range xs{v:=0;func(){if len(xs)>1{defer func(){v++}()};defer func(){go func(){use(v)}()}()}()}}`, 1},
		{"prior captured cell", `func work(xs []int){for range xs{v:=0;f:=func(){defer func(){v++}()};defer f();go func(){use(v)}()}}`, 1},
	} {
		for _, mode := range []string{"typed", "lexical", "partial"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				got := analyzeLoopRegression(t, loopTestPrelude+tc.source, "go1.23", mode)
				if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
					t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
				}
			})
		}
	}
}

func BenchmarkLoopDeferredExitBindings(b *testing.B) {
	for _, n := range []int{120, 240, 480, 960} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			z := newDeferredFixture(b, "func work(){v:=0;"+strings.Repeat("defer func(){v++}();", n)+"return}")
			z.steps()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				z.p.executeExit(z.exit, z.w, z.f.aliasSlot, nil, false)
			}
		})
	}
}

func BenchmarkLoopDeferredAcrossCFGBlocks(b *testing.B) {
	for _, n := range []int{120, 240, 480, 960} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			source := loopTestPrelude + "func work(xs []int){for range xs{n:=0;func(){" + strings.Repeat("defer func(){go func(){n++}()}();if len(xs)>1{use(n)};", n) + "}()}}"
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := analyzeLoopRegression(b, source, "go1.23", "typed"); countRules(got)["goroutine-in-loop"] != n {
					b.Fatal("lost launches")
				}
			}
		})
	}
}
