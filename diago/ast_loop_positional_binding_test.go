package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
	"testing"
)

var loopPositionalParameterCases = []struct {
	name, body string
	want       int
}{
	{"blank before pointer", `for range xs{v:=0;go func(_ int,p *int){*p=1}(0,&v);_=v}`, 1},
	{"named before pointer control", `for range xs{v:=0;go func(n int,p *int){*p=1}(0,&v);_=v}`, 1},
	{"grouped blanks before pointer", `for range xs{v:=0;go func(_, _ int,p *int){*p=1}(0,1,&v);_=v}`, 1},
	{"separate blank fields before pointer", `for range xs{v:=0;go func(_ int,_ string,p *int){*p=1}(0,"",&v);_=v}`, 1},
	{"blank between copied pointers", `for range xs{v,u:=0,0;go func(p *int,_ int,q *int){*q=1}(&u,0,&v);_=v}`, 1},
	{"blank before callback", `v:=0;for range xs{go func(_ int,f func()){f()}(0,func(){v++})}`, 1},
	{"grouped blank callbacks", `v:=0;for range xs{go func(_, f func()){f()}(func(){},func(){v++})}`, 1},
	{"discarded callback stays inactive", `v:=0;for range xs{go func(_ func(),f func()){f()}(func(){v++},func(){})}`, 0},
	{"discarded pointer stays inactive", `for range xs{v,u:=0,0;go func(_ *int,p *int){*p=1}(&v,&u);_=v}`, 0},
	{"blank before packed variadic", `type Box struct{N int};var a Box;for range xs{func(_ int,ps ...*Box){z:=ps[0];go func(){z.N++}()}(0,&a);a.N++}`, 1},
	{"grouped blanks before packed variadic", `type Box struct{N int};var a Box;for range xs{func(_, _ int,ps ...*Box){z:=ps[0];go func(){z.N++}()}(0,1,&a);a.N++}`, 1},
	{"blank before ellipsis", `type Box struct{N int};var a Box;for range xs{func(_ int,ps ...*Box){z:=ps[0];go func(){z.N++}()}(0,[]*Box{&a}...);a.N++}`, 1},
	{"packed variadic slice is not first pointee", `type Box struct{N int};var a Box;for range xs{func(ps ...*Box){z:=ps[0];go func(){z.N++}()}(&a);a.N++}`, 1},
	{"explicit slice control", `type Box struct{N int};var a Box;for range xs{func(ps []*Box){z:=ps[0];go func(){z.N++}()}([]*Box{&a});a.N++}`, 1},
	{"nonvariadic pointer control", `type Box struct{N int};var a Box;for range xs{func(p *Box){z:=p;go func(){z.N++}()}(&a);a.N++}`, 1},
	{"ellipsis slice control", `type Box struct{N int};var a Box;for range xs{ps:=[]*Box{&a};func(ps ...*Box){z:=ps[0];go func(){z.N++}()}(ps...);a.N++}`, 1},
	{"packed variadic retains later pointer", `type Box struct{N int};var a,other Box;for range xs{func(ps ...*Box){z:=ps[1];go func(){z.N++}()}(&other,&a);a.N++}`, 1},
	{"packed variadic retains first pointer", `type Box struct{N int};var a,other Box;for range xs{func(ps ...*Box){z:=ps[0];go func(){z.N++}()}(&a,&other);a.N++}`, 1},
	{"packed variadic callback contents", `v:=0;for range xs{func(fs ...func()){go fs[0]()}(func(){v++},func(){})}`, 1},
	{"packed variadic later callback contents", `v:=0;for range xs{func(fs ...func()){go fs[1]()}(func(){},func(){v++})}`, 1},
	{"empty variadic after blank", `v:=0;for range xs{func(_ int,ps ...*int){go func(){_=len(ps)}()}(v)}`, 0},
	{"blank variadic discards contents", `v:=0;for range xs{func(_ ...*int){go func(){}()}(&v);v++}`, 0},
	{"parameter whole overwrite", `for range xs{v,u:=0,0;func(_ int,p *int){p=&u;go func(){*p=1}()}(0,&v);_=v}`, 0},
	{"packed parameter whole overwrite", `type Box struct{N int};var a Box;for range xs{func(ps ...*Box){ps=nil;go func(){_=ps}()}(&a);a.N++}`, 0},
	{"packed invocation local is fresh", `type Box struct{N int};for range xs{func(ps ...*Box){v:=0;go func(){v++}()}()}`, 0},
	{"same callable distinct actual parameters", `type Box struct{N int};var a,other Box;worker:=func(ps ...*Box){z:=ps[0];go func(){z.N++}()};for range xs{worker(&a);other.N++;worker(&other)}`, 1},
	{"actual arguments do not leak between invocations", `type Box struct{N int};for range xs{var a,other Box;worker:=func(ps ...*Box){z:=ps[0];go func(){z.N++}()};worker(&a);other.N++;worker(&other)}`, 0},
	{"second actual argument conflicts after launch", `type Box struct{N int};for range xs{var a,other Box;worker:=func(ps ...*Box){z:=ps[0];go func(){z.N++}()};worker(&a);worker(&other);other.N++}`, 1},
	{"pointer parameter is copied", `for range xs{v,u:=0,0;p:=&v;go func(_ int,p *int){*p=1}(0,p);p=&u;_=p}`, 0},
}

func TestLoopPositionalParameterBindings(t *testing.T) {
	for _, tc := range loopPositionalParameterCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s := loopSyncTestScanner(t, tc.body, "1.23", mode)
					s.report()
					if got := countRules(*s.env.findings)["goroutine-in-loop"]; got != tc.want {
						t.Fatalf("findings=%d, want %d: %+v", got, tc.want, *s.env.findings)
					}
				})
			}
		})
	}
}

func TestLoopPackedParameterOriginsAndSnapshots(t *testing.T) {
	for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
		t.Run(mode, func(t *testing.T) {
			s := loopSyncTestScanner(t, `v,u:=0,0;for range xs{func(fs ...func()){go fs[0]()}(func(){v++},func(){u++})}`, "1.23", mode)
			f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: s.beginFlowEffects(), conflicts: s.beginFlowConflicts()}
			f.effects.aliases = f.aliases
			f.refine(s.rootNode.(*ast.BlockStmt), nil)
			var call *ast.CallExpr
			ast.Inspect(s.rootNode, func(n ast.Node) bool {
				if candidate, ok := n.(*ast.CallExpr); ok {
					if lit, ok := candidate.Fun.(*ast.FuncLit); ok && len(lit.Type.Params.List) != 0 {
						call = candidate
					}
				}
				return true
			})
			if call == nil {
				t.Fatal("missing variadic invocation")
			}
			lit := call.Fun.(*ast.FuncLit)
			param := s.index.ids[lit.Type.Params.List[0].Names[0]]
			var seed map[*loopVariable]loopValue
			for _, frame := range f.conflicts.activeContextFrames() {
				if frame.owner == lit && frame.actual {
					seed = frame.seed
				}
			}
			if seed == nil || len(seed[param]) != 2 || len(seed[f.aliasSlot(param)]) != 0 {
				t.Fatalf("packed parameter lost contents or invented address: %v", seed[param])
			}
			origins := seed[f.deferred.companion(param)]
			if len(origins) != 2 {
				t.Fatalf("packed parameter origins=%v, want both arguments", origins)
			}
			for _, arg := range call.Args {
				if origins[f.deferred.literal(arg.(*ast.FuncLit))] == 0 {
					t.Fatal("packed parameter dropped an argument's callable origin")
				}
			}

			// Rebinding a prior/current mixture must preserve provenance, copy
			// argument maps, and never merge a later value into the first source.
			var v, u *loopVariable
			for variable := range seed[param] {
				if variable.ident.Name == "v" {
					v = variable
				} else if variable.ident.Name == "u" {
					u = variable
				}
			}
			if v == nil || u == nil {
				t.Fatal("missing captured argument variables")
			}
			first, second := &ast.Ident{Name: "first"}, &ast.Ident{Name: "second"}
			s.index.ids[first], s.index.ids[second] = v, u
			w := &loopWalk{values: map[*loopVariable]loopValue{
				v: {v: loopRead | loopPriorRef}, u: {v: loopWrite, u: loopRead},
				f.aliasSlot(v):          {v: loopRead},
				f.deferred.companion(v): {f.deferred.literal(call.Args[0].(*ast.FuncLit)): loopRetain},
				f.deferred.companion(u): {f.deferred.literal(call.Args[1].(*ast.FuncLit)): loopRetain},
			}, aliases: f.aliases}
			before := deepDeferredBindings(w.values)
			bound := f.invocationLiteralContext(&ast.CallExpr{Fun: lit, Args: []ast.Expr{first, second}}, lit, w, true)
			want := loopValue{}
			loopMerge(want, before[v])
			loopMerge(want, before[u])
			if !reflect.DeepEqual(bound.seed[param], want) || len(bound.seed[f.aliasSlot(param)]) != 0 {
				t.Fatalf("packed provenance=%v, want %v without an address alias", bound.seed[param], want)
			}
			if !reflect.DeepEqual(w.values, before) {
				t.Fatal("parameter binding mutated an argument snapshot")
			}
		})
	}
}

func TestLoopPositionalParameterBindingsPublic(t *testing.T) {
	var source strings.Builder
	source.WriteString("package positional\nfunc use(...any){}\n")
	for i, tc := range loopPositionalParameterCases {
		fmt.Fprintf(&source, "func case%d(xs []int){%s}\n", i, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":        "module example.com/positional\n\ngo 1.23\n",
		"positional.go": source.String(),
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("fixture lost type info: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for i, tc := range loopPositionalParameterCases {
		if got := counts[fmt.Sprintf("case%d", i)]; got != tc.want {
			t.Errorf("%s: findings=%d, want %d", tc.name, got, tc.want)
		}
	}
}
