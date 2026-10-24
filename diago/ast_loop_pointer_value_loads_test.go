package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestLoopPointerFieldIndexValueLoads(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"direct field reader", `v:=0; s:=struct{F func()}{func(){use(v)}}; p:=&s; for range xs {go p.F(); v++}`, 1},
		{"direct field writer", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; for range xs {go p.F()}`, 1},
		{"direct index reader", `v:=0; a:=[1]func(){func(){use(v)}}; p:=&a; for range xs {go p[0](); v++}`, 1},
		{"direct index writer", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; for range xs {go p[0]()}`, 1},
		{"wrapped field reader", `v:=0; s:=struct{F func()}{func(){use(v)}}; p:=&s; for range xs {go func(){p.F()}(); v++}`, 1},
		{"wrapped field writer", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; for range xs {go func(){p.F()}()}`, 1},
		{"wrapped index reader", `v:=0; a:=[1]func(){func(){use(v)}}; p:=&a; for range xs {go func(){p[0]()}(); v++}`, 1},
		{"wrapped index writer", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; for range xs {go func(){p[0]()}()}`, 1},
		{"wrapped aggregate address chain", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; pp:=&p; for range xs {go func(){(*pp).F()}()}`, 1},
		{"field pointer copied before go", `v:=0; s:=struct{P *int}{&v}; p:=&s; for range xs {go use(p.P); s=struct{P *int}{&v}}`, 0},
		{"field pointer header overwritten", `v:=0; s:=struct{P *int}{&v}; p:=&s; for range xs {go use(p.P); p=&s}`, 0},
		{"copied method value comparison", `var s item; f:=s.Use; for range xs {go func(){_=f==nil}(); s.N++}`, 0},
		{"copied method value invocation", `var s item; f:=s.Use; for range xs {go f(); s.N++}`, 1},
		{"method field copied comparison", `var v item; s:=struct{F func()}{v.Use}; p:=&s; f:=p.F; for range xs {go func(){_=f==nil}(); v.N++}`, 0},
		{"method field invocation", `var v item; s:=struct{F func()}{v.Use}; p:=&s; for range xs {go p.F(); v.N++}`, 1},
		{"method index copied comparison", `var v item; a:=[1]func(){v.Use}; p:=&a; f:=p[0]; for range xs {go func(){_=f==nil}(); v.N++}`, 0},
		{"method index invocation", `var v item; a:=[1]func(){v.Use}; p:=&a; for range xs {go p[0](); v.N++}`, 1},
		{"method receiver callback remains opaque", `v:=0; s:=callbackReceiver{func(){v++}}; for range xs {go s.Touch()}`, 0},
		{"wrapped method receiver callback remains opaque", `v:=0; s:=callbackReceiver{func(){v++}}; for range xs {go func(){s.Touch()}()}`, 0},
		{"method comparison implicit address is not a read", `var s item; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 0},
		{"wrapped method call reads receiver", `var s item; for range xs {go func(){s.Use()}(); s.N++}`, 1},
		{"field pointer target mutated", `v:=0; s:=struct{P *int}{&v}; p:=&s; for range xs {go use(p.P); v++}`, 1},
		{"index pointer copied before go", `v:=0; a:=[1]*int{&v}; p:=&a; for range xs {go use(p[0]); a=[1]*int{&v}}`, 0},
		{"index pointer header overwritten", `v:=0; a:=[1]*int{&v}; p:=&a; for range xs {go use(p[0]); p=&a}`, 0},
		{"index pointer target mutated", `v:=0; a:=[1]*int{&v}; p:=&a; for range xs {go use(p[0]); v++}`, 1},
		{"field callback copied comparison", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; f:=p.F; for range xs {go func(){_=f==nil}()}`, 0},
		{"index callback copied comparison", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; f:=p[0]; for range xs {go func(){_=f==nil}()}`, 0},
		{"field comparison does not invoke", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; for range xs {go func(){_=p.F==nil}()}`, 0},
		{"index comparison does not invoke", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; for range xs {go func(){_=p[0]==nil}()}`, 0},
		{"field compared reader remains latent", `v:=0; s:=struct{F func()}{func(){use(v)}}; p:=&s; for range xs {go func(){_=p.F==nil}(); v++}`, 0},
		{"index compared reader remains latent", `v:=0; a:=[1]func(){func(){use(v)}}; p:=&a; for range xs {go func(){_=p[0]==nil}(); v++}`, 0},
		{"field callee only compares another callback", `v:=0; g:=func(){v++}; s:=struct{F func()}{func(){_=g==nil}}; p:=&s; for range xs {go p.F()}`, 0},
		{"wrapped field callee only compares another callback", `v:=0; g:=func(){v++}; s:=struct{F func()}{func(){_=g==nil}}; p:=&s; for range xs {go func(){p.F()}()}`, 0},
		{"index callee only compares another callback", `v:=0; g:=func(){v++}; a:=[1]func(){func(){_=g==nil}}; p:=&a; for range xs {go p[0]()}`, 0},
		{"wrapped index callee only compares another callback", `v:=0; g:=func(){v++}; a:=[1]func(){func(){_=g==nil}}; p:=&a; for range xs {go func(){p[0]()}()}`, 0},
		{"field load uses current safe contents", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; s=struct{F func()}{func(){}}; for range xs {go p.F()}`, 0},
		{"index load uses current safe contents", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; a=[1]func(){func(){}}; for range xs {go p[0]()}`, 0},
		{"field snapshot preserves old callback", `v:=0; s:=struct{F func()}{func(){v++}}; p:=&s; f:=p.F; s=struct{F func()}{func(){}}; for range xs {go f()}`, 1},
		{"index snapshot preserves old callback", `v:=0; a:=[1]func(){func(){v++}}; p:=&a; f:=p[0]; a=[1]func(){func(){}}; for range xs {go f()}`, 1},
		{"unknown heap field is not a local address", `v:=0; p:=&struct{F func()}{func(){v++}}; for range xs {go p.F()}`, 1},
		{"unknown heap index is not a local address", `v:=0; p:=&[1]func(){func(){v++}}; for range xs {go p[0]()}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := loopTestPrelude + `type callbackReceiver struct{F func()}
func (*callbackReceiver) Touch() {}
` + "func work(xs []int){" + tc.body + "}"
			// Validate every fixture independently of fallback mode. A missing
			// type/selection map must never excuse invalid test source.
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "pointer_loads.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/loads", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, source, "go1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopPointerValueLoadsPublicCompleteTypes(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/loads\n\ngo 1.23\n",
		"loads.go": `package loads
func use(...any) {}
func field(xs []int) {v:=0; s:=struct{F func()}{func(){use(v)}}; p:=&s; for range xs {go p.F(); v++}}
func index(xs []int) {v:=0; a:=[1]func(){func(){use(v)}}; p:=&a; for range xs {go p[0](); v++}}
func copied(xs []int) {v:=0; s:=struct{P *int}{&v}; p:=&s; for range xs {go use(p.P); s=struct{P *int}{&v}}}
func target(xs []int) {v:=0; s:=struct{P *int}{&v}; p:=&s; for range xs {go use(p.P); v++}}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	byFunction := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("fixture must have complete public type information: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			byFunction[finding.Symbol]++
		}
	}
	for name, want := range map[string]int{"field": 1, "index": 1, "copied": 0, "target": 1} {
		if byFunction[name] != want {
			t.Errorf("%s: got %d findings, want %d: %+v", name, byFunction[name], want, findings)
		}
	}
}

func TestLoopFieldInvocationExpansionRequiresKnownAddress(t *testing.T) {
	p, aggregate, v := &loopVariable{}, &loopVariable{}, &loopVariable{}
	slot := &loopVariable{}
	w := &loopWalk{
		values: map[*loopVariable]loopValue{
			p: {aggregate: loopRead}, aggregate: {v: loopRetain | loopWrite<<loopCallableShift},
			slot: {aggregate: loopRead},
		},
		aliases: map[*loopVariable]*loopVariable{p: slot},
	}
	s := &loopScanner{}
	got := s.expand(loopValue{p: loopRead | loopInvoke | loopFieldInvoke}, w)
	if got[v]&loopWrite == 0 {
		t.Fatalf("known aggregate address failed to invoke its callback: %v", got)
	}
	w.aliases = nil
	got = s.expand(loopValue{p: loopRead | loopInvoke | loopFieldInvoke}, w)
	if got[v]&loopWrite != 0 || got[v]&(loopWrite<<loopCallableShift) == 0 {
		t.Fatalf("ordinary captured contents were treated as an address: %v", got)
	}
	w.aliases = map[*loopVariable]*loopVariable{p: slot}
	w.values[slot] = loopValue{aggregate: loopRead | loopPriorRef}
	got = s.expand(loopValue{p: loopRead | loopInvoke | loopFieldInvoke}, w)
	if got[v]&loopWrite != 0 {
		t.Fatalf("old address invoked a fresh aggregate's contents: %v", got)
	}
}

func TestLoopMethodReceiverBindingsAreLatentData(t *testing.T) {
	data, callback := &loopVariable{}, &loopVariable{}
	value := loopValue{
		data:     loopRead | loopPriorRef,
		callback: loopRetain | loopWrite<<loopCallableShift,
	}
	bound := loopBindMethodReceiver(value)
	if bound[data]&loopRead != 0 || bound[data]&(loopRead<<loopCallableShift) == 0 || bound[data]&loopPriorRef == 0 {
		t.Fatalf("binding read receiver storage or lost its epoch: %v", bound)
	}
	if bound[callback] != loopRetain {
		t.Fatalf("binding treated a receiver's stored callback as a callee: %v", bound)
	}
	if called := loopActivateCallableMode(bound[data]); called&loopRead == 0 || called&loopPriorRef == 0 {
		t.Fatalf("invocation failed to activate the receiver read: %v", called)
	}
	if value[data] != loopRead|loopPriorRef || value[callback] != loopRetain|loopWrite<<loopCallableShift {
		t.Fatal("binding mutated a shared receiver value")
	}
}

func TestLoopFieldInvocationHintRemainsLatent(t *testing.T) {
	v := &loopVariable{}
	lit := &ast.FuncLit{}
	s := &loopScanner{index: &loopIndex{captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{
		lit: {v: loopRead | loopInvoke | loopFieldInvoke},
	}}}
	stored := s.value(lit, &loopWalk{})
	if stored[v]&loopFieldInvoke != 0 || stored[v]&(loopFieldInvoke<<loopCallableShift) == 0 {
		t.Fatalf("stored callable lost or activated its field invocation hint: %v", stored)
	}
	called := s.invokeValue(lit, &loopWalk{})
	if called[v]&loopFieldInvoke == 0 || called[v]&loopCallableLatentMask != 0 {
		t.Fatalf("invocation failed to activate bit 15: %v", called)
	}
}
