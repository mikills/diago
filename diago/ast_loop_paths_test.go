package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestLoopSiblingBranchRegressions(t *testing.T) {
	tests := []struct {
		name, body, version string
		want                int
	}{
		{"fresh if alternatives", `for _,v:=range xs {if v==0 {go func(){v++}()} else {go func(){use(v)}()}}`, "1.22", 0},
		{"sequential siblings", `for _,v:=range xs {go func(){v++}(); go func(){use(v)}()}`, "1.22", 2},
		{"same arm siblings", `for _,v:=range xs {if v==0 {go func(){v++}(); go func(){use(v)}()} else {go func(){use(v)}()}}`, "1.22", 2},
		{"all read only", `for _,v:=range xs {go func(){use(v)}(); if v==0 {go func(){use(v)}()} else {go func(){use(v)}()}}`, "1.22", 0},
		{"writer alternatives", `for _,v:=range xs {if v==0 {go func(){v++}()} else {go func(){v--}()}}`, "1.22", 0},
		{"else if alternatives", `for _,v:=range xs {if v==0 {go func(){v++}()} else if len(xs)==1 {go func(){use(v)}()} else {go func(){use(v)}()}}`, "1.22", 0},
		{"nested disjoint opposing subtrees", `for _,v:=range xs {if v>0 {if v==1 {go func(){v++}()} else {go func(){use(v)}()}} else {go func(){use(v)}()}}`, "1.22", 0},
		{"sequential independent decisions", `for _,v:=range xs {if v==0 {go func(){v++}()}; if v!=0 {go func(){use(v)}()}}`, "1.22", 2},
		{"sequential nested decisions", `for _,v:=range xs {if v>0 {if v==1 {go func(){v++}()}; if v==2 {go func(){use(v)}()}}}`, "1.22", 2},
		{"only feasible sites reported", `for _,v:=range xs {if v==0 {go func(){v++}()} else {go func(){use(v)}()}; go func(){use(v)}()}`, "1.22", 2},
		{"unconditional writer", `for _,v:=range xs {go func(){v++}(); if v==0 {go func(){use(v)}()} else {go func(){use(v)}()}}`, "1.22", 3},
		{"switch alternatives", `for _,v:=range xs {switch v {case 0: go func(){v++}(); case 1: go func(){use(v)}(); default: go func(){use(v)}()}}`, "1.22", 0},
		{"type switch alternatives", `for _,v:=range xs {switch any(v).(type) {case int: go func(){v++}(); default: go func(){use(v)}()}}`, "1.22", 0},
		{"select alternatives", `ch:=make(chan int); for _,v:=range xs {select {case <-ch: go func(){v++}(); default: go func(){use(v)}()}}`, "1.22", 0},
		{"switch fallthrough", `for _,v:=range xs {switch v {case 0: go func(){v++}(); fallthrough; default: go func(){use(v)}()}}`, "1.22", 2},
		{"switch fallthrough trailing empty statement", `for _,v:=range xs {switch v {case 0: go func(){v++}(); fallthrough; ; default: go func(){use(v)}()}}`, "1.22", 2},
		{"switch fallthrough chain", `for _,v:=range xs {switch v {case 0: go func(){v++}(); fallthrough; case 1: fallthrough; default: go func(){use(v)}()}}`, "1.22", 2},
		{"fallthrough leaves other alternatives exclusive", `for _,v:=range xs {switch v {case 0: go func(){v++}(); fallthrough; case 1: go func(){use(v)}(); default: go func(){use(v)}()}}`, "1.22", 2},
		{"nested fallthrough does not join outer cases", `for _,v:=range xs {switch v {case 0: switch {case true: go func(){v++}(); fallthrough; default: use()}; default: go func(){use(v)}()}}`, "1.22", 0},
		{"nested owner alternatives", `for _,v:=range xs {go func(){if v==0 {go func(){v++}()} else {go func(){use(v)}()}}()}`, "1.22", 0},
		{"nested owner siblings", `for _,v:=range xs {go func(){go func(){v++}(); go func(){use(v)}()}()}`, "1.22", 2},
		{"ancestor delegation remains separate", `for _,v:=range xs {go func(){go func(){v++}()}()}`, "1.22", 0},
		{"different variables", `for _,v:=range xs {w:=v; go func(){v++}(); go func(){use(w)}()}`, "1.22", 0},
		{"legacy alternatives still share across iterations", `for _,v:=range xs {if v==0 {go func(){v++}()} else {go func(){use(v)}()}}`, "1.21", 2},
		{"reused alternatives still share across iterations", `var v int; for _,v=range xs {if v==0 {go func(){v++}()} else {go func(){use(v)}()}}`, "1.22", 2},
		{"inner repeated writers still share fresh outer variable", `for _,v:=range xs {for j:=0;j<2;j++ {if j==0 {go func(){v++}()} else {go func(){v--}()}}}`, "1.22", 2},
		{"parent mutation retained", `for _,v:=range xs {if v==0 {go func(){use(v)}()} else {go func(){use(v)}()}; v++}`, "1.22", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", tc.version, mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

// Exercise the helper independently of report's integration and other hazard
// sources. All synthetic launches share one variable and function owner.
func TestLoopSiblingHazards(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       string // launch-order hazard bits
	}{
		{"if", `if v==0 {go func(){v++}()} else {go func(){use(v)}()}`, "00"},
		{"sequential", `go func(){v++}(); go func(){use(v)}()`, "11"},
		{"same arm", `if v==0 {go func(){v++}(); go func(){use(v)}()} else {go func(){use(v)}()}`, "110"},
		{"nested", `if v>0 {if v==1 {go func(){v++}()} else {go func(){use(v)}()}} else {go func(){use(v)}()}`, "000"},
		{"sequential ifs", `if v==0 {go func(){v++}()}; if v!=0 {go func(){use(v)}()}`, "11"},
		{"asymmetric", `if v==0 {go func(){v++}()} else {go func(){use(v)}()}; go func(){use(v)}()`, "101"},
		{"switch", `switch v {case 0: go func(){v++}(); case 1: go func(){use(v)}(); default: go func(){use(v)}()}`, "000"},
		{"fallthrough", `switch v {case 0: go func(){v++}(); fallthrough; case 1: go func(){use(v)}(); default: go func(){use(v)}()}`, "110"},
		{"fallthrough trailing empty statement", `switch v {case 0: go func(){v++}(); fallthrough; ; default: go func(){use(v)}()}`, "11"},
		{"fallthrough chain", `switch v {case 0: go func(){v++}(); fallthrough; case 1: fallthrough; default: go func(){use(v)}()}`, "11"},
		{"type switch", `switch any(v).(type) {case int: go func(){v++}(); default: go func(){use(v)}()}`, "00"},
		{"select", `select {case <-ch: go func(){v++}(); default: go func(){use(v)}()}`, "00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, launches := loopSiblingTestLaunches(t, tc.body)
			hazards := loopSiblingHazards(root, launches)
			var bits strings.Builder
			for _, launch := range launches {
				if hazards[launch.stmt] {
					bits.WriteByte('1')
				} else {
					bits.WriteByte('0')
				}
			}
			if got := bits.String(); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLoopSiblingHazardsIdentity(t *testing.T) {
	root, launches := loopSiblingTestLaunches(t, `go func(){v++}(); go func(){use(v)}()`)
	launches[1].owner = &ast.FuncLit{}
	if got := loopSiblingHazards(root, launches); len(got) != 0 {
		t.Fatalf("different owners must not be siblings: %v", got)
	}
	launches[1].owner = nil
	launches[1].values = loopValue{new(loopVariable): loopRead}
	if got := loopSiblingHazards(root, launches); len(got) != 0 {
		t.Fatalf("different variables must not conflict: %v", got)
	}
}

func loopSiblingTestLaunches(t testing.TB, body string) (ast.Node, []loopLaunch) {
	t.Helper()
	root, err := parser.ParseFile(token.NewFileSet(), "paths.go", "package sample; func work(){"+body+"}", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	v := new(loopVariable)
	var launches []loopLaunch
	ast.Inspect(root, func(node ast.Node) bool {
		launch, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		mode := loopRead
		ast.Inspect(launch.Call, func(child ast.Node) bool {
			if _, ok := child.(*ast.IncDecStmt); ok {
				mode |= loopWrite
			}
			return true
		})
		launches = append(launches, loopLaunch{stmt: launch, values: loopValue{v: mode}})
		return false
	})
	return root, launches
}

func BenchmarkLoopSiblingHazards(b *testing.B) {
	for _, alternatives := range []bool{false, true} {
		for _, n := range []int{100, 1000, 10000} {
			b.Run(fmt.Sprintf("alternatives=%v/%d", alternatives, n), func(b *testing.B) {
				body := strings.Repeat(`if v==0 {go func(){v++}()}; if v!=0 {go func(){use(v)}()};`, n)
				if alternatives {
					body = "switch v {"
					var source strings.Builder
					source.WriteString(body)
					for i := 0; i < n; i++ {
						fmt.Fprintf(&source, "case %d: go func(){v++}();", i)
					}
					source.WriteString("}")
					body = source.String()
				}
				root, launches := loopSiblingTestLaunches(b, body)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					loopSiblingHazards(root, launches)
				}
			})
		}
	}
}
