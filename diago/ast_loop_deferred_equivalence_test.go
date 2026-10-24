package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"sort"
	"testing"
)

// Compare against the independent, unfactored slot representation. Both runs
// use the production CFG/runner/projection, but the reference keeps operands in
// ordinary maps and disables all static operand facts. No performance shortcut
// is allowed to change source launch identities, values, or conflict classes.
func loopDeferredRepresentationProof(t *testing.T, body, mode string, factored bool) []string {
	t.Helper()
	s := loopSyncTestScanner(t, body, "1.23", mode)
	c, effects := s.beginFlowConflicts(), s.beginFlowEffects()
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: effects, conflicts: c}
	effects.aliases = f.aliases
	f.deferred = newLoopDeferredFlow(s, f.aliases)
	if !factored {
		disable := func(body *ast.BlockStmt, owner *ast.FuncLit) {
			p := f.prepareDeferredCFG(body, owner).plan
			p.static = nil
			p.persistent = false
		}
		disable(s.rootNode.(*ast.BlockStmt), nil)
		ast.Inspect(s.rootNode, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok {
				disable(lit.Body, lit)
			}
			return true
		})
	}
	f.refine(s.rootNode.(*ast.BlockStmt), nil)
	c.publishContextLaunches()
	hazards := c.finishFlowConflicts()
	var proof []string
	for _, launch := range s.launches {
		proof = append(proof, fmt.Sprintf("go@%d direct=%t siblings=%t copies=%t loop=%t", launch.stmt.Pos(), hazards.direct[launch.stmt], hazards.siblings[launch.stmt], hazards.copies[launch.stmt], hazards.loopCalls[launch.stmt]))
		for v, mode := range launch.values {
			pos := 0
			if v.ident != nil {
				pos = int(v.ident.Pos())
			}
			proof = append(proof, fmt.Sprintf("go@%d value@%d=%x", launch.stmt.Pos(), pos, mode))
		}
	}
	sort.Strings(proof)
	return proof
}

func TestLoopDeferredFactoredRepresentationMatchesSlots(t *testing.T) {
	cases := []string{
		`for range xs{v:=0;func(){defer func(){go func(){v++}()}();if b{use(v)};defer func(){go func(){v++}()}()}()}`,
		`v:=0;f:=func(){v++};for range xs{if b{defer f()}else{defer func(){f()}()};f=func(){};go func(){use(v)}()}`,
		`for range xs{v,u:=0,0;p:=&v;func(){defer func(p *int){go func(){(*p)++}()}(p);if b{p=&u};defer func(){p=&u}()}();use(v)}`,
		`v,u:=0,0;p:=&v;for range xs{defer func(p *int){(*p)++}(p);p=&u;go func(){use(v)}()}`,
		`for range xs{v:=0;func(){if b{defer func(){go func(){v++}()}()}else{defer func(){go func(){v++}()}()}}()}`,
		`for range xs{v:=0;func(){defer func(){go func(){v++}()}();again:defer func(){v++}();if b{goto again}}()}`,
		`for range xs{func()(n int){defer func(){go func(){n++}()}();return}()}`,
		`for range xs{v:=0;for range func(yield func()bool){defer func(){v++}();yield()}{go func(){use(v)}()}}`,
		`for range xs{v:=0;func(){defer func(){v++}();for range func(yield func()bool){defer func(){go func(){use(v)}()}();yield()}{return}}()}`,
		`for range xs{v:=0;func(){for range func(yield func()bool){defer func(){v++}();for range func(yield func()bool){defer func(){go func(){use(v)}()}();yield()}{yield()}}{return}}()}`,
		`v:=0;f:=func(){v++};defer func(){f()}();defer func(){f=func(){}}();for range xs{go func(){use(v)}()}`,
		`v:=0;f:=func(){v++};p:=&f;pp:=&p;defer func(){f()}();defer func(){**pp=func(){}}();for range xs{go func(){use(v)}()}`,
		`for range xs{v:=0;func(){defer func(cb func()){cb()}(func(){go func(){use(v)}()});if b{use(v)}}();v++}`,
		`v:=0;defer func(){v++}();defer func(){for{}}();for range xs{go func(){use(v)}()}`,
		`v:=0;f:=func(){};var recur func();recur=func(){if b{recur();f=func(){v++}}};defer func(){f()}();defer recur();for range xs{go func(){use(v)}()}`,
		`for range xs{v:=0;func(){defer func(_ func(), cb func(), rest ...func()){cb();for _,f:=range rest{f()}}(func(){},func(){go func(){v++}()},func(){use(v)})}()}`,
	}
	for i, body := range cases {
		for _, mode := range []string{"typed", "lexical", "partial"} {
			t.Run(fmt.Sprintf("%d/%s", i, mode), func(t *testing.T) {
				got := loopDeferredRepresentationProof(t, body, mode, true)
				want := loopDeferredRepresentationProof(t, body, mode, false)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("factored representation changed results:\n got: %v\nwant: %v", got, want)
				}
			})
		}
	}
}
