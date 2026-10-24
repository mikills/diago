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

const loopMethodHeaderPrelude = `package headers
func use(...any) {}
type item struct{N int}
func (*item) Use() {}
func (item) Val() {}
type callbackReceiver struct{F func()}
func (*callbackReceiver) Touch() {}
type valueReceiver struct{P *int; F func()}
func (valueReceiver) Val() {}
type generic[T any] struct{N T}
func (*generic[T]) Use() {}
func (generic[T]) Val() {}
type wrapper struct{*item}
`

var loopMethodHeaderCases = []struct {
	name, body string
	want       int
}{
	{"pointerCompare", `var v item; p:=&v; for range xs {go func(){_=p.Use==nil}(); v.N++}`, 0},
	{"pointerHeaderWrite", `var v item; p:=&v; for range xs {go func(){_=p.Use==nil}(); p=&item{}}`, 1},
	{"pointerCall", `var v item; p:=&v; for range xs {go func(){p.Use()}(); v.N++}`, 1},
	{"pointerDirectCall", `var v item; p:=&v; for range xs {go p.Use(); v.N++}`, 1},
	{"pointerBoundCompare", `var v item; p:=&v; f:=p.Use; for range xs {go func(){_=f==nil}(); v.N++}`, 0},
	{"pointerCallerBindingNoDataRead", `var v item; p:=&v; for range xs {go func(){v.N++}(); f:=p.Use; _=f; break}`, 0},
	{"pointerBoundCall", `var v item; p:=&v; f:=p.Use; for range xs {go f(); v.N++}`, 1},
	{"valueDirectCopy", `var v item; p:=&v; for range xs {go p.Val(); v.N++}`, 0},
	{"valueJobBinding", `var v item; p:=&v; for range xs {go func(){_=p.Val==nil}(); v.N++}`, 1},
	{"valueJobCall", `var v item; p:=&v; for range xs {go func(){p.Val()}(); v.N++}`, 1},
	{"valueCallerBindingRead", `var v item; p:=&v; for range xs {go func(){v.N++}(); f:=p.Val; _=f; break}`, 1},
	{"valueCallerGoBindingRead", `var v item; p:=&v; for range xs {go func(){v.N++}(); go p.Val()}`, 1},
	{"valueBoundCompare", `var v item; p:=&v; f:=p.Val; for range xs {go func(){_=f==nil}(); v.N++}`, 0},
	{"valueCurrentContents", `n:=0; v:=valueReceiver{P:&n}; p:=&v; v=valueReceiver{}; for range xs {go p.Val(); n++}`, 0},
	{"valueRetainedContents", `n:=0; v:=valueReceiver{P:&n}; p:=&v; for range xs {go p.Val(); n++}`, 1},
	{"valueCopyNotStorage", `n:=0; v:=valueReceiver{P:&n}; p:=&v; for range xs {go p.Val(); v=valueReceiver{}}`, 0},
	{"pointerCallbackOpaque", `n:=0; v:=callbackReceiver{func(){n++}}; p:=&v; for range xs {go func(){p.Touch()}()}`, 0},
	{"pointerDirectCallbackOpaque", `n:=0; v:=callbackReceiver{func(){n++}}; p:=&v; for range xs {go p.Touch()}`, 0},
	{"valueCallbackOpaque", `n:=0; v:=valueReceiver{F:func(){n++}}; p:=&v; for range xs {go p.Val()}`, 0},
	{"addressStarHeaderOnly", `var v item; p:=&v; for range xs {go func(){_=&*p}(); v.N++}`, 0},
	{"addressFieldHeaderOnly", `var v item; p:=&v; for range xs {go func(){_=&p.N}(); v.N++}`, 0},
	{"addressHeaderWrite", `var v item; p:=&v; for range xs {go func(){_=&p.N}(); p=&item{}}`, 1},
	{"addressIndexRead", `a:=[2]int{}; p:=&a; i:=0; for range xs {go func(){_=&p[i]}(); i++}`, 1},
	{"addressIndexHeaderOnly", `a:=[2]int{}; p:=&a; for range xs {go func(){_=&p[0]}(); a[0]++}`, 0},
	{"storageFieldHeaderOnly", `var v item; s:=struct{P *item}{&v}; for range xs {go func(){_=s.P.Use==nil}(); v.N++}`, 0},
	{"storageFieldHeaderWrite", `var v item; s:=struct{P *item}{&v}; for range xs {go func(){_=s.P.Use==nil}(); s.P=&item{}}`, 1},
	{"indirectFieldLoad", `var v item; s:=struct{P *item}{&v}; p:=&s; for range xs {go func(){_=p.P.Use==nil}(); s.P=&item{}}`, 1},
	{"indexPointerLoad", `a:=[1]*item{{}}; for range xs {go func(){_=a[0].Use==nil}(); a[0]=&item{}}`, 1},
	{"genericHeaderOnly", `var v generic[int]; p:=&v; for range xs {go func(){_=p.Use==nil}(); v.N++}`, 0},
	{"genericValueCopy", `var v generic[int]; p:=&v; for range xs {go p.Val(); v.N++}`, 0},
	{"promotedHeaderOnly", `var v item; s:=wrapper{&v}; for range xs {go func(){_=s.Use==nil}(); v.N++}`, 0},
	{"promotedHeaderWrite", `var v item; s:=wrapper{&v}; for range xs {go func(){_=s.Use==nil}(); s=wrapper{&item{}}}`, 1},
	{"siblingHeaderRead", `var v item; p:=&v; for range xs {go func(){_=p.Use==nil}(); go func(){p=&item{}}()}`, 2},
	{"callerHeaderRead", `var v item; p:=&v; for range xs {go func(){p=&item{}}(); _=p.Use}`, 1},
	{"freshHeaderEpoch", `var old *item; for _, p:=range []*item{{},{}} {if old!=nil {go func(){_=old.Use==nil}()}; old=p; p=&item{}; _=p}`, 1},
}

func TestLoopMethodHeaderReads(t *testing.T) {
	for _, tc := range loopMethodHeaderCases {
		t.Run(tc.name, func(t *testing.T) {
			source := loopMethodHeaderPrelude + "func work(xs []int){" + tc.body + "}"
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "headers.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&types.Config{}).Check("example.com/headers", fset, []*ast.File{file}, nil); err != nil {
				t.Fatal(err)
			}
			for _, version := range []string{"go1.21", "go1.23"} {
				for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
					t.Run(version+"/"+mode, func(t *testing.T) {
						got := analyzeLoopRegression(t, source, version, mode)
						if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
							t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
						}
					})
				}
			}
		})
	}
}

func TestLoopMethodHeaderReadsPublicCompleteInfo(t *testing.T) {
	for _, version := range []string{"1.21", "1.23"} {
		t.Run(version, func(t *testing.T) {
			var source strings.Builder
			source.WriteString(loopMethodHeaderPrelude)
			for _, tc := range loopMethodHeaderCases {
				fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
			}
			dir := writeTypeModule(t, map[string]string{
				"go.mod":     "module example.com/headers\n\ngo " + version + "\n",
				"headers.go": source.String(),
			})
			findings, err := AnalyzeAST(dir, "./...")
			if err != nil {
				t.Fatal(err)
			}
			byFunction := map[string]int{}
			for _, finding := range findings {
				if finding.Rule == "type-info-unavailable" {
					t.Fatalf("fixture must have complete type information: %+v", finding)
				}
				if finding.Rule == "goroutine-in-loop" {
					byFunction[finding.Symbol]++
				}
			}
			for _, tc := range loopMethodHeaderCases {
				if byFunction[tc.name] != tc.want {
					t.Errorf("%s: got %d findings, want %d", tc.name, byFunction[tc.name], tc.want)
				}
			}
		})
	}
}
