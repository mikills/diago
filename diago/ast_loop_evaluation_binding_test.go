package diago

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var loopEvaluationBindings = []struct {
	name, body string
	want       int
}{
	{"mapRHS", `v:=0;f:=func(){v++};m:=map[int]func(){};defer func(){m,_=map[int]func(){0:f},func()int{f=func(){};return 0}();m[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"mapTemporary", `v:=0;f:=func(){v++};defer func(){saved:=map[int]func(){0:f};f=func(){};m:=saved;m[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"arrayRHS", `v:=0;f:=func(){v++};defer func(){a,_:=[1]func(){f},func()int{f=func(){};return 0}();a[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"structRHS", `v:=0;f:=func(){v++};defer func(){a,_:=struct{F func()}{f},func()int{f=func(){};return 0}();a.F()}();for range xs{go func(){use(v)}()}`, 1},
	{"mapRHSDoesNotReload", `v:=0;f:=func(){};defer func(){a,_:=map[int]func(){0:f},func()int{f=func(){v++};return 0}();a[0]()}();for range xs{go func(){use(v)}()}`, 0},
	{"arrayRHSOrdinaryLoadMayBeLate", `v:=0;f:=func(){};defer func(){a,_:=[1]func(){f},func()int{f=func(){v++};return 0}();a[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"structRHSOrdinaryLoadMayBeLate", `v:=0;f:=func(){};defer func(){a,_:=struct{F func()}{f},func()int{f=func(){v++};return 0}();a.F()}();for range xs{go func(){use(v)}()}`, 1},
	{"structNestedMapDoesNotReload", `v:=0;f:=func(){};defer func(){a,_:=struct{M map[int]func()}{map[int]func(){0:f}},func()int{f=func(){v++};return 0}();a.M[0]()}();for range xs{go func(){use(v)}()}`, 0},
	{"arrayNestedMapDoesNotReload", `v:=0;f:=func(){};defer func(){a,_:=[1]map[int]func(){map[int]func(){0:f}},func()int{f=func(){v++};return 0}();a[0][0]()}();for range xs{go func(){use(v)}()}`, 0},
	{"mapPointerKey", `defer func(){for range 2{v,u:=0,0;p:=&v;m,_:=map[*int]int{p:0},func()int{p=&u;return 0}();go use(m);v++}}()`, 1},
	{"mapPointerKeyDoesNotReload", `defer func(){for range 2{v,u:=0,0;p:=&u;m,_:=map[*int]int{p:0},func()int{p=&v;return 0}();go use(m);v++}}()`, 0},
	{"nestedAggregateRHS", `v:=0;f:=func(){v++};defer func(){a,_:=map[int][1]func(){0:{f}},func()int{f=func(){};return 0}();a[0][0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"blankFirst", `v:=0;f:=func(){v++};defer func(){_,a,_:=0,map[int]func(){0:f},func()int{f=func(){};return 0}();a[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"valueSpec", `v:=0;f:=func(){v++};defer func(){var a,_=map[int]func(){0:f},func()int{f=func(){};return 0}();a[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"aggregateOverwrite", `v:=0;f:=func(){v++};defer func(){a,_:=map[int]func(){0:f},func()int{f=func(){};return 0}();a=map[int]func(){0:func(){}};a[0]()}();for range xs{go func(){use(v)}()}`, 0},
	{"savedOrigin", `v:=0;f:=func(){v++};defer func(){g,_:=f,func()int{f=func(){};return 0}();g()}();for range xs{go func(){use(v)}()}`, 1},
	{"originOverwrite", `v:=0;f:=func(){v++};defer func(){g,_:=f,func()int{f=func(){};return 0}();g=func(){};g()}();for range xs{go func(){use(v)}()}`, 0},
	{"savedAddress", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){q,_:=p,func()int{p=&g;return 0}();*q=func(){v++};f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unorderedAddressOtherCell", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){q,_:=p,func()int{p=&g;return 0}();*q=func(){v++};g()}();for range xs{go func(){use(v)}()}`, 1},
	{"savedAddressStrongOverwrite", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){q,_:=p,func()int{p=&g;return 0}();q=&f;*q=func(){v++};g()}();for range xs{go func(){use(v)}()}`, 0},
	{"unorderedOriginInstalled", `v:=0;f:=func(){};defer func(){g,_:=f,func()int{f=func(){v++};return 0}();g()}();for range xs{go func(){use(v)}()}`, 1},
	{"unorderedCompositeLoad", `v:=0;f:=func(){};defer func(){a:=[]func(){f,func()func(){f=func(){v++};return nil}()};a[0]()}();for range xs{go func(){use(v)}()}`, 1},
	{"savedLHSAddress", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){*p,_=func(){v++},func()int{p=&g;return 0}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"deferAggregateArgument", `v:=0;f:=func(){v++};defer func(){defer func(a map[int]func(),_ int){a[0]()}(map[int]func(){0:f},func()int{f=func(){};return 0}())}();for range xs{go func(){use(v)}()}`, 1},
	{"deferVariadic", `v:=0;f:=func(){v++};defer func(){defer func(a ...func()){a[0]()}(f,func()func(){f=func(){};return nil}())}();for range xs{go func(){use(v)}()}`, 1},
	{"deferEllipsis", `v:=0;f:=func(){v++};defer func(){a,_:=[]func(){f},func()int{f=func(){};return 0}();defer func(a ...func()){a[0]()}(a...)}();for range xs{go func(){use(v)}()}`, 1},
	{"opaqueLiveCell", `defer func(){for range 2{v,u:=0,0;p:=&u;go readPair(func()int{p=&v;return 0}(),&p);v++}}()`, 1},
	{"opaqueLiveCellBeforeMutation", `defer func(){for range 2{v,u:=0,0;p:=&u;go readPairCell(&p,func()int{p=&v;return 0}());v++}}()`, 1},
	{"opaqueSafeRebind", `defer func(){for range 2{v,u:=0,0;p:=&v;go readPair(func()int{p=&u;return 0}(),&p);v++}}()`, 0},
	{"opaquePreCall", `defer func(){for range 2{v,u:=0,0;p:=&u;func(){p=&v}();go readPair(0,&p);v++}}()`, 1},
	{"opaqueUnorderedPointer", `defer func(){for range 2{v,u:=0,0;p:=&u;go readPointer(p,func()int{p=&v;return 0}());v++}}()`, 1},
	{"opaqueCopiedPointer", `defer func(){for range 2{v,u:=0,0;p:=&u;saved:=p;go readPointer(saved,func()int{p=&v;return 0}());v++}}()`, 0},
	{"opaqueCopiedBadPointer", `defer func(){for range 2{v,u:=0,0;p:=&v;go readPointer(p,func()int{p=&u;return 0}());v++}}()`, 1},
	{"opaqueCopiedFunction", `defer func(){for range 2{v:=0;f:=func(_ int){use(v)};go f(func()int{f=func(int){};return 0}());v++}}()`, 1},
	{"opaqueCopiedSafeFunction", `defer func(){for range 2{v:=0;f:=func(int){};go f(func()int{f=func(int){use(v)};return 0}());v++}}()`, 0},
	{"opaqueCallbackArgument", `defer func(){for range 2{v:=0;f:=func(){use(v)};go readCallback(f,func()int{f=func(){};return 0}());v++}}()`, 1},
	{"opaqueUnorderedCallbackArgument", `defer func(){for range 2{v:=0;f:=func(){};go readCallback(f,func()int{f=func(){use(v)};return 0}());v++}}()`, 1},
	{"opaqueSafeCallbackArgument", `defer func(){for range 2{v:=0;f:=func(){};saved:=f;go readCallback(saved,func()int{f=func(){use(v)};return 0}());v++}}()`, 0},
	{"opaqueAggregateArgument", `defer func(){for range 2{v:=0;f:=func(){use(v)};go readCallbacks(map[int]func(){0:f},func()int{f=func(){};return 0}());v++}}()`, 1},
	{"opaqueUnusedPointerStillMayRetain", `defer func(){for range 2{v,u:=0,0;p:=&u;go unusedPair(func()int{p=&v;return 0}(),&p);v++}}()`, 1},
	{"opaqueCallResultNotInferred", `defer func(){for range 2{v:=0;go readPointer(func()*int{return &v}(),0);v++}}()`, 0},
	{"opaqueCallbackResultNotInferred", `defer func(){for range 2{v:=0;go readCallback(func()func(){return func(){use(v)}}(),0);v++}}()`, 0},
	{"knownLiveCell", `defer func(){for range 2{v,u:=0,0;p:=&u;go func(_ int,pp **int){use(**pp)}(func()int{p=&v;return 0}(),&p);v++}}()`, 1},
	{"knownSafeRebind", `defer func(){for range 2{v,u:=0,0;p:=&v;go func(_ int,pp **int){use(**pp)}(func()int{p=&u;return 0}(),&p);v++}}()`, 0},
	{"unorderedIndexLoad", `v:=0;a:=[1]func(){func(){v++}};defer func(){g:=a[func()int{a=[1]func(){func(){}};return 0}()];g()}();for range xs{go func(){use(v)}()}`, 1},
}

const loopEvaluationPrelude = `package evaluation
func use(...any){}
func readPair(_ int,pp **int){use(**pp)}
func unusedPair(_ int,_ **int){}
func readPairCell(pp **int,_ int){use(**pp)}
func readPointer(p *int,_ int){use(*p)}
func readCallback(f func(),_ int){f()}
func readCallbacks(m map[int]func(),_ int){m[0]()}
`

func TestLoopEvaluationBindingModes(t *testing.T) {
	for _, tc := range loopEvaluationBindings {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopEvaluationPrelude+"func work(xs []int){"+tc.body+"}", "go1.24", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopEvaluationBindingPublicCompleteTypes(t *testing.T) {
	var source strings.Builder
	source.WriteString(loopEvaluationPrelude)
	for _, tc := range loopEvaluationBindings {
		fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/evaluation\n\ngo 1.24\n", "evaluation.go": source.String()})
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile fixtures: %v\n%s", err, out)
	}
	got, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range got {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("complete public type information required: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for _, tc := range loopEvaluationBindings {
		if counts[tc.name] != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, counts[tc.name], tc.want)
		}
	}
}

func TestLoopEvaluationSnapshotLatticesAndIdentity(t *testing.T) {
	for _, mode := range []loopAccess{loopRead, loopRead | loopPriorRef, loopRead | loopPriorRef | loopEpochMixedRef} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};p:=&f;g:=func(){};q:=&g;g,q=f,p}`)
			z.f.deferred = z.d
			for _, node := range z.body.List[:len(z.body.List)-1] {
				z.step(node)
			}
			node := z.body.List[len(z.body.List)-1]
			f, p, g, q := z.vars["f"], z.vars["p"], z.vars["g"], z.vars["q"]
			z.w.values[z.f.aliasSlot(p)] = loopValue{f: mode}
			value, origin, address := z.w.values[f], z.w.values[z.d.companion(f)], z.w.values[z.f.aliasSlot(p)]
			var before, after bytes.Buffer
			if err := format.Node(&before, token.NewFileSet(), z.body); err != nil {
				t.Fatal(err)
			}
			z.f.evaluateDeferredCalls(node, z.w, nil)
			literals, companions, aliases := len(z.d.literals), len(z.d.companions), len(z.f.aliases)
			z.f.evaluateDeferredCalls(node, z.w, nil)
			if len(z.d.literals) != literals || len(z.d.companions) != companions || len(z.f.aliases) != aliases {
				t.Fatal("repeated emission lost interning")
			}
			z.w.values[f], z.w.values[p] = nil, nil
			z.w.values[z.d.companion(f)], z.w.values[z.f.aliasSlot(p)] = nil, nil
			z.f.transferDeferredOrigins(node, z.w)
			z.f.transfer(node, z.w, &loopFlowAST{})
			if !reflect.DeepEqual(z.w.values[g], value) || !reflect.DeepEqual(z.w.values[z.d.companion(g)], origin) || !reflect.DeepEqual(z.w.values[z.f.aliasSlot(q)], address) {
				t.Fatal("transfer reloaded a source instead of consuming all three emitted lattices")
			}
			if err := format.Node(&after, token.NewFileSet(), z.body); err != nil {
				t.Fatal(err)
			}
			if before.String() != after.String() {
				t.Fatal("expression evaluation mutated the AST")
			}
		})
	}
}

func TestLoopEvaluationSnapshotIndependentOfEnvironmentSize(t *testing.T) {
	z := newDeferredFixture(t, `func work(){v:=0;f:=func(){v++};a:=[1]func(){f};_=a}`)
	z.f.deferred = z.d
	z.steps()
	node := z.body.List[2]
	small := testing.AllocsPerRun(20, func() { z.f.evaluateDeferredCalls(node, z.w, nil) })
	for i := 0; i < 3200; i++ {
		z.w.values[&loopVariable{}] = loopValue{}
	}
	large := testing.AllocsPerRun(20, func() { z.f.evaluateDeferredCalls(node, z.w, nil) })
	if large > small+2 {
		t.Fatalf("emission copied the full environment: small=%g large=%g", small, large)
	}
	if n := len(z.w.operands[nil].expressions); n > 20 {
		t.Fatalf("unrelated bindings entered the expression cache: %d", n)
	}
}

func TestLoopEvaluationBindingGoSemantics(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/evaluationsemantics\n\ngo 1.24\n",
		"semantics_test.go": `package evaluation
import "testing"
func TestCopiesAndCells(t *testing.T){
 n:=0;f:=func(){n++};m:=map[int]func(){}
 m,_=map[int]func(){0:f},func()int{f=func(){};return 0}();m[0]()
 if n!=1{t.Fatalf("map copied callback: %d",n)}
 a,_:=[1]func(){func(){n++}},func()int{f=func(){};return 0}();a[0]()
 s,_:=struct{F func()}{func(){n++}},func()int{f=func(){};return 0}();s.F()
 f=func(){n++}
 func(){defer func(cb func()){cb()}(f);f=func(){}}()
 if n!=4{t.Fatalf("aggregate/parameter copies: %d",n)}
 v,u:=1,2;p:=&u
 func(){defer func(pp **int){if **pp!=1{t.Fatal("saved cell reloaded stale pointer")}}(&p);p=&v}()
 p=&u
 func(){defer func(q *int){if *q!=2{t.Fatal("saved pointer was reloaded")}}(p);p=&v}()
 p=&u;ch:=make(chan int,1)
 go func(_ int,pp **int){ch<-**pp}(func()int{p=&v;return 0}(),&p)
 if got:=<-ch;got!=1{t.Fatalf("launch-time live cell: %d",got)}
}
`,
	})
	cmd := exec.Command("go", "test", "-race", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Go copy/cell semantics: %v\n%s", err, out)
	}
}
