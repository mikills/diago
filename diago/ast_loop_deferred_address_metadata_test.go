package diago

import (
	"go/ast"
	"reflect"
	"testing"
)

// Source IDs are the original GoStmt identities; projection must not replace
// them with registration/exit nodes or borrow a constructor execution context.
func TestLoopDeferredAddressWritebackExecutionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"parameter", `for range xs{v:=0;func(){f:=func(){};defer func(){f()}();defer func(p *func()){*p=func(){go func(){use(v)}()}}(&f)}();v++}`, true},
		{"captured", `for range xs{v:=0;func(){f:=func(){};p:=&f;defer func(){f()}();defer func(){*p=func(){go func(){use(v)}()}}()}();v++}`, true},
		{"reboundAlias", `for range xs{v:=0;func(){f,g:=func(){},func(){go func(){use(v)}()};p:=&f;defer func(){(*p)()}();defer func(pp **func(),q *func()){*pp=q}(&p,&g)}();v++}`, true},
		{"killedOrigin", `for range xs{v:=0;func(){f:=func(){go func(){use(v)}()};defer func(){f()}();defer func(p *func()){*p=func(){}}(&f)}();v++}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := loopSyncTestScanner(t, tc.body, "1.23", "lexical")
			c := loopLaunchTestContexts(s)
			if len(s.launches) != 1 {
				t.Fatal("expected one source go")
			}
			source := s.launches[0].stmt
			owner := s.index.locations[source].owner
			seenActual := false
			for _, frame := range c.activeContextFrames() {
				if frame.owner == owner && frame.actual {
					seenActual = true
					if frame.execution == nil || frame.execution.literal != owner {
						t.Fatal("actual callback lost execution identity")
					}
				}
			}
			if seenActual != tc.want {
				t.Fatalf("callback actual=%t want %t", seenActual, tc.want)
			}
			escapingGoSourceIDs := map[*ast.GoStmt]bool{}
			for _, frame := range c.projectContextEscapes().frames {
				if frame.graph.root {
					for _, launch := range frame.launches {
						escapingGoSourceIDs[launch.stmt] = true
					}
				}
			}
			want := map[*ast.GoStmt]bool{}
			if tc.want {
				want[source] = true
			}
			if !reflect.DeepEqual(escapingGoSourceIDs, want) {
				t.Fatalf("root escaping go sources=%v want %v", escapingGoSourceIDs, want)
			}
		})
	}
}

func TestLoopDeferredAddressExportsExcludeRetainedData(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;a:=func(){v++};bad:=func(){a()};f:=func(){};defer func(cb func(),p *func()){local:=func(){};r:=&local;_=r;*p=cb}(bad,&f);return}`)
	z.f.deferred = z.d
	z.f.conflicts = &loopFlowConflicts{ids: map[*loopVariable]uint64{}}
	z.steps()
	stmt := z.body.List[4].(*ast.DeferStmt)
	inv := z.p.invocations(z.d.slots[stmt], z.w.values, z.f.aliasSlot)[0]
	inv.seed = z.f.deferredSeed(inv)
	output := z.f.simulateDeferred(inv)
	a, f := z.vars["a"], z.vars["f"]
	if len(inv.seed[a]) == 0 {
		t.Fatal("fixture did not retain callback data")
	}
	// Even a changed retained data binding is not an export without lexical
	// capture or actual local-address evidence. No general heap writeback.
	output = loopCloneValues(output)
	delete(output, a)
	got := z.f.exportDeferred(inv, loopCloneValues(z.w.values), output)
	if !reflect.DeepEqual(got[a], z.w.values[a]) {
		t.Fatal("retained callback data was exported as an addressed cell")
	}
	if got[f][a] == 0 {
		t.Fatal("proven local cell did not receive callback contents")
	}
	for _, v := range z.s.index.ids {
		if v.owner == inv.literal {
			for _, slot := range []*loopVariable{v, z.f.aliases[v], z.d.companions[v]} {
				if slot != nil {
					if _, leaked := got[slot]; leaked {
						t.Fatalf("fresh callee binding/metadata leaked: %s", v.ident.Name)
					}
				}
			}
		}
	}
}
