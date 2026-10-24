package diago

import (
	"go/ast"
	"go/token"
	"reflect"
	"testing"
)

func TestUninvokedCapturedFunction(t *testing.T) {
	for _, body := range []string{
		`v:=0; f:=func(){v++}; for range xs {go func(){_ = f}()}`,
		`v:=0; f:=func(){v++}; for range xs {go func(){use(f==nil)}()}`,
	} {
		for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
			got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+body+"}", "1.22", mode)
			if countRules(got)["goroutine-in-loop"] != 0 {
				t.Errorf("%s: uninvoked function: %+v", mode, got)
			}
		}
	}
}

func TestLoopCaptureEffects(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"uninvoked nested writer", `v:=0; for range xs {go func(){f:=func(){v++}; _=f}()}`, 0},
		{"discarded nested writer", `v:=0; for range xs {go func(){_=func(){v++}}()}`, 0},
		{"uninvoked nested reader of reused range slot", `var v int; for _,v=range xs {go func(){f:=func(){_=v}; _=f}()}`, 0},
		{"uninvoked nested write only capture", `var v int; _=v; for _,v=range xs {go func(){f:=func(){v=1}; _=f}()}`, 0},
		{"unused callback does not poison sibling reader", `v:=0; for range xs {go func(){f:=func(){v++}; _=f}(); go func(){_=v}()}`, 0},
		{"unreachable literal write", `v:=0; for range xs {go func(){return; v++}()}`, 0},
		{"unreachable literal read", `var v int; for _,v=range xs {go func(){return; _=v}()}`, 0},
		{"unreachable literal write only", `var v int; _=v; for _,v=range xs {go func(){return; v=1}()}`, 0},
		{"goto skips literal write", `v:=0; for range xs {go func(){goto done; v++; done: use()}()}`, 0},
		{"unreachable nested invocation", `v:=0; for range xs {go func(){f:=func(){v++}; _=f; return; f()}()}`, 0},
		{"unreachable nested launch", `v:=0; for range xs {go func(){return; go func(){v++}()}()}`, 0},
		{"reachable literal write preserved", `v:=0; for range xs {go func(){v++; return; _=v}()}`, 1},
		{"invoked nested writer", `v:=0; for range xs {go func(){f:=func(){v++}; f()}()}`, 1},
		{"invoked nested write only", `v:=0; _=v; for range xs {go func(){f:=func(){v=1}; f()}()}`, 1},
		{"immediately invoked nested writer", `v:=0; for range xs {go func(){(func(){v++})()}()}`, 1},
		{"deep synchronous invocation", `v:=0; for range xs {go func(){f:=func(){g:=func(){v++}; g()}; f()}()}`, 1},
		{"deep uninvoked writer", `v:=0; for range xs {go func(){f:=func(){g:=func(){v++}; _=g}; f()}()}`, 0},
		{"captured callback current binding", `v:=0; for range xs {f:=func(){v++}; g:=func(){f()}; f=func(){}; go g()}`, 0},
		{"captured callback still invoked", `v:=0; for range xs {f:=func(){v++}; g:=func(){f()}; go g()}`, 1},
		{"known nested asynchronous writer", `v:=0; for range xs {go func(){f:=func(){v++}; go f()}()}`, 2},
		{"known nested true capture", `var v int; for _,v=range xs {go func(){go func(){_=v}()}()}`, 2},
		{"fresh nested exclusive writer", `for range xs {v:=0; go func(){go func(){v++}()}()}`, 0},
		{"known synchronous mutator after launch", `for range xs {v:=0; f:=func(){g:=func(){v++}; g()}; go func(){_=v}(); f()}`, 1},
		{"uninvoked mutator after launch", `for range xs {v:=0; f:=func(){g:=func(){v++}; _=g}; go func(){_=v}(); f()}`, 0},
		{"unknown go pointer argument", `v:=0; for range xs {go use(&v); v++}`, 1},
		{"unknown go reused pointer argument", `var v int; for _,v=range xs {go use(&v)}`, 1},
		{"ordinary reused read", `var v int; for _,v=range xs {go func(){_=v}()}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					findings := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "go1.22", mode)
					if got := countRules(findings)["goroutine-in-loop"]; got != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", got, tc.want, findings)
					}
				})
			}
		})
	}
}

func TestLoopCaptureEffectsResetRetainsReferencesWithoutLoadingSlots(t *testing.T) {
	outer, inner := &ast.FuncLit{}, &ast.FuncLit{}
	v, p := &loopVariable{}, &loopVariable{}
	index := &loopIndex{captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{
		outer: {v: loopRead | loopWrite, p: loopWrite},
		inner: {v: loopRead | loopWrite, p: loopWrite},
	}}
	s := &loopScanner{index: index}
	s.resetCaptureEffects()
	for lit, captures := range index.captures {
		if len(captures) != 2 || captures[v] != loopRetain || captures[p] != loopRetain {
			t.Fatalf("lost lexical keys or kept executable modes for %p: %v", lit, captures)
		}
	}
	// The old contents of a write-only captured pointer/function slot must not
	// be followed just because its reference survives a binding join.
	w := &loopWalk{values: map[*loopVariable]loopValue{p: {v: loopWrite}}}
	value := s.expand(s.value(outer, w), w)
	if value[p] != loopRetain || value[v] != loopRetain {
		t.Fatalf("retention loaded a slot's old value: %v", value)
	}
	joined := map[*loopVariable]loopValue{}
	if !loopJoinBindings(joined, map[*loopVariable]loopValue{p: {v: loopRetain}}) || joined[p][v] != loopRetain {
		t.Fatal("binding join dropped a retention-only key")
	}
	if loopJoinBindings(joined, map[*loopVariable]loopValue{p: {v: loopRetain}}) {
		t.Fatal("unchanged retention-only binding did not converge")
	}
	// Reachable assignment adds a write but still does not read old contents.
	index.direct = []loopDirectAccess{{owner: outer, variable: p, mode: loopWrite}}
	if !s.updateCaptureEffects() || index.captures[outer][p] != loopRetain|loopWrite {
		t.Fatal("reachable write-only capture was not restored")
	}
	if got := s.expand(s.value(outer, w), w); got[v] != loopRetain {
		t.Fatalf("write-only capture loaded old contents: %v", got)
	}
}

func TestLoopCaptureEffectsUpdatesOnlyOwningFrame(t *testing.T) {
	outer, inner := &ast.FuncLit{}, &ast.FuncLit{}
	v := &loopVariable{}
	local := &loopVariable{owner: inner}
	synthetic := &loopVariable{} // lexical reference, executed by a known local callee
	indirect := &loopVariable{}  // reached only through another captured binding
	index := &loopIndex{captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{
		outer: {v: loopRead | loopWrite}, inner: {v: loopRead | loopWrite, synthetic: loopWrite},
	}}
	s := &loopScanner{index: index}
	s.resetCaptureEffects()
	index.direct = []loopDirectAccess{
		{owner: inner, variable: v, mode: loopRead | loopWrite},
		{owner: inner, variable: local, mode: loopRead | loopWrite},
		{owner: inner, variable: synthetic, mode: loopWrite, effect: true},
		{owner: inner, variable: indirect, mode: loopWrite, effect: true},
		{variable: v, mode: loopWrite}, // top-level frame is not a literal
	}
	if !s.updateCaptureEffects() {
		t.Fatal("reachable frame effects did not grow")
	}
	if index.captures[outer][v] != loopRetain {
		t.Fatal("inner executable modes leaked lexically into outer frame")
	}
	if index.captures[inner][v] != loopRetain|loopRead|loopWrite || index.captures[inner][synthetic] != loopRetain|loopWrite {
		t.Fatal("reachable direct/synthesized effects were not summarized")
	}
	if _, ok := index.captures[inner][local]; ok {
		t.Fatal("frame's own local became a capture")
	}
	if _, ok := index.captures[inner][indirect]; ok {
		t.Fatal("effect through captured binding was frozen into a lexical capture")
	}
	if s.updateCaptureEffects() {
		t.Fatal("unchanged executable modes did not converge")
	}
	// A later replay observes an actual launch in the outer frame. This, not
	// the nesting relationship, is what makes inner effects executable there.
	s.launches = []loopLaunch{{owner: outer, values: loopValue{v: loopRead | loopWrite, local: loopRetain}}}
	if !s.updateCaptureEffects() || index.captures[outer][v] != loopRetain|loopRead|loopWrite {
		t.Fatal("launched effects were not propagated to their owning frame")
	}
	if _, ok := index.captures[outer][local]; ok {
		t.Fatal("retention-only launch reference became an executable effect")
	}
	index.direct = nil
	s.launches = nil
	if s.updateCaptureEffects() || index.captures[outer][v] != loopRetain|loopRead|loopWrite {
		t.Fatal("summary update was not monotone")
	}
}

func TestLoopCaptureEffectsPreservesOrdinaryLaunchValues(t *testing.T) {
	literal := &ast.FuncLit{}
	v := &loopVariable{}
	id := &ast.Ident{Name: "v"}
	unknown := loopValue{v: loopRead}
	s := &loopScanner{
		index: &loopIndex{
			ids:      map[*ast.Ident]*loopVariable{id: v},
			captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{literal: {v: loopWrite}},
		},
		launches: []loopLaunch{{values: unknown}},
	}
	s.resetCaptureEffects()
	s.updateCaptureEffects()
	if !reflect.DeepEqual(unknown, loopValue{v: loopRead}) {
		t.Fatal("capture refinement cleared ordinary launch argument modes")
	}
	address := &ast.UnaryExpr{Op: token.AND, X: id}
	if got := s.value(address, &loopWalk{}); got[v] != loopRead {
		t.Fatalf("unknown direct go pointer argument lost read semantics: %v", got)
	}
}
