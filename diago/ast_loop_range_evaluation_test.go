package diago

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"strings"
	"testing"
)

var loopRangeEvaluationCases = []struct {
	name, body string
	want       int
}{
	{"named array", `for { type A [1]int; a:=A{}; go func(){a[0]++}(); for range a {}; break }`, 0},
	{"named array conversion", `for { type A [1]int; a:=A{}; go func(){a[0]++}(); for range A(a) {}; break }`, 0},
	{"global named array", `for { a:=rangeArray{}; go func(){a[0]++}(); for range a {}; break }`, 0},
	{"named pointer array", `for { type A [1]int; type P *A; var p P; go func(){p=nil}(); for range p {}; break }`, 0},
	{"local const call", `for { const N=1; v:=0; go func(){v++}(); for range [1]int{v+max(N,2)} {}; break }`, 0},
	{"shadow builtin call", `for { v:=0; len:=func([]int)int{v++;return 1}; go func(){use(v)}(); for range [1]int{len(nil)} {}; break }`, 1},
	{"array no variables", `for { a:=[1]int{}; go func(){a[0]++}(); for range a {}; break }`, 0},
	{"array key declaration", `for { a:=[1]int{}; go func(){a[0]++}(); for k:=range a {_=k}; break }`, 0},
	{"array key assignment", `for { a:=[1]int{}; go func(){a[0]++}(); var k int; for k=range a {_=k}; break }`, 0},
	{"array blank key", `for { a:=[1]int{}; go func(){a[0]++}(); for _=range a {}; break }`, 0},
	{"array trailing blank declaration", `for { a:=[1]int{}; go func(){a[0]++}(); for k,_:=range a {_=k}; break }`, 0},
	{"array two blanks", `for { a:=[1]int{}; go func(){a[0]++}(); for _,_=range a {}; break }`, 0},
	{"array trailing blank assignment", `for { a:=[1]int{}; go func(){a[0]++}(); var k int; for k,_=range a {_=k}; break }`, 0},
	{"array two variables", `for { a:=[1]int{}; go func(){a[0]++}(); for k,v:=range a {_,_=k,v}; break }`, 1},
	{"indexed array operand not evaluated", `for { a:=[1][1]int{}; k:=0; go func(){k++}(); for range a[k] {}; break }`, 0},
	{"dereference not evaluated", `for { var p *[1]int; go func(){p=nil}(); for range *p {}; break }`, 0},
	{"assertion not evaluated", `for { var v any; go func(){v=nil}(); for range v.([1]int) {}; break }`, 0},
	{"array value only", `for { a:=[1]int{}; go func(){a[0]++}(); for _,v:=range a {_=v}; break }`, 1},
	{"literal value not evaluated", `for { v:=0; go func(){v++}(); for range [1]int{v} {}; break }`, 0},
	{"literal value evaluated", `for { v:=0; go func(){v++}(); for _,n:=range [1]int{v} {_=n}; break }`, 1},
	{"nil pointer array", `for { var p *[1]int; go func(){p=nil}(); for range p {}; break }`, 0},
	{"nil pointer trailing blank", `for { var p *[1]int; go func(){p=nil}(); for k,_:=range p {_=k}; break }`, 0},
	{"pointer array conversion", `for { var p *[1]int; go func(){p=nil}(); for range (*[1]int)(p) {}; break }`, 0},
	{"array conversion", `for { a:=[1]int{}; go func(){a[0]++}(); for range [1]int(a) {}; break }`, 0},
	{"constant builtin", `for { v:=0; go func(){v++}(); for range [2]int{v,len([1]int{v})} {}; break }`, 0},
	{"constant numeric builtins", `for { v:=0; go func(){v++}(); for range [2]int{v,int(real(complex(max(1,2),0)))} {}; break }`, 0},
	{"nonconstant builtin", `for { v:=0; go func(){v++}(); for range [1]int{len([]int{v})} {}; break }`, 1},
	{"array returning call effects", `for { v:=0; go func(){use(v)}(); for range func()[1]int{v++;return [1]int{}}() {}; break }`, 1},
	{"pointer returning call effects", `for { v:=0; go func(){use(v)}(); for range func()*[1]int{v++;return nil}() {}; break }`, 1},
	{"array channel receive", `for { var c chan [1]int; go func(){c=nil}(); for range <-c {}; break }`, 1},
	{"function call effects", `for { v:=0; go func(){use(v)}(); for range [1]int{func()int{v++;return 0}()} {}; break }`, 1},
	{"function call trailing blank", `for { v:=0; go func(){use(v)}(); for k,_:=range [1]int{func()int{v++;return 0}()} {_=k}; break }`, 1},
	{"channel receive", `for { c:=make(chan int,1); go func(){c=nil}(); for range [1]int{<-c} {}; break }`, 1},
	{"literal body calls do not count", `for { v:=0; go func(){v++}(); for range [1]func(){func(){use(v)}} {}; break }`, 0},
	{"literal body launches not discovered", `for { v:=0; for range [1]func(){func(){go func(){use(v)}();v++}} {}; break }`, 0},
	{"literal defer not discovered", `for { v:=0; for range [1]func(){func(){defer use(v)}} {}; break }`, 0},
	{"literal body not seeded", `for { v:=0; go func(){v++}(); for range [1]func(){func(){use(v)}} {}; break }`, 0},
	{"range body still executes", `for { v:=0; go func(){use(v)}(); for k:=range [1]int{} {v++;_=k}; break }`, 1},
	{"fresh range key", `for k:=range [2]int{} {go func(){k++}()}`, 0},
	{"assigned range key", `var k int; for k=range [2]int{} {go func(){use(k)}()}`, 1},
	{"scalar key type", `for k:=range [2]*int{} {go use(k)}`, 0},
	{"slice still evaluated", `for { a:=[]int{0}; go func(){a=nil}(); for range a {}; break }`, 1},
	{"map still evaluated", `for { a:=map[int]int{}; go func(){a=nil}(); for range a {}; break }`, 1},
	{"string still evaluated", `for { a:="x"; go func(){a="y"}(); for range a {}; break }`, 1},
	{"integer still evaluated", `for { a:=1; go func(){a++}(); for range a {}; break }`, 1},
	{"snapshot unchanged", `v:=0; for _,f:=range [1]func(){func(){use(v)}} {go f();v++}`, 1},
}

func TestLoopRangeEvaluationModes(t *testing.T) {
	for _, tc := range loopRangeEvaluationCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"type rangeArray [1]int\nfunc work(xs []int){"+tc.body+"}", "go1.23", mode)
				if n := countRules(got)["defer-in-loop"]; n != 0 {
					t.Errorf("%s: discovered an unevaluated literal's defer: %+v", mode, got)
				}
				if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
					t.Errorf("%s: got %d want %d: %+v", mode, n, tc.want, got)
				}
			}
		})
	}
}

func TestLoopRangeEvaluationGenericModes(t *testing.T) {
	for _, tc := range []struct {
		decl string
		want int
	}{
		{`func work[T ~[1]int](a T) {for {go func(){a[0]++}();for range a {};break}}`, 1},
		{`func work[T any](v T) {for {a:=[1]T{v};go func(){a[0]=v}();for range a {};break}}`, 0},
		{`func work[T any](v T) {for {a:=(*[1]T)(nil);go func(){a=nil}();for range a {};break}}`, 0},
	} {
		for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
			got := analyzeLoopRegression(t, "package sample\n"+tc.decl, "go1.23", mode)
			if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
				t.Errorf("%s: %s: got %d want %d: %+v", mode, tc.decl, n, tc.want, got)
			}
		}
	}
}

func TestLoopRangeEvaluationPublicGo123(t *testing.T) {
	var source strings.Builder
	source.WriteString("package ranges\nfunc use(...any){}\ntype rangeArray [1]int\n")
	for i, tc := range loopRangeEvaluationCases {
		fmt.Fprintf(&source, "func case%d(xs []int){%s}\n", i, tc.body)
	}
	source.WriteString(`func generic[T ~[1]int](a T) {for {go func(){a[0]++}();for range a {};break}}
func element[T any](v T) {for {a:=[1]T{v};go func(){a[0]=v}();for range a {};break}}
`)
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/ranges\n\ngo 1.23\n", "ranges.go": source.String(),
	})
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Go 1.23 fixture compile: %v\n%s", err, out)
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if countRules(findings)["type-info-unavailable"] != 0 {
		t.Fatalf("fixture lost type information: %+v", findings)
	}
	bySymbol := map[string]int{}
	for _, f := range findings {
		if f.Rule == "goroutine-in-loop" {
			bySymbol[f.Symbol]++
		}
	}
	for i, tc := range loopRangeEvaluationCases {
		if got := bySymbol[fmt.Sprintf("case%d", i)]; got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
	if bySymbol["generic"] != 1 || bySymbol["element"] != 0 {
		t.Errorf("generic len distinction: %v", bySymbol)
	}
}

// Compile a value-len fixture first, then ask the Go 1.23 type checker whether
// the very same len is a constant. This catches both invalid test snippets and
// incorrect assumptions about conversions, built-ins, and function literals.
func TestLoopRangeConstantLenOracle(t *testing.T) {
	for _, tc := range []struct {
		name, prelude, expr string
		constant            bool
	}{
		{"array", "var a [1]int", "a", true},
		{"named array", "type A [1]int; var a A", "a", true},
		{"constant declaration", "const N=2; var v int", "[1]int{v+max(N,1)}", true},
		{"constant real", "var v int", "[1]int{v+int(real(complex(1,2)))}", true},
		{"nonconstant real", "var v complex128", "[1]float64{real(v)}", false},
		{"nonconstant min", "var v int", "[1]int{min(v,1)}", false},
		{"shadow builtin", "min:=func(int,int)int{return 0}", "[1]int{min(1,2)}", false},
		{"pointer", "var p *[1]int", "p", true},
		{"conversion", "var p *[1]int", "(*[1]int)(p)", true},
		{"nonconstant conversion", "var v int", "[1]int{int(v)}", true},
		{"constant builtin", "var v int", "[1]int{len([1]int{v})}", true},
		{"nonconstant builtin", "var v int", "[1]int{len([]int{v})}", false},
		{"call", "f:=func()int{return 0}", "[1]int{f()}", false},
		{"receive", "var c chan int", "[1]int{<-c}", false},
		{"literal body", "var v int", "[1]func(){func(){println(v)}}", true},
		{"unsafe constant", "var v int", "[1]uintptr{unsafe.Sizeof(v)}", true},
		{"unsafe nested call", "f:=func()int{return 0}", "[1]uintptr{unsafe.Sizeof(f())}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				testRangeEvaluationPredicate(t, tc.prelude, tc.expr, tc.constant, mode)
			}
			for _, constant := range []bool{false, true} {
				kind := "var"
				if constant {
					kind = "const"
				}
				imports := ""
				if strings.Contains(tc.expr, "unsafe.") {
					imports = "import \"unsafe\"\n"
				}
				source := "package p\n" + imports + "func work(){" + tc.prelude + ";" + kind + " _ = len(" + tc.expr + ")}"
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, "oracle.go", source, 0)
				if err != nil {
					t.Fatal(err)
				}
				conf := types.Config{GoVersion: "go1.23", Importer: importer.Default()}
				_, err = conf.Check("p", fset, []*ast.File{file}, nil)
				if (err == nil) != (!constant || tc.constant) {
					t.Fatalf("%s len: constant=%v: %v", kind, tc.constant, err)
				}
			}
		})
	}
}

func testRangeEvaluationPredicate(t *testing.T, prelude, expr string, constant bool, mode string) {
	t.Helper()
	imports := ""
	if strings.Contains(expr, "unsafe.") {
		imports = "import \"unsafe\"\n"
	}
	source := "package p\n" + imports + "func work(){" + prelude + "; for k,_:=range " + expr + "{_=k}}"
	fset := token.NewFileSet()
	var flags parser.Mode
	if strings.Contains(mode, "lexical") {
		flags = parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(fset, "range.go", source, flags)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{GoVersion: "go1.23", Importer: importer.Default()}
	if _, err := conf.Check("p", fset, []*ast.File{file}, info); err != nil {
		t.Fatalf("%s valid range fixture: %v", mode, err)
	}
	if strings.HasPrefix(mode, "partial") {
		info.Defs, info.Uses, info.Selections = nil, nil, nil
	} else if mode != "typed" {
		info = nil
	}
	fn := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	x := &loopIndex{
		ctx: loopContext{fset: fset, goVersion: "go1.23", file: file, decl: fn, types: info}, fresh: true,
		ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
		parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	x.node(fn.Body)
	rng := fn.Body.List[len(fn.Body.List)-1].(*ast.RangeStmt)
	if evaluated := x.rangeExpressionEvaluated(rng); evaluated == constant {
		t.Fatalf("%s: range evaluated=%v, constant len=%v", mode, evaluated, constant)
	}
	a := loopFlowAST{index: x, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}}
	adapted := a.stmt(rng).(*ast.RangeStmt)
	if constant {
		key := x.ids[rng.Key.(*ast.Ident)]
		if id, ok := key.typeExpr.(*ast.Ident); !ok || id.Name != "int" || key.birth != x.regions[rng] {
			t.Fatalf("%s: unevaluated operand lost fresh integer key", mode)
		}
		for _, access := range x.direct {
			if access.node.Pos() >= rng.X.Pos() && access.node.End() <= rng.X.End() {
				t.Fatalf("%s: indexed an unevaluated operand read", mode)
			}
		}
		if _, marker := adapted.X.(*ast.BasicLit); !marker || adapted.X == rng.X || len(a.entries) != 0 {
			t.Fatalf("%s: unevaluated X exposed to CFG evaluation", mode)
		}
	} else if adapted.X != rng.X || a.entries[rng.X] == nil {
		t.Fatalf("%s: evaluated range lost original snapshot entry", mode)
	}
}
