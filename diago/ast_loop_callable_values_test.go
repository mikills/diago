package diago

import (
	"go/ast"
	"reflect"
	"testing"
)

func TestLoopCallableValuesPublic(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"single callback field comparison", `v:=0; s:=struct{F func()}{func(){v++}}; for range xs {go func(){_=s.F==nil}()}`, 0},
		{"single callback field reader comparison", `v:=0; s:=struct{F func()}{func(){_=v}}; for range xs {go func(){_=s.F==nil}(); v++}`, 0},
		{"single callback field invocation", `v:=0; s:=struct{F func()}{func(){v++}}; for range xs {go func(){s.F()}()}`, 1},
		{"direct callback field invocation", `v:=0; s:=struct{F func()}{func(){v++}}; for range xs {go s.F()}`, 1},
		{"retained callback field argument", `v:=0; s:=struct{F func()}{func(){v++}}; for range xs {go use(s.F)}`, 1},
		{"callback copying does not invoke", `v:=0; s:=struct{F func()}{func(){v++}}; for range xs {go func(){f:=s.F; _=f==nil}()}`, 0},
		{"mixed same variable preserves pointer read", `v:=0; s:=struct{P *int; F func()}{&v,func(){v++}}; for range xs {go func(){_=*s.P; _=s.F==nil}(); v++}`, 1},
		{"mixed same variable keeps callback write latent", `v:=0; s:=struct{P *int; F func()}{&v,func(){v++}}; for range xs {go func(){_=*s.P; _=s.F==nil}()}`, 0},
		{"mixed pointer read via argument", `v:=0; s:=struct{P *int; F func()}{&v,func(){v++}}; for range xs {go use(s.P); v++}`, 1},
		{"function pointer invocation", `v:=0; f:=func(){v++}; p:=&f; for range xs {go (*p)()}`, 1},
		{"function pointer reader", `var i int; f:=func(){_=i}; p:=&f; for i=range xs {go (*p)()}`, 1},
		{"captured function pointer invocation", `v:=0; f:=func(){v++}; p:=&f; for range xs {go func(){(*p)()}()}`, 1},
		{"captured function pointer reader", `var i int; f:=func(){_=i}; p:=&f; for i=range xs {go func(){(*p)()}()}`, 1},
		{"function pointer pointer invocation", `v:=0; f:=func(){v++}; p:=&f; pp:=&p; for range xs {go (**pp)()}`, 1},
		{"captured function pointer pointer invocation", `v:=0; f:=func(){v++}; p:=&f; pp:=&p; for range xs {go func(){(**pp)()}()}`, 1},
		{"declared function pointer pointer invocation", `v:=0; var f func()=func(){v++}; var p *func()=&f; var pp **func()=&p; for range xs {go (**pp)()}`, 1},
		{"function pointer comparison", `v:=0; f:=func(){v++}; p:=&f; for range xs {go func(){_=(*p)==nil}()}`, 0},
		{"function pointer reader comparison", `v:=0; f:=func(){_=v}; p:=&f; for range xs {go func(){_=(*p)==nil}(); v++}`, 0},
		{"function pointer pointer comparison", `v:=0; f:=func(){v++}; p:=&f; pp:=&p; for range xs {go func(){_=(**pp)==nil}()}`, 0},
		{"synchronous function pointer mutation", `for range xs {v:=0; f:=func(){v++}; p:=&f; go func(){_=v}(); (*p)()}`, 1},
		{"synchronous function pointer pointer mutation", `for range xs {v:=0; f:=func(){v++}; p:=&f; pp:=&p; go func(){_=v}(); (**pp)()}`, 1},
		{"function pointer current safe contents", `v:=0; f:=func(){v++}; p:=&f; f=func(){}; for range xs {go (*p)()}`, 0},
		{"loaded function snapshot keeps old contents", `v:=0; f:=func(){v++}; p:=&f; loaded:=*p; f=func(){}; for range xs {go loaded()}`, 1},
		{"loaded function snapshot drops old contents", `v:=0; f:=func(){v++}; p:=&f; f=func(){}; loaded:=*p; for range xs {go loaded()}`, 0},
		{"function pointer does not invoke compared capture", `v:=0; g:=func(){v++}; f:=func(){_=g==nil}; p:=&f; for range xs {go (*p)()}`, 0},
		{"captured function pointer does not invoke compared capture", `v:=0; g:=func(){v++}; f:=func(){_=g==nil}; p:=&f; for range xs {go func(){(*p)()}()}`, 0},
		{"field pointer invocation", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s.F; for range xs {go (*p)()}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopCallableModesRemainLatentUntilInvoked(t *testing.T) {
	v, reference := &loopVariable{}, &loopVariable{}
	lit := &ast.FuncLit{}
	s := &loopScanner{index: &loopIndex{captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{
		lit: {v: loopRead | loopHeaderRead | loopWrite | loopIndirectRead | loopIndirectWrite | loopInvoke, reference: loopRetain},
	}}}
	w := &loopWalk{}
	stored := s.value(lit, w)
	want := loopRetain | loopCallableExecMask<<loopCallableShift
	if stored[v] != want || stored[reference] != loopRetain {
		t.Fatalf("storage executed a capture or lost a reference: %v", stored)
	}
	invoked := s.invokeValue(lit, w)
	if invoked[v] != loopRetain|loopCallableExecMask || invoked[reference] != loopRetain {
		t.Fatalf("invocation did not activate exactly the executable modes: %v", invoked)
	}
	if stored[v] != want || s.index.captures[lit][v] != loopCallableExecMask {
		t.Fatal("activation mutated stored values or ordinary capture summaries")
	}
}

func TestLoopCallableExpansionKeepsMixedDataProvenance(t *testing.T) {
	v, aggregate := &loopVariable{}, &loopVariable{}
	w := &loopWalk{values: map[*loopVariable]loopValue{
		aggregate: {v: loopRead | loopRetain | loopWrite<<loopCallableShift},
	}}
	s := &loopScanner{}
	before := loopCloneValues(w.values)
	copied := s.expand(loopValue{aggregate: loopRead}, w)
	if copied[v]&loopRead == 0 || copied[v]&loopWrite != 0 || copied[v]&(loopWrite<<loopCallableShift) == 0 {
		t.Fatalf("aggregate read lost data or executed its callback: %v", copied)
	}
	called := s.expand(loopValue{aggregate: loopRead | loopInvoke}, w)
	if called[v]&(loopRead|loopWrite) != loopRead|loopWrite {
		t.Fatalf("aggregate invocation lost executable/data modes: %v", called)
	}
	if !reflect.DeepEqual(w.values, before) {
		t.Fatal("expansion mutated immutable bindings")
	}
}

func TestLoopCallableExpansionRevisitsNewInvocationModes(t *testing.T) {
	f, g, v := &loopVariable{}, &loopVariable{}, &loopVariable{}
	w := &loopWalk{values: map[*loopVariable]loopValue{
		f: {v: loopWrite << loopCallableShift},
		g: {f: loopRead | loopInvoke},
	}}
	s := &loopScanner{}
	// Map traversal order must not matter if f is first visited as a copy,
	// then invoked by the expansion of g. A single visited bit is unsound.
	for i := 0; i < 100; i++ {
		got := s.expand(loopValue{f: loopRead, g: loopRead}, w)
		if got[v]&loopWrite == 0 {
			t.Fatalf("late invocation was skipped: %v", got)
		}
	}
}
