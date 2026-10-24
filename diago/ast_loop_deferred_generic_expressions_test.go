package diago

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

var loopDeferredGenericExpressions = []struct {
	name, decl string
	want       int
}{
	{"sizeof type parameter", `func work[T any](xs []int,x T){v:=0;f:=func(){v++};defer func(){_=unsafe.Sizeof(func()T{f=func(){};return x}());f()}();for range xs{go func(){use(v)}()}}`, 1},
	{"alignof type parameter", `func work[T any](xs []int,x T){v:=0;f:=func(){v++};defer func(){_=unsafe.Alignof(func()T{f=func(){};return x}());f()}();for range xs{go func(){use(v)}()}}`, 1},
	{"offsetof variable struct", `func work[T any](xs []int,x T){v:=0;f:=func(){v++};defer func(){_=unsafe.Offsetof(func()struct{X T}{f=func(){};return struct{X T}{}}().X);f()}();for range xs{go func(){use(v)}()}}`, 1},
	{"len core array call evaluated", `func work[T ~[1]int](xs []int,x T){v:=0;f:=func(){v++};defer func(){_=len(func()T{f=func(){};return x}());f()}();for range xs{go func(){use(v)}()}}`, 0},
	{"cap core array call evaluated", `func work[T ~[1]int](xs []int,x T){v:=0;f:=func(){v++};defer func(){_=cap(func()T{f=func(){};return x}());f()}();for range xs{go func(){use(v)}()}}`, 0},
	{"len core array operand evaluated", `func work[T ~[1]int](xs []int,x T){defer func(){for range xs{a:=x;go func(){a[0]++}();_=len(a)}}()}`, 1},
	{"cap core array operand evaluated", `func work[T ~[1]int](xs []int,x T){defer func(){for range xs{a:=x;go func(){a[0]++}();_=cap(a)}}()}`, 1},
	{"len array of type parameter constant", `func work[T any](xs []int,x T){defer func(){for range xs{a:=[1]T{x};go func(){a[0]=x}();_=len(a)}}()}`, 0},
}

func TestLoopDeferredGenericExpressionModes(t *testing.T) {
	for _, tc := range loopDeferredGenericExpressions {
		t.Run(tc.name, func(t *testing.T) {
			prelude := "package sample\nfunc use(...any){}\n"
			if strings.Contains(tc.decl, "unsafe.") {
				prelude = "package sample\nimport \"unsafe\"\nfunc use(...any){}\n"
			}
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				got := analyzeDeferredExpressions(t, prelude+tc.decl, mode)
				if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
					t.Errorf("%s: got %d want %d: %+v", mode, n, tc.want, got)
				}
			}
		})
	}
}

func TestLoopDeferredGenericExpressionPublicGo123(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/genericexpressions\n\ngo 1.23\n", "use.go": "package expressions\nfunc use(...any){}\n"}
	for i, tc := range loopDeferredGenericExpressions {
		prelude := "package expressions\n"
		if strings.Contains(tc.decl, "unsafe.") {
			prelude += "import \"unsafe\"\n"
		}
		files[fmt.Sprintf("case%d.go", i)] = prelude + strings.Replace(tc.decl, "work[", fmt.Sprintf("case%d[", i), 1)
	}
	dir := writeTypeModule(t, files)
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile Go 1.23 generic fixtures: %v\n%s", err, out)
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if countRules(findings)["type-info-unavailable"] != 0 {
		t.Fatalf("type info unavailable: %+v", findings)
	}
	bySymbol := map[string]int{}
	for _, f := range findings {
		if f.Rule == "goroutine-in-loop" {
			bySymbol[f.Symbol]++
		}
	}
	for i, tc := range loopDeferredGenericExpressions {
		if n := bySymbol[fmt.Sprintf("case%d", i)]; n != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, n, tc.want)
		}
	}
}
