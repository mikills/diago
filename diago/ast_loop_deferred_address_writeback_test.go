package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var loopDeferredAddressWritebackCases = []struct {
	name, body string
	want       int
}{
	{"capturedInstall", `v:=0;f:=func(){};p:=&f;defer func(){f()}();defer func(){*p=func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
	{"parameterInstall", `x:=0;bad:=func(){use(x)};f:=func(){};defer func(){for range 2{go f();x++}}();defer func(p *func()){*p=bad}(&f)`, 1},
	{"capturedKill", `v:=0;f:=func(){v++};p:=&f;defer func(){f()}();defer func(){*p=func(){}}();for range xs{go func(){use(v)}()}`, 0},
	{"parameterKill", `x:=0;f:=func(){use(x)};defer func(){for range 2{go f();x++}}();defer func(p *func()){*p=func(){}}(&f)`, 0},
	{"capturedAcrossBlock", `v:=0;f:=func(){};p:=&f;defer func(){f()}();defer func(){*p=func(){v++};if len(xs)>0{use(0)}}();for range xs{go func(){use(v)}()}`, 1},
	{"parameterAcrossBlock", `x:=0;bad:=func(){use(x)};f:=func(){};defer func(){for range 2{go f();x++}}();defer func(p *func()){*p=bad;if len(xs)>0{use(0)}}(&f)`, 1},
	{"parameterSavedAtRegistration", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){f()}();defer func(p *func()){*p=func(){v++}}(p);p=&g;_=p;for range xs{go func(){use(v)}()}`, 1},
	{"capturedReadAtExit", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){f()}();defer func(){*p=func(){v++}}();p=&g;for range xs{go func(){use(v)}()}`, 0},
	{"capturedReboundTarget", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){g()}();defer func(){*p=func(){v++}}();p=&g;for range xs{go func(){use(v)}()}`, 1},
	{"doubleCaptured", `v:=0;f:=func(){};p:=&f;pp:=&p;defer func(){f()}();defer func(){**pp=func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
	{"doubleParameter", `v:=0;f:=func(){};p:=&f;defer func(){f()}();defer func(pp **func()){**pp=func(){v++}}(&p);for range xs{go func(){use(v)}()}`, 1},
	{"savedCellLoadsCurrentPointer", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){g()}();defer func(pp **func()){**pp=func(){v++}}(&p);p=&g;for range xs{go func(){use(v)}()}`, 1},
	{"savedCallbackDoesNotReload", `v:=0;f:=func(){};p:=&f;defer func(cb func()){cb()}(f);defer func(){*p=func(){v++}}();for range xs{go func(){use(v)}()}`, 0},
	{"savedCallbackKeepsOldContents", `v:=0;f:=func(){v++};p:=&f;defer func(cb func()){cb()}(f);defer func(){*p=func(){}}();for range xs{go func(){use(v)}()}`, 1},
	{"tripleParameter", `v:=0;f:=func(){};p:=&f;pp:=&p;defer func(){f()}();defer func(ppp ***func()){***ppp=func(){v++}}(&pp);for range xs{go func(){use(v)}()}`, 1},
	{"aliasHandback", `v:=0;f,g:=func(){},func(){v++};p:=&f;defer func(){(*p)()}();defer func(pp **func(),q *func()){*pp=q}(&p,&g);for range xs{go func(){use(v)}()}`, 1},
	{"aliasHandbackKill", `v:=0;f,g:=func(){v++},func(){};p:=&f;defer func(){(*p)()}();defer func(pp **func(),q *func()){*pp=q}(&p,&g);for range xs{go func(){use(v)}()}`, 0},
	{"rebindThenStore", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){g()}();defer func(pp **func(),q *func()){*pp=q;**pp=func(){v++}}(&p,&g);for range xs{go func(){use(v)}()}`, 1},
	{"threeDeferredHandbacks", `v:=0;f,g:=func(){},func(){};p:=&f;defer func(){g()}();defer func(){*p=func(){v++}}();defer func(pp **func(),q *func()){*pp=q}(&p,&g);for range xs{go func(){use(v)}()}`, 1},
	{"pointeeValueHandback", `v:=0;p:=(*int)(nil);defer func(){for range 2{go func(){use(*p)}();v++}}();defer func(pp **int,q *int){*pp=q}(&p,&v)`, 1},
	{"originHandback", `v:=0;f:=func(){};defer func(){f();use(v)}();defer func(p *func()){*p=func(){for range xs{go func(){v++}()}}}(&f)`, 1},
	{"originHandbackKill", `for range xs{v:=0;func(){f:=func(){go func(){use(v)}()};defer func(){f()}();defer func(p *func()){*p=func(){}}(&f)}();v++}`, 0},
	{"nestedActualParameter", `v:=0;f:=func(){};defer func(){f()}();defer func(p *func()){func(q *func()){*q=func(){v++}}(p)}(&f);for range xs{go func(){use(v)}()}`, 1},
	{"joinedStoreKeepsOld", `v:=0;f,g:=func(){v++},func(){};p:=&f;if len(xs)>0{p=&g};defer func(){f()}();defer func(){*p=func(){}}();for range xs{go func(){use(v)}()}`, 1},
	{"joinedStoreInstalls", `v:=0;f,g:=func(){},func(){};p:=&f;if len(xs)>0{p=&g};defer func(){f()}();defer func(){*p=func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
	{"aggregateFieldWeakContract", `v:=0;s:=struct{F func()}{func(){v++}};defer func(){s.F()}();defer func(p *struct{F func()}){p.F=func(){}}(&s);for range xs{go func(){use(v)}()}`, 1},
	{"wholeAggregateWeakContract", `v:=0;s:=struct{F func()}{func(){v++}};defer func(){s.F()}();defer func(p *struct{F func()}){*p=struct{F func()}{func(){}}}(&s);for range xs{go func(){use(v)}()}`, 1},
	{"aggregateInstall", `v:=0;s:=struct{F func()}{func(){}};defer func(){for range xs{go s.F();v++}}();defer func(p *struct{F func()}){*p=struct{F func()}{func(){use(v)}}}(&s)`, 1},
	{"callbackReferenceNotAddress", `v:=0;f:=func(){v++};defer func(){f()}();defer func(p func()){p=func(){};_=p}(f);for range xs{go func(){use(v)}()}`, 1},
	{"freshParameterRebound", `v:=0;f:=func(){v++};defer func(){f()}();defer func(p *func()){g:=func(){};p=&g;*p=func(){}}(&f);for range xs{go func(){use(v)}()}`, 1},
	{"constructorNotExecution", `v:=0;f:=func(){};defer func(){for range xs{go f();v++}}();unused:=func(p *func()){*p=func(){use(v)}};_=unused`, 0},
}

func TestLoopDeferredAddressWritebackModes(t *testing.T) {
	for _, tc := range loopDeferredAddressWritebackCases {
		t.Run(tc.name, func(t *testing.T) {
			source := loopTestPrelude + "func work(xs []int){" + tc.body + "}"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "writeback.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/writeback", fset, []*ast.File{file}, nil); err != nil {
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

func TestLoopDeferredAddressWritebackPublicCompleteTypes(t *testing.T) {
	var source strings.Builder
	source.WriteString("package writeback\nfunc use(...any){}\n")
	for _, tc := range loopDeferredAddressWritebackCases {
		fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/writeback\n\ngo 1.23\n", "writeback.go": source.String()})
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile public fixtures: %v\n%s", err, out)
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
	for _, tc := range loopDeferredAddressWritebackCases {
		if counts[tc.name] != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, counts[tc.name], tc.want)
		}
	}
}

func TestLoopDeferredAddressWritebackEpochsAndIsolation(t *testing.T) {
	for _, mode := range []loopAccess{loopRead, loopRead | loopPriorRef, loopRead | loopPriorRef | loopEpochMixedRef} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			z := newDeferredFixture(t, `func work(){v:=0;f:=func(){};defer func(p *func()){local:=func(){};*p=func(){v++};_=local}(&f);return}`)
			z.f.deferred = z.d
			z.f.conflicts = &loopFlowConflicts{ids: map[*loopVariable]uint64{}}
			z.steps()
			stmt := z.body.List[2].(*ast.DeferStmt)
			slot := z.d.slots[stmt]
			f := z.vars["f"]
			z.w.values[slot.addresses[0]] = loopValue{f: mode}
			inv := z.p.invocations(slot, z.w.values, z.f.aliasSlot)[0]
			inv.seed = z.f.deferredSeed(inv)
			before := deepDeferredBindings(z.w.values)
			output := z.f.simulateDeferred(inv)
			got := z.f.exportDeferred(inv, loopCloneValues(z.w.values), output)
			for v := range got {
				if v.ident != nil && v.owner == inv.literal {
					t.Fatalf("callee binding leaked: %s", v.ident.Name)
				}
			}
			for _, cell := range []*loopVariable{f, z.f.aliasSlot(f), z.d.companion(f)} {
				if loopEpochCurrentMode(mode) == 0 && !reflect.DeepEqual(got[cell], before[cell]) {
					t.Fatal("prior-only proof overwrote current cell")
				}
				if mode&loopPriorRef != 0 {
					for v, m := range before[cell] {
						if got[cell][v]&m != m {
							t.Fatal("mixed/prior writeback lost old contents")
						}
					}
				}
			}
			if loopEpochCurrentMode(mode) != 0 && got[f][z.vars["v"]] == 0 {
				t.Fatal("current address did not export installed closure")
			}
			if !reflect.DeepEqual(before, z.w.values) {
				t.Fatal("writeback mutated saved snapshot")
			}
			prepared := z.f.prepareDeferredCFG(inv.literal.Body, inv.literal)
			if prepared.adapter.exports[f] {
				t.Fatal("invocation address contaminated shared CFG exports")
			}
		})
	}
}
