package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

const loopPromotedWritePrelude = loopPromotedPathPrelude + `
type Alias = Inner
type AliasOuter struct{*Alias}
type Generic[T any] struct{N T}
type GenericOuter[T any] struct{*Generic[T]}
`

var loopPromotedWriteCases = []struct {
	name, body string
	want       int
}{
	{"sharedPromotedAssign", `v:=Inner{}; for range xs {s:=Outer{&v}; go func(){s.N=1}()}`, 1},
	{"sharedExplicitAssign", `v:=Inner{}; for range xs {s:=Outer{&v}; go func(){s.Inner.N=1}()}`, 1},
	{"sharedPromotedInc", `v:=Inner{}; for range xs {s:=Outer{&v}; go func(){s.N++}()}`, 1},
	{"sharedPromotedCompound", `v:=Inner{}; for range xs {s:=Outer{&v}; go func(){s.N+=1}()}`, 1},
	{"freshDataParentRead", `for range xs {s:=Outer{&Inner{}}; go func(){s.N=1}(); _=s.N}`, 1},
	{"freshDataParentWrite", `for range xs {s:=Outer{&Inner{}}; go func(){_=s.N}(); s.N=1}`, 1},
	{"freshDataSiblings", `for range xs {s:=Outer{&Inner{}}; go func(){_=s.N}(); go func(){s.N=1}()}`, 2},
	{"freshDataWriterSiblings", `for range xs {s:=Outer{&Inner{}}; go func(){s.N=1}(); go func(){s.N=2}()}`, 2},
	{"knownLocalParentRead", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){s.N=1}(); _=v.N}`, 1},
	{"knownLocalParentWrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=s.N}(); v.N=1}`, 1},
	{"knownLocalWorkerRead", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); s.N=1}`, 1},
	{"knownLocalWorkerWrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){v.N=1}(); _=s.N}`, 1},
	{"knownLocalSiblings", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); go func(){s.N=1}()}`, 2},
	{"noPointeeWriteForHeaderStore", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); s.Inner=&Inner{}}`, 0},
	{"copiedPointerBeforeOverwrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(p *Inner){_=p.N}(s.Inner); s=Outer{&Inner{}}}`, 0},
	{"copiedFieldAddressBeforeOverwrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(p *int){_=*p}(&s.N); s=Outer{&Inner{}}}`, 0},
	{"knownFieldAddress", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s.N; go func(){_=*p}(); s.N=1}`, 1},
	{"knownFieldAddressPhysicalStore", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s.N; go func(){_=v.N}(); *p=1}`, 1},
	{"knownFieldAddressPhysicalRead", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s.N; go func(){v.N=1}(); _=*p}`, 1},
	{"deepSharedAssign", `v:=Inner{}; for range xs {s:=Deep{Outer{&v}}; go func(){s.N=1}()}`, 1},
	{"pointerDeepSharedAssign", `v:=Inner{}; for range xs {s:=PointerDeep{&Outer{&v}}; go func(){s.N=1}()}`, 1},
	{"knownPointerOuterParentRead", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s; go func(){p.N=1}(); _=v.N}`, 1},
	{"knownPointerOuterParentWrite", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s; go func(){_=p.N}(); v.N=1}`, 1},
	{"unknownHeapSameBinding", `for range xs {p:=&Outer{&Inner{}}; go func(){_=p.N}(); p.N=1}`, 1},
	{"unknownHeapNoLocalAlias", `for range xs {v:=Inner{}; p:=&Outer{&v}; go func(){_=v.N}(); p.N=1}`, 0},
	{"knownPointerDoesNotWriteWrapper", `for range xs {s:=Outer{&Inner{}}; p:=&s; go func(){p.N=1}(); _=s}`, 0},
	{"knownPointerExplicitDoesNotWriteWrapper", `for range xs {s:=Outer{&Inner{}}; p:=&s; go func(){p.Inner.N=1}(); _=s}`, 0},
	{"unknownHeapWorkerNoLocalAlias", `for range xs {v:=Inner{}; p:=&Outer{&v}; go func(){p.N=1}(); _=v.N}`, 0},
	{"noPromotedWrapperWrite", `for range xs {s:=Outer{&Inner{}}; go func(){s.N=1}(); _=s}`, 0},
	{"noExplicitWrapperWrite", `for range xs {s:=Outer{&Inner{}}; go func(){s.Inner.N=1}(); _=s}`, 0},
	{"explicitWorkerRead", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); s.Inner.N=1}`, 1},
	{"deepParentWrite", `for range xs {v:=Inner{}; s:=Deep{Outer{&v}}; go func(){_=v.N}(); s.N=1}`, 1},
	{"deepExplicitParentWrite", `for range xs {v:=Inner{}; s:=Deep{Outer{&v}}; go func(){_=v.N}(); s.Outer.Inner.N=1}`, 1},
	{"localFieldAddressParentWrite", `for range xs {s:=ValueOuter{}; p:=&s.N; go func(){_=*p}(); s.N=1}`, 1},
	{"promotedAddressLoadsWrapper", `for range xs {s:=Outer{&Inner{}}; go func(){_=&s.N}(); s=Outer{&Inner{}}}`, 1},
	{"promotedAddressDoesNotReadPointee", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){v.N=1}(); _=&s.N}`, 0},
	{"promotedAddressLoadsKnownWrapper", `for range xs {s:=Outer{&Inner{}}; p:=&s; go func(){s.Inner=&Inner{}}(); _=&p.N}`, 1},
	{"promotedAddressNotKnownPointeeRead", `for range xs {v:=Inner{}; s:=Outer{&v}; p:=&s; go func(){v.N=1}(); _=&p.N}`, 0},
	{"mixedWholeAndPromotedWrite", `for range xs {s:=Outer{&Inner{}}; p:=&s; go func(){p.N=1; *p=Outer{&Inner{}}}(); _=s}`, 1},
	{"knownLocalCompoundParentWrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); s.N+=1}`, 1},
	{"knownLocalIncParentWrite", `for range xs {v:=Inner{}; s:=Outer{&v}; go func(){_=v.N}(); s.N++}`, 1},
	{"aliasSharedAssign", `v:=Inner{}; for range xs {s:=AliasOuter{&v}; go func(){s.N=1}()}`, 1},
	{"genericSharedAssign", `v:=Generic[int]{}; for range xs {s:=GenericOuter[int]{&v}; go func(){s.N=1}()}`, 1},
	{"localTypeSharedAssign", `type Local struct{*Inner}; v:=Inner{}; for range xs {s:=Local{&v}; go func(){s.N=1}()}`, 1},
	{"freshValueField", `for range xs {s:=ValueOuter{}; go func(){s.N=1}()}`, 0},
	{"valueEmbeddedPointerWorkerWrite", `for range xs {s:=ValueOuter{}; p:=&s; go func(){p.N=1}(); _=s.N}`, 1},
	{"valueEmbeddedPointerParentWrite", `for range xs {s:=ValueOuter{}; p:=&s; go func(){_=s.N}(); p.N=1}`, 1},
	{"valueEmbeddedPointerExplicitParentWrite", `for range xs {s:=ValueOuter{}; p:=&s; go func(){_=s.N}(); p.Inner.N=1}`, 1},
	{"aliasChainSharedAssign", `type A = Inner; type B = A; type Wrap struct{*B}; v:=Inner{}; for range xs {s:=Wrap{&v}; go func(){s.N=1}()}`, 1},
	{"pointerAliasEmbedded", `type Ptr = *Inner; type Wrap struct{Ptr}; for range xs {v:=Inner{}; s:=Wrap{&v}; go func(){_=v.N}(); s.N=1}`, 1},
	{"pointerAliasWrapper", `type Ptr = *Outer; for range xs {v:=Inner{}; s:=Outer{&v}; var p Ptr=&s; go func(){_=v.N}(); p.N=1}`, 1},
	{"valueFieldParentRead", `for range xs {s:=ValueOuter{}; go func(){s.N=1}(); _=s.N}`, 1},
}

func TestLoopPromotedFieldWrites(t *testing.T) {
	for _, tc := range loopPromotedWriteCases {
		t.Run(tc.name, func(t *testing.T) {
			source := loopPromotedWritePrelude + "func work(xs []int){" + tc.body + "}"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "writes.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/writes", fset, []*ast.File{file}, nil); err != nil {
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

// Assert physical channels, not only finding counts: the known local receives
// a slot write, while the wrapper may only be read to obtain its pointer field.
func TestLoopPromotedFieldWriteStorageChannels(t *testing.T) {
	for _, target := range []string{"s.N", "s.Inner.N", "p.N", "p.Inner.N"} {
		t.Run(target, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "storage.go", loopPromotedPathPrelude+
				"func work(){v:=Inner{}; s:=Outer{&v}; p:=&s; "+target+"=1}", parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			ctx := loopContext{fset: fset, file: file}
			x := &loopIndex{ctx: ctx, ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
				parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
				regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{}}
			x.pushScope()
			body := file.Decls[len(file.Decls)-1].(*ast.FuncDecl).Body
			x.node(body)
			vars := map[string]*loopVariable{}
			for id, v := range x.ids {
				vars[id.Name] = v
			}
			slot, callback := &loopVariable{}, &loopVariable{}
			w := &loopWalk{aliases: map[*loopVariable]*loopVariable{vars["p"]: slot}, values: map[*loopVariable]loopValue{
				vars["s"]: {vars["v"]: loopRead | loopPriorRef, callback: loopRetain | loopWrite<<loopCallableShift},
				vars["p"]: {vars["s"]: loopRead}, slot: {vars["s"]: loopRead},
			}}
			s := &loopScanner{index: x, env: loopEnv{ctx: ctx}}
			e := s.beginFlowEffects()
			e.aliases = w.aliases
			store := body.List[len(body.List)-1].(*ast.AssignStmt)
			roots, handled := s.promotedFieldAddressRoots(store.Lhs[0], w)
			if !handled || len(roots) != 1 || roots[vars["v"]] != loopRead|loopPriorRef|loopAddressPartial {
				t.Fatalf("field address lost its partial local root: %v, handled %v", roots, handled)
			}
			e.recordFlowEffects(store, w)
			writes := 0
			for _, access := range e.direct {
				if access.mode&loopWrite == 0 {
					continue
				}
				if access.variable != vars["v"] || access.mode&loopPriorRef == 0 || access.mode&(loopIndirectWrite|loopIndirectRead) != 0 {
					t.Fatalf("write did not reach the actual local slot: %+v", access)
				}
				writes++
			}
			if writes != 1 {
				t.Fatalf("got %d physical slot writes, want one", writes)
			}
			if len(w.values) != 3 || len(w.aliases) != 1 {
				t.Fatal("effect replay changed bindings")
			}
		})
	}
}

func TestLoopPromotedFieldWritesPublic(t *testing.T) {
	var source strings.Builder
	source.WriteString(loopPromotedWritePrelude)
	for _, tc := range loopPromotedWriteCases {
		fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/writes\n\ngo 1.23\n", "writes.go": source.String()})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	byFunction := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("incomplete public type info: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			byFunction[finding.Symbol]++
		}
	}
	for _, tc := range loopPromotedWriteCases {
		if byFunction[tc.name] != tc.want {
			t.Errorf("%s: got %d findings, want %d", tc.name, byFunction[tc.name], tc.want)
		}
	}
}
