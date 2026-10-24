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

// Exercise the public scanLoops call directly: the effective file language
// version must be supplied by the caller, independently of the test toolchain.
func analyzeLoopRegression(t testing.TB, source, version, mode string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	var flags parser.Mode
	if strings.Contains(mode, "lexical") {
		flags = parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(fset, "loops.go", source, flags)
	if err != nil {
		t.Fatal(err)
	}
	var info *types.Info
	if mode == "typed" || strings.HasPrefix(mode, "partial") {
		info = &types.Info{
			Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
			Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		}
		conf := types.Config{}
		if _, err := conf.Check("example.com/loops", fset, []*ast.File{file}, info); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(mode, "partial") {
			// Keep unrelated Uses so a nonempty Info cannot be treated as a
			// certificate that all captures have been resolved.
			for id := range info.Uses {
				if id.Name != "use" && id.Name != "eval" {
					delete(info.Uses, id)
				}
			}
			info.Defs = nil
			info.Selections = nil
		}
	}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			scanLoops(loopEnv{findings: &findings, ctx: loopContext{
				pkg: "example.com/loops", path: "loops.go", fset: fset,
				fn: fn.Name.Name, types: info, file: file, decl: fn, goVersion: version,
			}, shade: map[string]bool{}}, fn.Body)
		}
	}
	return findings
}

const loopTestPrelude = `package sample
func use(...any) {}
func eval(f func()) int { f(); return 0 }
func read(p *int) int { return *p }
type item struct { N int }
func (*item) Use() {}
`

func TestLoopAnalyzerRegressions(t *testing.T) {
	tests := []struct {
		name, body, version string
		want                int
	}{
		{"unknown is legacy", `for i:=0;i<3;i++ { go func(){use(i)}() }`, "", 1},
		{"legacy three clause", `for i:=0;i<3;i++ { go func(){use(i)}() }`, "1.21", 1},
		{"legacy immutable header", `for i:=0;i<3; {go func(){use(i)}(); break}`, "1.21", 0},
		{"reused immutable header", `var i int; for i=0;i<3; {go func(){use(i)}(); break}`, "1.22", 0},
		{"fresh three clause", `for i:=0;i<3;i++ { go func(){use(i)}() }`, "1.22", 0},
		{"fresh range", `for _,v:=range xs { go func(){use(v)}() }`, "go1.22", 0},
		{"legacy range", `for _,v:=range xs { go func(){use(v)}() }`, "go1.21", 1},
		{"reused range", `var v int; for _,v=range xs { go func(){use(v)}() }`, "1.22", 1},
		{"reused three clause", `var i int; for i=0;i<3;i++ { go func(){use(i)}() }`, "1.22", 1},
		{"value argument", `var i int; for i=0;i<3;i++ { go use(i) }`, "1.22", 0},
		{"direct address", `var i int; for i=0;i<3;i++ { go use(&i) }`, "1.22", 1},
		{"fresh address", `for i:=0;i<3;i++ { go use(&i) }`, "1.22", 0},
		{"sync closure argument", `var i int; for i=0;i<3;i++ { go use(eval(func(){use(i)})) }`, "1.22", 0},
		{"sync address argument", `var i int; for i=0;i<3;i++ { go use(read(&i)) }`, "1.22", 0},
		{"sync named closure argument", `var i int; f:=func(){use(i)}; for i=0;i<3;i++ { go use(eval(f)) }`, "1.22", 0},
		{"escaping closure argument", `var i int; for i=0;i<3;i++ { go use(func(){use(i)}) }`, "1.22", 1},
		{"escaping named argument", `var i int; f:=func(){use(i)}; for i=0;i<3;i++ { go use(f) }`, "1.22", 1},
		{"composite escaping closure", `var i int; for i=0;i<3;i++ { go use([]func(){func(){use(i)}}) }`, "1.22", 1},
		{"outer mutable capture", `i:=0; for _,v:=range xs { go func(){use(i)}(); i++; _=v }`, "1.22", 1},
		{"outer immutable capture", `i:=0; for _,v:=range xs { go func(){use(i)}(); _=v }`, "1.22", 0},
		{"no loop variables", `for { go use() }`, "1.22", 0},
		{"condition loop capture", `i:=0; for i<3 { go func(){use(i)}(); i++ }`, "1.22", 1},
		{"explicit pointer receiver", `var v item; for _,v=range []item{{}} { go (&v).Use() }`, "1.22", 1},
		{"pointer receiver", `var v item; for _,v=range []item{{}} { go v.Use() }`, "1.22", 1},
		{"fresh pointer receiver", `for _,v:=range []item{{}} { go v.Use() }`, "1.22", 0},
		{"field address", `var v item; for _,v=range []item{{}} { go use(&v.N) }`, "1.22", 1},
		{"parenthesized literal", `var i int; for i=0;i<3;i++ { go (func(){use(i)})() }`, "1.22", 1},
		{"parenthesized named", `var i int; f:=func(){use(i)}; for i=0;i<3;i++ { go (f)() }`, "1.22", 1},
		{"named inside loop", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; go f() }`, "1.22", 1},
		{"named shadow", `var i int; f:=func(){use(i)}; _=f; for i=0;i<3;i++ { f:=func(){}; go f() }`, "1.22", 0},
		{"same line overwrite", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; f=func(){}; go f() }`, "1.22", 0},
		{"later overwrite", `var i int; for i=0;i<3;i++ { f:=func(){use(i)}; go f(); f=func(){}; _=f }`, "1.22", 1},
		{"overwrite before loop", `var i int; f:=func(){use(i)}; f=func(){}; for i=0;i<3;i++ { go f() }`, "1.22", 0},
		{"captured function binding", `var i int; f:=func(){use(i)}; g:=func(){f()}; for i=0;i<3;i++ {go g()}`, "1.22", 1},
		{"captured function current binding", `var i int; f:=func(){use(i)}; g:=func(){f()}; f=func(){}; for i=0;i<3;i++ {go g()}`, "1.22", 0},
		{"captured pointer alias", `var i int; p:=&i; for i=0;i<3;i++ {go func(){use(p)}()}`, "1.22", 1},
		{"recursive function binding", `var f func(); f=func(){if false {f()}}; for range xs {go f()}`, "1.22", 0},
		{"named alias", `var i int; f:=func(){use(i)}; g:=f; f=func(){}; for i=0;i<3;i++ { go g() }`, "1.22", 1},
		{"conditional overwrite retains capture", `var i int; f:=func(){use(i)}; for i=0;i<3;i++ { if i>0 {f=func(){}}; go f() }`, "1.22", 1},
		{"all branches overwrite", `var i int; f:=func(){use(i)}; for i=0;i<3;i++ { if i>0 {f=func(){}} else {f=func(){}}; go f() }`, "1.22", 0},
		{"loop carried binding", `var i int; f:=func(){}; for i=0;i<3;i++ {go f(); f=func(){use(i)}}`, "1.22", 1},
		{"loop carried alias", `var i int; f:=func(){}; g:=f; for i=0;i<3;i++ {go g(); g=f; f=func(){use(i)}}`, "1.22", 1},
		{"loop carried safe overwrite", `var i int; f:=func(){}; for i=0;i<3;i++ {f=func(){}; go f(); f=func(){use(i)}}`, "1.22", 0},
		{"switch overwrite alternatives", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; switch i {case 0: f=func(){} }; go f()}`, "1.22", 1},
		{"switch all overwrite", `var i int; for i=0;i<3;i++ {f:=func(){use(i)}; switch i {case 0: f=func(){}; default: f=func(){} }; go f()}`, "1.22", 0},
		{"different loop binding", `i:=0; f:=func(){use(i)}; for i:=0;i<3;i++ { go f(); _=i }`, "1.21", 0},
		{"if init condition capture", `var i int; for i=0;i<3;i++ { go func(){if j:=0; i>j {}}() }`, "1.22", 1},
		{"nested for initializer scope", `var i int; for i=0;i<3;i++ { go func(){for i:=0;i<1;i++ {}; use(i)}() }`, "1.22", 1},
		{"selector name is not capture", `s:=struct{i int}{}; var i int; for i=0;i<3;i++ { go func(){use(s.i)}() }`, "1.22", 0},
		{"block shadow", `var i int; for i=0;i<3;i++ { {i:=2; go func(){use(i)}()} }`, "1.22", 0},
		{"named result shadows", `var i int; for i=0;i<3;i++ { go func()(i int){use(i); return}() }`, "1.22", 0},
		{"parameter shadows", `var i int; for i=0;i<3;i++ { go func(i int){use(i)}(i) }`, "1.22", 0},
		{"copy idiom", `for i:=0;i<3;i++ { i:=i; go func(){use(i)}() }`, "1.21", 0},
		{"nested shadowed parameter", `var i int; for i=0;i<3;i++ { go func(i int){go func(){use(i)}()}(i) }`, "1.22", 0},
		{"nested exclusive fresh mutation", `for _,v:=range xs { go func(){go func(){v++}()}() }`, "1.22", 0},
		{"nested true capture", `var i int; for i=0;i<3;i++ { go func(){go func(){use(i)}()}() }`, "1.22", 2},
		{"fresh mutated after launch", `for _,v:=range xs { go func(){use(v)}(); v++ }`, "1.22", 1},
		{"fresh mutated before launch", `for _,v:=range xs { v++; go func(){use(v)}() }`, "1.22", 0},
		{"fresh closure exclusive mutation", `for _,v:=range xs { go func(){v++}() }`, "1.22", 0},
		{"fresh closure write parent read", `for _,v:=range xs { go func(){v++}(); use(v) }`, "1.22", 1},
		{"fresh closure write for copy", `for i:=0;i<3;i++ { go func(){i++}() }`, "1.22", 1},
		{"fresh sibling goroutines", `for _,v:=range xs { go func(){v++}(); go func(){use(v)}() }`, "1.22", 2},
		{"fresh local body mutation", `for range xs { v:=0; go func(){use(v)}(); v++ }`, "1.22", 1},
		{"fresh captured across inner loop", `for _,v:=range xs { for j:=0;j<3;j++ {go func(){v++}()} }`, "1.22", 1},
		{"fresh sync literal mutation after", `for _,v:=range xs { go func(){use(v)}(); (func(){v++})() }`, "1.22", 1},
		{"fresh sync named mutation after", `for _,v:=range xs { f:=func(){v++}; go func(){use(v)}(); f() }`, "1.22", 1},
		{"fresh sync named mutation before", `for _,v:=range xs { f:=func(){v++}; f(); go func(){use(v)}() }`, "1.22", 0},
		{"struct field key is not capture", `var i int; for i=0;i<3;i++ { go func(){use(struct{i int}{i:0})}() }`, "1.22", 0},
		{"map key is capture", `var i int; for i=0;i<3;i++ { go func(){use(map[int]int{i:0})}() }`, "1.22", 1},
		{"label is not capture", `var i int; for i=0;i<3;i++ { go func(){i: for {break i}}() }`, "1.22", 0},
		{"blank binding does not escape", `var i int; f:=func(){use(i)}; _=f; for i=0;i<3;i++ {go use(struct{f int}{f:0})}`, "1.22", 0},
		{"switch clause shadow", `var i int; for i=0;i<3;i++ { go func(){switch {case true: i:=0; use(i); default: use(i)}}() }`, "1.22", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					source := loopTestPrelude + "func work(xs []int) {" + tc.body + "}"
					got := analyzeLoopRegression(t, source, tc.version, mode)
					if counts := countRules(got); counts["goroutine-in-loop"] != tc.want || counts["defer-in-loop"] != 0 {
						t.Fatalf("got %v, want goroutine-in-loop=%d: %+v", counts, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopAnalyzerFunctionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`for range xs { defer use() }`, 1},
		{`for range xs { go func(){defer use()}() }`, 0},
		{`for range xs { (func(){defer use()})() }`, 0},
		{`for range xs { for range func(yield func(int)bool){defer use(); yield(0)} {} }`, 0},
		{`for range xs { for range (func(yield func(int)bool){defer use(); yield(0)}) {} }`, 0},
		{`for range xs { for range func(yield func(int)bool){for range xs {defer use()}; yield(0)} {} }`, 1},
		{`for range xs { for range func(yield func(int)bool){yield(0)} {defer use()} }`, 1},
	} {
		for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
			got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.23", mode)
			if n := countRules(got)["defer-in-loop"]; n != tc.want {
				t.Errorf("%s: %s: got %d defers, want %d", mode, tc.body, n, tc.want)
			}
		}
	}
}

func TestLoopAnalyzerTypedReceivers(t *testing.T) {
	for _, tc := range []struct{ declarations, body string }{
		{`type value struct{}; func (value) Use(){}`, `var v value; for _,v=range []value{{}} {go v.Use()}`},
		{``, `var v *item; for _,v=range []*item{{}} {go v.Use()}`},
		{``, `var v []int; for _,v=range [][]int{{0}} {go use(&v[0])}`},
		{``, `var v *item; for _,v=range []*item{{}} {go use(&v.N)}`},
		{``, `var v item; for _,v=range []item{{}} {go use(v.N)}`},
	} {
		got := analyzeLoopRegression(t, loopTestPrelude+tc.declarations+"\nfunc work(){"+tc.body+"}", "1.22", "typed")
		if len(got) != 0 {
			t.Errorf("%s: unexpected findings: %+v", tc.body, got)
		}
	}
}

// Parse/check outside the measured section. The old named-worker resolver
// rescanned the entire loop body at every launch, making this quadratic.
func BenchmarkLoopAnalyzerNamedWorker(b *testing.B) {
	for _, captured := range []bool{false, true} {
		for _, n := range []int{100, 1000, 10000} {
			b.Run(fmt.Sprintf("captured=%v/%d", captured, n), func(b *testing.B) {
				definition := ""
				if captured {
					definition = "worker:=func(){_ = i};"
				}
				source := "package sample; func worker(){}; func work(){for i:=0;i<3;i++{" + definition + strings.Repeat("go worker();", n) + "}}"
				fset := token.NewFileSet()
				file, err := parser.ParseFile(fset, "loops.go", source, 0)
				if err != nil {
					b.Fatal(err)
				}
				body := file.Decls[1].(*ast.FuncDecl).Body
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var findings []ASTFinding
					scanLoops(loopEnv{findings: &findings, ctx: loopContext{fset: fset, goVersion: "1.21"}}, body)
				}
			})
		}
	}
}
