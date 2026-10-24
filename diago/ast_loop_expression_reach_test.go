package diago

import (
	"fmt"
	"os/exec"
	"testing"
)

// Mutable function variables intentionally take the source-global fallback
// summary path, rather than the known immutable literal-callee path.
var loopExpressionReachCases = []struct {
	name, prelude, body string
	want                int
}{
	{"mutable assignment constant len", "", `x:=0;var f func();f=func(){_=len([1]int{x})};for range 2{go f();x++}`, 0},
	{"mutable declaration constant len", "", `x:=0;var f func()=func(){_=len([1]int{x})};for range 2{go f();x++}`, 0},
	{"mutable assignment false and", "", `x:=0;var f func();f=func(){_=false&&x>0};for range 2{go f();x++}`, 0},
	{"true or", "", `x:=0;var f func();f=func(){_=true||x>0};for range 2{go f();x++}`, 0},
	{"nested logical", "", `x:=0;var f func();f=func(){_=(b&&false)&&(x>0);_=(b||true)||(x>0);_=b&&(false&&x>0)};for range 2{go f();x++}`, 0},
	{"executed rhs", "", `x:=0;var f func();f=func(){_=true&&x>0};for range 2{go f();x++}`, 1},
	{"unknown rhs", "", `x:=0;var f func();f=func(){_=b&&x>0};for range 2{go f();x++}`, 1},
	{"nested executed rhs", "", `x:=0;var f func();f=func(){_=true&&(false||x>0)};for range 2{go f();x++}`, 1},
	{"left always executes", "", `x:=0;var f func();f=func(){_=x>0&&false};for range 2{go f();x++}`, 1},
	{"constant cap", "", `x:=0;var f func();f=func(){_=cap([1]int{x})};for range 2{go f();x++}`, 0},
	{"nil pointer array", "", `var p *[1]int;var f func();f=func(){_=len(p);_=cap(p)};for range 2{go f();p=nil}`, 0},
	{"nil pointer index", "", `var p *[1]int;var f func();f=func(){_=len([1]int{p[0]})};for range 2{go f();p=nil}`, 0},
	{"unsafe alias sizeof", `import u "unsafe"`, `x:=0;var f func();f=func(){_=u.Sizeof(x)};for range 2{go f();x++}`, 0},
	{"unsafe alias alignof", `import u "unsafe"`, `x:=0;var f func();f=func(){_=u.Alignof(x)};for range 2{go f();x++}`, 0},
	{"unsafe alias offsetof", `import u "unsafe"`, `x:=struct{N int}{};var f func();f=func(){_=u.Offsetof(x.N)};for range 2{go f();x.N++}`, 0},
	{"unsafe dot sizeof", `import . "unsafe"`, `x:=0;var f func();f=func(){_=Sizeof(x)};for range 2{go f();x++}`, 0},
	{"array element call executes", "", `x:=0;var f func();f=func(){_=len([1]int{func()int{return x}()})};for range 2{go f();x++}`, 1},
	{"array returning call executes", "", `x:=0;var f func();f=func(){_=len(func()[1]int{x++;return [1]int{}}())};for range 2{go f();x++}`, 1},
	{"shadow len executes", "", `x:=0;len:=func([1]int)int{return 1};var f func();f=func(){_=len([1]int{x})};for range 2{go f();x++}`, 1},
	{"shadow sizeof executes", "", `x:=0;Sizeof:=func(int)uintptr{return 0};var f func();f=func(){_=Sizeof(x)};for range 2{go f();x++}`, 1},
	{"shadow unsafe selector executes", `import u "unsafe";var _=u.Sizeof(0)`, `x:=0;u:=struct{Sizeof func(int)uintptr}{func(int)uintptr{return 0}};var f func();f=func(){_=u.Sizeof(x)};for range 2{go f();x++}`, 1},
	{"literal construction not execution", "", `x:=0;var f func();f=func(){_=len([1]func(){func(){x++}})};for range 2{go f();x++}`, 0},
	{"skipped literal call launch", "", `x:=0;_=false&&func()bool{for range 2{go func(){x++}()};return true}()`, 0},
	{"unsafe literal call launch", `import u "unsafe"`, `x:=0;_=u.Sizeof(func()int{for range 2{go func(){x++}()};return 0}())`, 0},
	{"unevaluated function literal body", "", `x:=0;_=len([1]func(){func(){for range 2{go func(){x++}()}}})`, 0},
	{"skipped call no physical write", "", `x:=0;for range 2{go func(){_=x}();_=false&&func()bool{x++;return true}()}`, 0},
	{"unsafe call no physical write", `import u "unsafe"`, `x:=0;for range 2{go func(){_=x}();_=u.Sizeof(func()int{x++;return 0}())}`, 0},
	{"synchronous argument not capture", "", `x:=0;f:=func(int){};for range 2{go f(func()int{return x}());x++}`, 0},
	{"skipped argument read", "", `x:=0;f:=func(bool){};for range 2{go f(false&&x>0);x++}`, 0},
	{"executed literal called normally", "", `x:=0;var f func();f=func(){func(){_=x}()};for range 2{go f();x++}`, 1},
	{"skipped nested literal capture", "", `x:=0;var f func();f=func(){_=false&&func()bool{x++;return true}()};for range 2{go f();x++}`, 0},
	{"unsafe nested literal capture", `import u "unsafe"`, `x:=0;var f func();f=func(){_=u.Sizeof(func()int{x++;return 0}())};for range 2{go f();x++}`, 0},
	{"known logical result keeps left execution", "", `x:=0;var f func();f=func(){_=(func()bool{x++;return b}()&&false)&&x>0};for range 2{go f();x++}`, 1},
	{"nested unsafe call makes len evaluated", `import u "unsafe"`, `x:=0;var f func();f=func(){_=len([2]uintptr{uintptr(x),u.Sizeof(func()int{return x}())})};for range 2{go f();x++}`, 1},
	{"ordinary literal body launches", "", `x:=0;func(){for range 2{go func(){x++}()}}()`, 1},
	{"unsafe operand does not overwrite binding", `import u "unsafe"`, `x:=0;f:=func(){x++};defer func(){_=u.Sizeof(func()int{f=func(){};return 0}());for range 2{go f()}}()`, 1},
	{"skipped operand does not overwrite binding", "", `x:=0;f:=func(){x++};defer func(){_=true||func()bool{f=func(){};return false}();for range 2{go f()}}()`, 1},
	{"unknown operand keeps skipped binding", "", `x:=0;f:=func(){x++};defer func(){_=b&&func()bool{f=func(){};return true}();for range 2{go f()}}()`, 1},
}

func TestLoopExpressionReachModes(t *testing.T) {
	for _, tc := range loopExpressionReachCases {
		t.Run(tc.name, func(t *testing.T) {
			source := "package sample\n" + tc.prelude + "\nfunc work(b bool){" + tc.body + "}"
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				findings := analyzeDeferredExpressions(t, source, mode)
				if n := countRules(findings)["goroutine-in-loop"]; n != tc.want {
					t.Errorf("%s: got %d want %d: %+v", mode, n, tc.want, findings)
				}
			}
		})
	}
}

func TestLoopExpressionReachPublic(t *testing.T) {
	for _, version := range []string{"1.22", "1.23", "1.24"} {
		t.Run(version, func(t *testing.T) {
			files := map[string]string{"go.mod": "module example.com/expressionreach\n\ngo " + version + "\n"}
			for i, tc := range loopExpressionReachCases {
				files[fmt.Sprintf("case%d.go", i)] = "package expressionreach\n" + tc.prelude + fmt.Sprintf("\nfunc case%d(b bool){%s}\n", i, tc.body)
			}
			dir := writeTypeModule(t, files)
			cmd := exec.Command("go", "test", "./...")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("compile strict fixtures: %v\n%s", err, out)
			}
			findings, err := AnalyzeAST(dir, "./...")
			if err != nil {
				t.Fatal(err)
			}
			if countRules(findings)["type-info-unavailable"] != 0 {
				t.Fatalf("incomplete analysis: %+v", findings)
			}
			bySymbol := map[string]int{}
			for _, f := range findings {
				if f.Rule == "goroutine-in-loop" {
					bySymbol[f.Symbol]++
				}
			}
			for i, tc := range loopExpressionReachCases {
				if n := bySymbol[fmt.Sprintf("case%d", i)]; n != tc.want {
					t.Errorf("%s: got %d want %d", tc.name, n, tc.want)
				}
			}
		})
	}
}
