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

var loopDeferredExpressionCases = []struct {
	name, prelude, body string
	want                int
}{
	{"false and keeps launch writer", "", `v:=0; f:=func(){v++}; defer func(){_=false&&func()bool{f=func(){};return true}();for range 2{go f()}}()`, 1},
	{"true or keeps launch writer", "", `v:=0; f:=func(){v++}; defer func(){_=true||func()bool{f=func(){};return false}();for range 2{go f()}}()`, 1},
	{"unknown and joins skipped", "", `v:=0;f:=func(){v++};defer func(){_=b&&func()bool{f=func(){};return true}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unknown or joins skipped", "", `v:=0;f:=func(){v++};defer func(){_=b||func()bool{f=func(){};return false}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"constant true executes", "", `v:=0;f:=func(){v++};const yes=true;defer func(){_=yes&&func()bool{f=func(){};return true}();f()}();for range xs{go func(){use(v)}()}`, 0},
	{"constant false skips", "", `v:=0;const no=false;defer func(){_=no&&func()bool{v++;return true}()}();for range xs{go func(){use(v)}()}`, 0},
	{"shadow true remains unknown", "", `v:=0;f:=func(){v++};true:=b;defer func(){_=true&&func()bool{f=func(){};return true}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"true and overwrites", "", `v:=0;f:=func(){v++};defer func(){_=true&&func()bool{f=func(){};return true}();f()}();for range xs{go func(){use(v)}()}`, 0},
	{"false or overwrites", "", `v:=0;f:=func(){v++};defer func(){_=false||func()bool{f=func(){};return false}();f()}();for range xs{go func(){use(v)}()}`, 0},
	{"excluded physical and writer", "", `v:=0;defer func(){_=false&&func()bool{v++;return true}()}();for range xs{go func(){use(v)}()}`, 0},
	{"excluded physical or writer", "", `v:=0;defer func(){_=true||func()bool{v++;return false}()}();for range xs{go func(){use(v)}()}`, 0},
	{"possible physical writer", "", `v:=0;defer func(){_=b&&func()bool{v++;return true}()}();for range xs{go func(){use(v)}()}`, 1},
	{"excluded physical launch", "", `v:=0;defer func(){_=false&&func()bool{for range 2{go func(){v++}()};return true}()}()`, 0},
	{"left executes exactly once", "", `v:=0;f:=func(){v++};g:=func(){};defer func(){_=func()bool{f,g=g,f;return true}()&&false;f()}();for range xs{go func(){use(v)}()}`, 0},
	{"condition short circuit", "", `v:=0;f:=func(){v++};defer func(){if false&&func()bool{f=func(){};return true}(){};f()}();for range xs{go func(){use(v)}()}`, 1},
	{"condition physical exclusion", "", `v:=0;defer func(){if true||func()bool{v++;return false}(){}}();for range xs{go func(){use(v)}()}`, 0},
	{"left always executes", "", `v:=0;f:=func(){v++};defer func(){_=func()bool{f=func(){};return true}()&&false;f()}();for range xs{go func(){use(v)}()}`, 0},
	{"left evaluated before argument", "", `v:=0;f:=func(){v++};defer func(){_=func()bool{f=func(){};return true}()&&func()bool{f();return true}()}();for range xs{go func(){use(v)}()}`, 0},
	{"nested skipped rhs", "", `v:=0;f:=func(){v++};defer func(){_=b&&(false&&func()bool{f=func(){};return true}());f()}();for range xs{go func(){use(v)}()}`, 1},
	{"nested known result excludes outer rhs", "", `v:=0;f:=func(){v++};defer func(){_=(b&&false)&&func()bool{f=func(){};return true}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"nested or known result excludes outer rhs", "", `v:=0;f:=func(){v++};defer func(){_=(b||true)||func()bool{f=func(){};return false}();f()}();for range xs{go func(){use(v)}()}`, 1},
	{"nested inner execution remains", "", `v:=0;f:=func(){v++};defer func(){_=true&&(false||func()bool{f=func(){};return true}());f()}();for range xs{go func(){use(v)}()}`, 0},
	{"unknown then unconditional overwrite", "", `v:=0;f:=func(){v++};defer func(){_=b&&func()bool{f=func(){};return true}();f=func(){};f()}();for range xs{go func(){use(v)}()}`, 0},
	{"unsafe sizeof call", `import "unsafe"`, `v:=0;f:=func(){v++};defer func(){_=unsafe.Sizeof(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unsafe dot import", `import . "unsafe"`, `v:=0;f:=func(){v++};defer func(){_=Sizeof(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unsafe alignof call", `import u "unsafe"`, `v:=0;f:=func(){v++};defer func(){_=u.Alignof(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unsafe offsetof call", `import u "unsafe"`, `v:=0;f:=func(){v++};defer func(){_=u.Offsetof(func()struct{X int}{f=func(){};return struct{X int}{}}().X);f()}();for range xs{go func(){use(v)}()}`, 1},
	{"unsafe fixed operand physical exclusion", `import "unsafe"`, `v:=0;defer func(){_=unsafe.Sizeof(func()int{v++;return 0}())}();for range xs{go func(){use(v)}()}`, 0},
	{"shadow sizeof function", "", `v:=0;f:=func(){v++};Sizeof:=func(int)uintptr{return 0};defer func(){_=Sizeof(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 0},
	{"shadow unsafe selector", `import u "unsafe";var _ = u.Sizeof(0)`, `v:=0;f:=func(){v++};u:=struct{Sizeof func(int)uintptr}{func(int)uintptr{return 0}};defer func(){_=u.Sizeof(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 0},
	{"normal arguments execute", "", `v:=0;f:=func(){v++};consume:=func(int){};defer func(){consume(func()int{f=func(){};return 0}());f()}();for range xs{go func(){use(v)}()}`, 0},
	{"len literal physical exclusion", "", `defer func(){for range xs{v:=0;go func(){v++}();_=len([1]int{v})}}()`, 0},
	{"cap local array physical exclusion", "", `defer func(){for range xs{a:=[1]int{};go func(){a[0]++}();_=cap(a)}}()`, 0},
	{"len named pointer physical exclusion", "", `defer func(){for range xs{type A [1]int;var p *A;go func(){p=nil}();_=len(p)}}()`, 0},
	{"len array nested unsafe call disqualifies constant", `import "unsafe"`, `v:=0;f:=func(){v++};defer func(){_=len([2]uintptr{uintptr(func()int{f=func(){};return 0}()),unsafe.Sizeof(func()int{f();return 0}())});f()}();for range xs{go func(){use(v)}()}`, 0},
	{"len array conversion no execution", "", `v:=0;defer func(){_=len([1]int{v})}();for range xs{go func(){v++}()}`, 1},
	{"len pointer nil no execution", "", `v:=0;defer func(){_=len((*[1]int)(nil));_=v}();for range xs{go func(){v++}()}`, 1},
	{"len array called element", "", `v:=0;f:=func(){v++};defer func(){_=len([1]int{func()int{f=func(){};return 0}()});f()}();for range xs{go func(){use(v)}()}`, 0},
	{"cap array called element", "", `v:=0;f:=func(){v++};defer func(){_=cap([1]int{func()int{f=func(){};return 0}()});f()}();for range xs{go func(){use(v)}()}`, 0},
	{"len array function literal body", "", `v:=0;f:=func(){v++};defer func(){_=len([1]func(){func(){f=func(){}}});f()}();for range xs{go func(){use(v)}()}`, 1},
	{"len array receives", "", `v:=0;f:=func(){v++};defer func(){_=len([2]int{<-make(chan int,1),func()int{f=func(){};return 0}()});f()}();for range xs{go func(){use(v)}()}`, 0},
	{"shadow len array operand read", "", `defer func(){for range xs{v:=0;len:=func([1]int)int{return 0};go func(){v++}();_=len([1]int{v})}}()`, 1},
	{"shadow cap array operand read", "", `defer func(){for range xs{v:=0;cap:=func([1]int)int{return 0};go func(){v++}();_=cap([1]int{v})}}()`, 1},
	{"nested unsafe call disqualifies constant len", `import "unsafe"`, `defer func(){for range xs{v:=0;go func(){v++}();_=len([2]uintptr{uintptr(v),unsafe.Sizeof(func()int{return 0}())})}}()`, 1},
	{"nested unsafe call disqualifies constant cap", `import "unsafe"`, `defer func(){for range xs{v:=0;go func(){v++}();_=cap([2]uintptr{uintptr(v),unsafe.Alignof(func()int{return 0}())})}}()`, 1},
	{"saved callee before logical argument", "", `v:=0;f:=func(bool){v++};defer func(){f(true&&func()bool{f=func(bool){};return true}())}();for range xs{go func(){use(v)}()}`, 1},
	{"nested defer short circuit", "", `v:=0;f:=func(){v++};defer func(){defer func(bool){f()}(false&&func()bool{f=func(){};return true}())}();for range xs{go func(){use(v)}()}`, 1},
	{"shadow len executes", "", `v:=0;f:=func(){v++};len:=func([1]int)int{return 1};defer func(){_=len([1]int{func()int{f=func(){};return 0}()});f()}();for range xs{go func(){use(v)}()}`, 0},
	{"len slice executes", "", `v:=0;f:=func(){v++};defer func(){_=len([]int{func()int{f=func(){};return 0}()});f()}();for range xs{go func(){use(v)}()}`, 0},
}

func TestLoopDeferredExpressionModes(t *testing.T) {
	for _, tc := range loopDeferredExpressionCases {
		t.Run(tc.name, func(t *testing.T) {
			source := "package sample\n" + tc.prelude + "\nfunc use(...any){}\nfunc work(xs []int,b bool){" + tc.body + "}"
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				got := analyzeDeferredExpressions(t, source, mode)
				if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
					t.Errorf("%s: got %d want %d: %+v", mode, n, tc.want, got)
				}
			}
		})
	}
}

func TestLoopDeferredExpressionPublicGo123(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/expressions\n\ngo 1.23\n"}
	for i, tc := range loopDeferredExpressionCases {
		files[fmt.Sprintf("case%d.go", i)] = "package expressions\n" + tc.prelude + fmt.Sprintf("\nfunc case%d(xs []int,b bool){%s}\n", i, tc.body)
	}
	files["use.go"] = "package expressions\nfunc use(...any){}\n"
	dir := writeTypeModule(t, files)
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile Go 1.23 fixtures: %v\n%s", err, out)
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if countRules(findings)["type-info-unavailable"] != 0 {
		t.Fatalf("type information unavailable: %+v", findings)
	}
	bySymbol := map[string]int{}
	for _, f := range findings {
		if f.Rule == "goroutine-in-loop" {
			bySymbol[f.Symbol]++
		}
	}
	for i, tc := range loopDeferredExpressionCases {
		if n := bySymbol[fmt.Sprintf("case%d", i)]; n != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, n, tc.want)
		}
	}
}

// Real compiler/runtime controls: an outer len/cap is nonconstant if its array
// operand contains even an unevaluated call nested inside unsafe.Sizeof.
func TestLoopDeferredExpressionRuntimeOracle(t *testing.T) {
	source := `package main
import "unsafe"
func main(){
 n:=0
 tick:=func()int{n++;return 0}
 check:=func(want int){if n!=want{panic("evaluation count")}}
 _=unsafe.Sizeof(tick());check(0)
 _=unsafe.Alignof(tick());check(0)
 _=unsafe.Offsetof(func()struct{X int}{n++;return struct{X int}{}}().X);check(0)
 _=len([1]func(){func(){tick()}});check(0)
 var p *[1]int
 _=len([1]int{p[0]}) // constant: no nil dereference
 panics:=func(f func()){defer func(){if recover()==nil{panic("expected evaluated nil dereference")}}();f()}
 panics(func(){_=len([2]uintptr{uintptr(p[0]),unsafe.Sizeof(tick())})})
 check(0)
 _=cap([1]int{tick()});check(1)
 c:=make(chan int,1);c<-1
 _=len([1]int{<-c});if len(c)!=0{panic("receive was skipped")}
 _=len([2]uintptr{uintptr(tick()),unsafe.Sizeof(tick())});check(2)
 _=false&&func()bool{tick();return true}();check(2)
 _=true||func()bool{tick();return false}();check(2)
 _=func()bool{tick();return true}()&&false;check(3)
 if generic([1]int{},&n)==0{panic("missing runtime layout")};check(3)
 _=genericLen([1]int{},&n);check(4)
 _=genericCap([1]int{},&n);check(5)
}
func generic[T any](v T,n *int)uintptr{
 return unsafe.Sizeof(func()T{*n++;return v}()) +
 unsafe.Alignof(func()T{*n++;return v}()) +
 unsafe.Offsetof(func()struct{X T}{*n++;return struct{X T}{}}().X)
}
func genericLen[T ~[1]int](v T,n *int)int{return len(func()T{*n++;return v}())}
func genericCap[T ~[1]int](v T,n *int)int{return cap(func()T{*n++;return v}())}
`
	dir := writeTypeModule(t, map[string]string{"go.mod": "module example.com/oracle\n\ngo 1.23\n", "main.go": source})
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("runtime control: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

// Always type-check first, including syntax-only modes: no invalid-program
// recovery may accidentally provide evidence for an evaluation decision.
func analyzeDeferredExpressions(t testing.TB, source, mode string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	var flags parser.Mode
	if strings.Contains(mode, "lexical") {
		flags = parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(fset, "expressions.go", source, flags)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{GoVersion: "go1.23", Importer: importer.Default()}
	if _, err := conf.Check("example.com/expressions", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "partial") {
		info.Defs = nil
		info.Uses = nil
		info.Selections = nil
	} else if mode != "typed" {
		info = nil
	}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			scanLoops(loopEnv{findings: &findings, ctx: loopContext{pkg: "example.com/expressions", path: "expressions.go", fset: fset, fn: fn.Name.Name, types: info, file: file, decl: fn, goVersion: "go1.23"}, shade: map[string]bool{}}, fn.Body)
		}
	}
	return findings
}
