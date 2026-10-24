package diago

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

func TestLoopDeferredProductionCopiedAliasLiveness(t *testing.T) {
	const n = 3200
	s, body := loopLiveTestScanner(t, loopLiveCopiedAliasBody(n), false)
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
	f.deferred = newLoopDeferredFlow(s, f.aliases)
	prepared := f.prepareDeferredCFG(body, nil)
	live := f.loopFlowLiveness(prepared.graph, prepared.adapter, nil)
	max, total := 0, 0
	for _, roots := range live {
		total += len(roots)
		if len(roots) > max {
			max = len(roots)
		}
	}
	if max > 12 || total > 32*n+64 {
		t.Fatalf("production metadata retains historical aliases: max=%d total=%d", max, total)
	}
}

func TestLoopDeferredExecutionDAGScaling(t *testing.T) {
	// The execution tree has 2^48 leaves. Binding simulation AND actual replay
	// must intern contexts instead of recursively expanding that tree.
	const depth = 48
	var source strings.Builder
	source.WriteString(`func work(xs []int){v:=0;f0:=func(){v++};`)
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&source, "f%d:=func(){f%d();f%d()};", i, i-1, i-1)
	}
	fmt.Fprintf(&source, "defer f%d();for range xs{go func(){use(v)}()}}", depth)
	for _, mode := range []string{"typed", "lexical"} {
		t.Run(mode, func(t *testing.T) {
			got := analyzeLoopRegression(t, loopTestPrelude+source.String(), "go1.23", mode)
			if n := countRules(got)["goroutine-in-loop"]; n != 1 {
				t.Fatalf("got %d findings, want 1: %+v", n, got)
			}
		})
	}
}

func TestLoopDeferredNewReferencesKeepCapturedEpoch(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;defer func(){g:=func(){v++};p:=&v;_=g;_=p}();return}`)
	z.steps()
	stmt := z.body.List[1].(*ast.DeferStmt)
	slot := z.d.slots[stmt]
	v := z.vars["v"]
	z.w.values[slot.callee] = loopEpochAgeValue(z.w.values[slot.callee], map[*loopVariable]bool{v: true})
	z.p.executeExit(z.exit, z.w, z.f.aliasSlot, func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if !replay {
			return state
		}
		w := &loopWalk{values: inv.seed, aliases: z.f.aliases, reference: &inv}
		closure := inv.literal.Body.List[0].(*ast.AssignStmt).Rhs[0]
		address := inv.literal.Body.List[1].(*ast.AssignStmt).Rhs[0]
		for _, value := range []loopValue{z.s.value(closure, w), z.s.value(address, w), z.f.addressAliases(address, w)} {
			if value[v]&loopPriorRef == 0 || loopEpochCurrentMode(value[v]) != 0 {
				t.Fatalf("new reference lost the captured cell epoch: %#x", value[v])
			}
		}
		return state
	}, true)
}
