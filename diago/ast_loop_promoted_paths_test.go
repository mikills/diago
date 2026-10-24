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

const loopPromotedPathPrelude = `package promoted
func use(...any) {}
type Inner struct{N int}
func (*Inner) Use() {}
func (Inner) Val() {}
type Outer struct{*Inner}
type ValueOuter struct{Inner}
type Deep struct{Outer}
type PointerDeep struct{*Outer}
`

var loopPromotedPathCases = []struct {
	name, body string
	want       int
}{
	{"promotedHeapFieldAddress", `s:=Outer{&Inner{}}; for range xs {p:=&s.N; go func(){use(*p)}(); s=Outer{&Inner{}}}`, 0},
	{"pointerPromotedHeapFieldAddress", `s:=Outer{&Inner{}}; q:=&s; for range xs {p:=&q.N; go func(){use(*p)}(); s=Outer{&Inner{}}}`, 0},
	{"pointerPromotedKnownInnerAddress", `v:=Inner{}; s:=Outer{&v}; q:=&s; for range xs {p:=&q.N; go func(){use(*p)}(); v.N++}`, 1},
	{"explicitHeapFieldAddress", `s:=Outer{&Inner{}}; for range xs {p:=&s.Inner.N; go func(){use(*p)}(); s=Outer{&Inner{}}}`, 0},
	{"promotedDeepHeapFieldAddress", `s:=Deep{Outer{&Inner{}}}; for range xs {p:=&s.N; go func(){use(*p)}(); s=Deep{Outer{&Inner{}}}}`, 0},
	{"promotedPointerDeepFieldAddress", `s:=PointerDeep{&Outer{&Inner{}}}; for range xs {p:=&s.N; go func(){use(*p)}(); s=PointerDeep{&Outer{&Inner{}}}}`, 0},
	{"lastPointerFieldAddress", `s:=Outer{&Inner{}}; for range xs {p:=&s.Inner; go func(){use(*p)}(); s=Outer{&Inner{}}}`, 1},
	{"valueFieldAddress", `s:=ValueOuter{}; for range xs {p:=&s.N; go func(){use(*p)}(); s.N++}`, 1},
	{"freshValueFieldAddress", `for range xs {s:=ValueOuter{}; p:=&s.N; go func(){use(*p)}()}`, 0},
	{"valueFieldReusedLocal", `for range xs {s:=ValueOuter{}; p:=&s.N; go func(){use(*p)}(); s.N++}`, 1},
	{"promotedKnownInnerAddress", `v:=Inner{}; s:=Outer{&v}; for range xs {p:=&s.N; go func(){use(*p)}(); v.N++}`, 1},
	{"promotedKnownInnerNotOuter", `v:=Inner{}; s:=Outer{&v}; for range xs {p:=&s.N; go func(){use(*p)}(); s=Outer{&Inner{}}}`, 0},
	{"pointerPromotedBindRead", `s:=Outer{&Inner{}}; p:=&s; ch:=make(chan func(),1); for {go func(){ch<-p.Use}(); s.Inner=&Inner{}; (<-ch)(); break}`, 1},
	{"explicitPointerBindRead", `s:=Outer{&Inner{}}; p:=&s; ch:=make(chan func(),1); for {go func(){ch<-p.Inner.Use}(); s.Inner=&Inner{}; (<-ch)(); break}`, 1},
	{"unknownPointerBindRead", `p:=&Outer{&Inner{}}; for range xs {go func(){_=p.Use==nil}(); p.Inner=&Inner{}}`, 1},
	{"pointerDeepBindRead", `s:=Deep{Outer{&Inner{}}}; p:=&s; for range xs {go func(){_=p.Use==nil}(); s.Outer=Outer{&Inner{}}}`, 1},
	{"pointerDeepPointerBindRead", `s:=PointerDeep{&Outer{&Inner{}}}; p:=&s; for range xs {go func(){_=p.Use==nil}(); s.Outer=&Outer{&Inner{}}}`, 1},
	{"directPointerHeaderOnly", `v:=Inner{}; p:=&v; for range xs {go func(){_=p.Use==nil}(); v.N++}`, 0},
	{"unknownHeapDoesNotReadInner", `v:=Inner{}; p:=&Outer{&v}; for range xs {go func(){_=p.Use==nil}(); v.N++}`, 0},
	{"unknownPointerHeaderOnly", `p:=&Outer{&Inner{}}; for range xs {go func(){_=p.Use==nil}(); use(p)}`, 0},
	{"promotedHeaderOnly", `v:=Inner{}; s:=Outer{&v}; p:=&s; for range xs {go func(){_=p.Use==nil}(); v.N++}`, 0},
	{"explicitPointerFieldHeaderOnly", `v:=Inner{}; s:=Outer{&v}; p:=&s; for range xs {go func(){_=p.Inner.Use==nil}(); v.N++}`, 0},
	{"implicitValueAddressOnly", `s:=ValueOuter{}; for range xs {go func(){_=s.Use==nil}(); s.N++}`, 0},
	{"pointerValueEmbedBindRead", `s:=ValueOuter{}; p:=&s; for range xs {go func(){_=p.Val==nil}(); s.N++}`, 1},
	{"directValueCopyRead", `v:=Inner{}; p:=&v; for range xs {go func(){_=p.Val==nil}(); v.N++}`, 1},
	{"callerBindingBeforeLaunch", `s:=Outer{&Inner{}}; p:=&s; for range xs {go use(p.Use); s.Inner=&Inner{}}`, 0},
	{"localNamedType", `type Local struct{*Inner}; s:=Local{&Inner{}}; p:=&s; for range xs {go func(){_=p.Use==nil}(); s.Inner=&Inner{}}`, 1},
}

func TestLoopPromotedSelectionPaths(t *testing.T) {
	for _, tc := range loopPromotedPathCases {
		t.Run(tc.name, func(t *testing.T) {
			source := loopPromotedPathPrelude + "func work(xs []int){" + tc.body + "}"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "promoted.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/promoted", fset, []*ast.File{file}, nil); err != nil {
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

func TestLoopMethodPathHeaderMode(t *testing.T) {
	access := loopHeaderRead | loopIndirectRead
	for _, epoch := range []loopAccess{0, loopPriorRef, loopPriorRef | loopEpochMixedRef} {
		if got := methodPathHeaderMode(access, loopRead|epoch, true); got != loopHeaderRead|epoch {
			t.Fatalf("header load followed retained data or lost provenance: %#x", got)
		}
		if got := methodPathHeaderMode(access, loopRetain|loopWrite<<loopCallableShift|epoch, true); got != loopRetain|epoch {
			t.Fatalf("header load activated callback contents: %#x", got)
		}
		if got := methodPathHeaderMode(access, loopRead|epoch, false); got&loopReadAccessMask != 0 {
			t.Fatalf("unknown heap contents became local field storage: %#x", got)
		}
		mode := loopRead | loopWrite | epoch
		if got := methodPathHeaderMode(access|loopRead, mode, true); got != mode {
			t.Fatalf("actual receiver data read was suppressed: %#x", got)
		}
	}
}

func TestLoopPromotedSelectionPathsPublic(t *testing.T) {
	var source strings.Builder
	source.WriteString(loopPromotedPathPrelude)
	for _, tc := range loopPromotedPathCases {
		fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/promoted\n\ngo 1.23\n", "promoted.go": source.String()})
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
	for _, tc := range loopPromotedPathCases {
		if byFunction[tc.name] != tc.want {
			t.Errorf("%s: got %d findings, want %d", tc.name, byFunction[tc.name], tc.want)
		}
	}
}
