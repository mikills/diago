package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
	"testing"
)

var loopOriginTransferCases = []struct {
	name, body string
	want       int
}{
	{"indirectInstall", `func(){f:=func(){};defer func(){p:=&f;*p=func(){go func(){use(v)}()};f()}()}()`, 1},
	{"indirectKill", `func(){f:=func(){go func(){use(v)}()};defer func(){p:=&f;*p=func(){};f()}()}()`, 0},
	{"opaqueCallAddress", `func(){f:=func(){go func(){use(v)}()};defer func(){get:=func()*func(){return &f};p:=get();*p=func(){};f()}()}()`, 1},
	{"heapFieldAddress", `func(){f:=func(){go func(){use(v)}()};defer func(){s:=struct{P *func()}{&f};*s.P=func(){};f()}()}()`, 1},
	{"copiedAddress", `func(){f:=func(){};defer func(){p:=&f;q:=p;*q=func(){go func(){use(v)}()};f()}()}()`, 1},
	{"mixedCellAndFieldProof", `func(){f:=func(){};defer func(){s:=struct{F func()}{func(){}};p:=&f;if len(xs)==2 {p=&s.F};*p=func(){go func(){use(v)}()};f()}()}()`, 1},
	{"simultaneousStore", `func(){f,g:=func(){},func(){};defer func(){p:=&f;p,*p=&g,func(){go func(){use(v)}()};f()}()}()`, 1},
	{"yieldKey", `func(){defer func(){for f:=range func(y func(func())bool){y(func(){go func(){use(v)}()})}{f()}}()}()`, 1},
	{"yieldSecond", `func(){defer func(){for _,f:=range func(y func(int,func())bool){y(0,func(){go func(){use(v)}()})}{f()}}()}()`, 1},
	{"yieldBoth", `func(){defer func(){for f,g:=range func(y func(func(),func())bool){y(func(){},func(){go func(){use(v)}()})}{f();g()}}()}()`, 1},
	{"yieldSlotsSeparated", `func(){defer func(){for _,g:=range func(y func(func(),func())bool){y(func(){go func(){use(v)}()},func(){})}{g()}}()}()`, 0},
	{"yieldTwoAssignments", `func(){defer func(){f,g:=func(){},func(){};for f,g=range func(y func(func(),func())bool){y(func(){},func(){go func(){use(v)}()})}{f();g()}}()}()`, 1},
	{"yieldSimultaneousDestinations", `func(){defer func(){f,g:=func(){},func(){};p:=&f;for p,*p=range func(y func(*func(),func())bool){y(&g,func(){go func(){use(v)}()})}{f()}}()}()`, 1},
	{"yieldAssignment", `func(){defer func(){f:=func(){};for f=range func(y func(func())bool){y(func(){go func(){use(v)}()})}{f()}}()}()`, 1},
	{"yieldAssignmentKills", `func(){defer func(){f:=func(){go func(){use(v)}()};for f=range func(y func(func())bool){y(func(){})}{f()}}()}()`, 0},
	{"yieldBodyOverwrite", `func(){defer func(){for f:=range func(y func(func())bool){y(func(){go func(){use(v)}()})}{f=func(){};f()}}()}()`, 0},
	{"yieldFalseCleanup", `func(){defer func(){for f:=range func(y func(func())bool){if !y(func(){go func(){use(v)}()}) {v++}}{f();break}}()}()`, 1},
	{"yieldFalseKillCleanup", `func(){defer func(){for f:=range func(y func(func())bool){if !y(func(){go func(){use(v)}()}) {v++}}{f=func(){};f();break}}()}()`, 0},
	{"yieldFreshBody", `func(){defer func(){for f:=range func(y func(func())bool){y(func(){go func(){use(v)}()});y(func(){})}{f=func(){};defer func(){f()}()}}()}()`, 0},
	{"yieldSavedBody", `func(){defer func(){for f:=range func(y func(func())bool){y(func(){go func(){use(v)}()});y(func(){})}{defer f()}}()}()`, 1},
	{"yieldAddressProof", `func(){defer func(){f:=func(){};for p:=range func(y func(*func())bool){y(&f)}{*p=func(){go func(){use(v)}()};f()}}()}()`, 1},
}

func TestLoopCallableOriginTransfers(t *testing.T) {
	for _, tc := range loopOriginTransferCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					source := loopTestPrelude + `func work(xs []int){for range xs{v:=0;` + tc.body + `;v++}}`
					got := analyzeLoopRegression(t, source, "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopCallableOriginTransfersPublicCompleteInfo(t *testing.T) {
	var source strings.Builder
	source.WriteString("package origins\nfunc use(...any){}\n")
	for _, tc := range loopOriginTransferCases {
		fmt.Fprintf(&source, "func %s(xs []int){for range xs{v:=0;%s;v++}}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/origins\n\ngo 1.23\n", "origins.go": source.String()})
	got, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range got {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("public fixture must have complete types: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for _, tc := range loopOriginTransferCases {
		if counts[tc.name] != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, counts[tc.name], tc.want)
		}
	}
}

func TestLoopCallableOriginStoreWeakProofs(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(fmt.Sprint(multiple), func(t *testing.T) {
			z := newDeferredFixture(t, `func work(){v:=0;f:=func(){};g:=func(){};p:=&f;*p=func(){use(v)}}`)
			for _, node := range z.body.List[:4] {
				z.step(node)
			}
			f, g, p, v := z.vars["f"], z.vars["g"], z.vars["p"], z.vars["v"]
			store := z.body.List[4].(*ast.AssignStmt)
			value := z.s.value(store.Rhs[0], z.w)
			old := loopEpochAgeValue(value, map[*loopVariable]bool{v: true})
			z.w.values[f], z.w.values[g] = old, old
			proofs := loopValue{f: loopRead | loopPriorRef | loopEpochMixedRef}
			if multiple {
				proofs = loopValue{f: loopRead, g: loopRead}
			}
			z.w.values[z.f.aliasSlot(p)] = proofs
			before := deepDeferredBindings(z.w.values)
			origin := z.d.origins(store.Rhs[0], z.w)
			z.step(store)
			for target := range proofs {
				want := loopValue{}
				loopMerge(want, before[z.d.companion(target)])
				loopMerge(want, origin)
				if !reflect.DeepEqual(z.w.values[z.d.companion(target)], want) {
					t.Fatal("weak store killed an origin alternative")
				}
				if z.w.values[target][v]&loopEpochMixedRef == 0 {
					t.Fatal("weak store lost mixed current/prior contents")
				}
			}
			if !reflect.DeepEqual(old, before[f]) {
				t.Fatal("weak store mutated shared contents")
			}
		})
	}
}

func TestLoopCallableOriginAggregateStoreRemainsWeak(t *testing.T) {
	for _, source := range []string{
		`func work(){v:=0;s:=struct{F func()}{func(){use(v)}};p:=&s.F;*p=func(){}}`,
		`func work(){v:=0;s:=[1]func(){func(){use(v)}};p:=&s[0];*p=func(){}}`,
	} {
		z := newDeferredFixture(t, source)
		for _, node := range z.body.List[:3] {
			z.step(node)
		}
		s := z.vars["s"]
		before := deepDeferredBindings(z.w.values)
		z.step(z.body.List[3])
		if !reflect.DeepEqual(z.w.values[s], before[s]) || !reflect.DeepEqual(z.w.values[z.d.companion(s)], before[z.d.companion(s)]) {
			t.Fatal("aggregate member store strongly replaced its root or invented a callable root origin")
		}
	}
}

func TestLoopCallableOriginStoreEpochs(t *testing.T) {
	for _, mode := range []loopAccess{loopRead, loopRead | loopPriorRef, loopRead | loopPriorRef | loopEpochMixedRef} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			z := newDeferredFixture(t, `func work(){v:=0;f:=func(){use(v)};p:=&f;*p=func(){}}`)
			for _, node := range z.body.List[:3] {
				z.step(node)
			}
			f, p := z.vars["f"], z.vars["p"]
			z.w.values[z.f.aliasSlot(p)] = loopValue{f: mode}
			before := deepDeferredBindings(z.w.values)
			store := z.body.List[3].(*ast.AssignStmt)
			old := z.w.values[z.d.companion(f)]
			fresh := z.d.origins(store.Rhs[0], z.w)
			z.step(store)
			got := z.w.values[z.d.companion(f)]
			want := loopValue{}
			if mode&loopPriorRef != 0 {
				loopMerge(want, old)
			}
			if loopEpochCurrentMode(mode) != 0 {
				loopMerge(want, fresh)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("origins=%v want %v", got, want)
			}
			if mode&loopPriorRef == 0 && len(z.w.values[f]) != 0 {
				t.Fatalf("strong noop store retained old callable contents: %v", z.w.values[f])
			}
			if mode&loopPriorRef != 0 && !reflect.DeepEqual(z.w.values[f], before[f]) {
				t.Fatalf("old/mixed store killed retained contents")
			}
			if before[z.f.aliasSlot(p)][f] != mode {
				t.Fatal("store mutated shared address proof")
			}
		})
	}
}
