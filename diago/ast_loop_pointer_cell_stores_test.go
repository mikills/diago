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
)

var loopPointerCellStoreCases = []struct {
	name, body string
	want       int
}{
	{"rebindBeforeLoop", `a,b:=0,0;p:=&a;pp:=&p;*pp=&b;for range 2{go func(){use(b)}();*p=1}`, 1},
	{"rebindBody", `for range xs{v,u:=0,0;p:=&u;pp:=&p;*pp=&v;go func(){*p=1}();use(v)}`, 1},
	{"killBeforeLoop", `a,b:=0,0;p:=&a;pp:=&p;*pp=&b;for range 2{go func(){use(a)}();*p=1}`, 0},
	{"killBody", `for range xs{v,u:=0,0;p:=&v;pp:=&p;*pp=&u;go func(){*p=1}();use(v)}`, 0},
	{"snapshotOld", `a,b:=0,0;p:=&a;pp:=&p;r:=*pp;*pp=&b;for range 2{go func(){use(b)}();*r=1}`, 0},
	{"snapshotRetained", `a,b:=0,0;p:=&a;pp:=&p;r:=*pp;*pp=&b;for range 2{go func(){use(a)}();*r=1}`, 1},
	{"doubleWrite", `a,b:=0,0;p:=&a;pp:=&p;*pp=&b;for range 2{go func(){use(b)}();**pp=1}`, 1},
	{"doubleRead", `a,b:=0,0;p:=&a;pp:=&p;*pp=&b;for range 2{go func(){use(**pp)}();b++}`, 1},
	{"doubleReadKilled", `a,b:=0,0;p:=&a;pp:=&p;*pp=&b;for range 2{go func(){use(**pp)}();a++}`, 0},
	{"doubleSnapshot", `a,b:=0,0;p:=&a;pp:=&p;r:=*pp;*pp=&b;for range 2{go func(){use(*r)}();a++}`, 1},
	{"thirdLevel", `a,b:=0,0;p:=&a;pp:=&p;ppp:=&pp;**ppp=&b;for range 2{go func(){use(b)}();***ppp=1}`, 1},
	{"simultaneous", `a,b:=0,0;p,q:=&a,&b;pp:=&p;pp,*pp=&q,&b;for range 2{go func(){use(b)}();*p=1}`, 1},
	{"simultaneousKill", `a,b:=0,0;p,q:=&a,&b;pp:=&p;pp,*pp=&q,&b;for range 2{go func(){use(a)}();*p=1}`, 0},
	{"joinedTarget", `a,b:=0,0;p,q:=&a,&a;pp:=&p;if len(xs)>0{pp=&q};*pp=&b;for range 2{go func(){use(b)}();*p=1}`, 1},
	{"joinedTargetKeepsOld", `a,b:=0,0;p,q:=&a,&a;pp:=&p;if len(xs)>0{pp=&q};*pp=&b;for range 2{go func(){use(a)}();*p=1}`, 1},
	{"branchFutureAlias", `a,b:=0,0;p:=&a;pp:=&p;if len(xs)>0{*pp=&b};for range 2{go func(){use(b)}();*p=1}`, 1},
	{"deepJoinFutureContents", `v:=0;var p *int;pp:=&p;ppp:=&pp;f:=func(){};if len(xs)>0{f=func(){use(p)}}else{**ppp=&v};for range xs{go f();v++}`, 1},
	{"deepJoinFutureAlias", `v:=0;var p *int;pp:=&p;ppp:=&pp;f:=func(){};if len(xs)>0{f=func(){*p=1}}else{**ppp=&v};for range xs{go f();use(v)}`, 1},
	{"loadedStoreAddress", `a,b:=0,0;p:=&a;pp:=&p;r:=*(&pp);*r=&b;for range xs{go func(){use(b)}();*p=1}`, 1},
	{"nilKillsPointer", `v:=0;p:=&v;pp:=&p;*pp=nil;for range xs{go use(p);v++}`, 0},
	{"cellContentsRetention", `for range xs{v:=0;var p *int;pp:=&p;*pp=&v;go use(p);v++}`, 1},
	{"callbackPointerInstall", `v:=0;f,g:=func(){},func(){use(v)};p:=&f;pp:=&p;*pp=&g;for range xs{go (*p)();v++}`, 1},
	{"callbackPointerKill", `v:=0;f,g:=func(){use(v)},func(){};p:=&f;pp:=&p;*pp=&g;for range xs{go (*p)();v++}`, 0},
	{"doubleCallbackInstall", `v:=0;f,g:=func(){},func(){};p:=&f;pp:=&p;*pp=&g;**pp=func(){use(v)};for range xs{go g();v++}`, 1},
	{"doubleCallbackKill", `v:=0;f,g:=func(){},func(){use(v)};p:=&f;pp:=&p;*pp=&g;**pp=func(){};for range xs{go g();v++}`, 0},
	{"deferredRebind", `v:=0;for range xs{func(){u:=0;p:=&u;pp:=&p;defer func(){*pp=&v;go func(){*p=1}()}()}();use(v)}`, 1},
	{"deferredOriginInstall", `for range xs{v:=0;func(){f,g:=func(){},func(){};p:=&f;pp:=&p;defer func(){*pp=&g;**pp=func(){go func(){use(v)}()};g()}()}();v++}`, 1},
	{"deferredOriginKill", `for range xs{v:=0;func(){f,g:=func(){},func(){go func(){use(v)}()};p:=&f;pp:=&p;defer func(){*pp=&g;**pp=func(){};g()}()}();v++}`, 0},
	{"wholeAggregateKill", `v:=0;s:=struct{F func()}{func(){use(v)}};p:=&s;*p=struct{F func()}{func(){}};for range xs{go s.F();v++}`, 0},
	{"wholeAggregateInstall", `v:=0;s:=struct{F func()}{func(){}};p:=&s;*p=struct{F func()}{func(){use(v)}};for range xs{go s.F();v++}`, 1},
	{"aggregateFieldWeak", `v:=0;s:=struct{F,G func()}{func(){use(v)},func(){}};p:=&s.F;*p=func(){};for range xs{go s.F();v++}`, 1},
	{"aggregateIndexWeak", `v:=0;a:=[2]func(){func(){use(v)},func(){}};p:=&a[0];*p=func(){};for range xs{go a[0]();v++}`, 1},
	{"pointerFieldNotRoot", `v:=0;type node struct{Next *node;F func()};n:=node{F:func(){use(v)}};p:=&n;pp:=&p.Next;*pp=&node{};for range xs{go p.F();v++}`, 1},
	{"retainedCallbackNotAddress", `v:=0;f:=func(){use(v)};p:=&struct{F func()}{f};p.F=func(){};for range xs{go f();v++}`, 1},
	{"unknownCallResult", `v:=0;f:=func(){use(v)};get:=func()*func(){return &f};p:=get();*p=func(){};for range xs{go f();v++}`, 1},
}

func TestLoopPointerCellStores(t *testing.T) {
	for _, tc := range loopPointerCellStoreCases {
		t.Run(tc.name, func(t *testing.T) {
			source := loopTestPrelude + "func work(xs []int){" + tc.body + "}"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "cell_stores.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/cells", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, source, "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopPointerCellStoresPublicCompleteTypes(t *testing.T) {
	var source strings.Builder
	source.WriteString("package cells\nfunc use(...any){}\n")
	for _, tc := range loopPointerCellStoreCases {
		fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/cells\n\ngo 1.23\n", "cells.go": source.String()})
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
	for _, tc := range loopPointerCellStoreCases {
		if counts[tc.name] != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, counts[tc.name], tc.want)
		}
	}
}

func TestLoopPointerCellStoreEpochsAndSnapshots(t *testing.T) {
	for _, mode := range []loopAccess{loopRead, loopRead | loopPriorRef, loopRead | loopPriorRef | loopEpochMixedRef} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			z := newDeferredFixture(t, `func work(){a,b:=0,0;p:=&a;pp:=&p;*pp=&b}`)
			for _, node := range z.body.List[:3] {
				z.step(node)
			}
			a, b, p, pp := z.vars["a"], z.vars["b"], z.vars["p"], z.vars["pp"]
			z.w.values[z.f.aliasSlot(pp)] = loopValue{p: mode}
			before := deepDeferredBindings(z.w.values)
			z.step(z.body.List[3])
			want := loopValue{}
			if mode&loopPriorRef != 0 {
				loopMerge(want, before[z.f.aliasSlot(p)])
			}
			if loopEpochCurrentMode(mode) != 0 {
				want[b] = loopRead
			}
			if !reflect.DeepEqual(z.w.values[z.f.aliasSlot(p)], want) {
				t.Fatalf("aliases=%v want %v", z.w.values[z.f.aliasSlot(p)], want)
			}
			if mode&loopPriorRef == 0 && z.w.values[p][a] != 0 {
				t.Fatal("strong pointer store retained stale pointee")
			}
			if before[z.f.aliasSlot(p)][a] != loopRead {
				t.Fatal("store mutated shared snapshot")
			}
			if loopEpochCurrentMode(mode) == 0 && !reflect.DeepEqual(z.w.values[p], before[p]) {
				t.Fatal("old cell overwrote fresh allocation")
			}
		})
	}
}
