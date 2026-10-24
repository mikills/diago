package diago

import "testing"

func TestLoopAccessReachability(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"shared read exits before mutation", `v:=0;for _,n:=range xs{if n==0{go func(){use(v)}();break};v++}`, 0},
		{"shared writer exits before parent read", `v:=0;for _,n:=range xs{if n==0{go func(){v++}();break};use(v)}`, 0},
		{"shared repeated writer", `v:=0;for range xs{go func(){v++}()}`, 1},
		{"shared writer before eventual read", `v:=0;for _,n:=range xs{if n==0{go func(){v++}();break}};use(v)`, 1},
		{"fresh goto reuses storage", `for range xs{v:=0;again:v++;go func(){use(v)}();if v<3{goto again}}`, 1},
		{"fresh storage reset on ordinary iteration", `for range xs{v:=0;go func(){v++}()}`, 0},
		{"conditional copy not reachable after launch", `for i:=0;i<len(xs);i++{if xs[i]==0{go func(){i++}();break}}`, 0},
		{"conditional copy reachable after launch", `for i:=0;i<len(xs);i++{if xs[i]==0{go func(){i++}()}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.22", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got%+v want%d", mode, got, tc.want)
				}
			}
		})
	}
}
