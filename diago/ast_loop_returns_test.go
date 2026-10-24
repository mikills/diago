package diago

import "testing"

func TestNamedReturnAccesses(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"explicit assignment", `func work(xs []int)(v int){for range xs{go func(){use(v)}()};return 1}`, 1},
		{"bare return read", `func work(xs []int)(v int){for range xs{go func(){v=1}();break};return}`, 1},
		{"bare return reader", `func work(xs []int)(v int){for range xs{go func(){use(v)}()};return}`, 0},
		{"grouped results", `func work(xs []int)(a,b int){for range xs{go func(){use(b)}();break};return 0,1}`, 1},
		{"shadowed local", `func work(xs []int)(v int){for range xs{v:=0;go func(){use(v)}();return 1};return}`, 0},
		{"unnamed results", `func work(xs []int)(int,int){for range xs{v:=0;go func(){use(v)}()};return 0,1}`, 0},
		{"literal named result", `func work(xs []int){for range xs{func()(v int){go func(){use(v)}();return 1}()}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+tc.source, "1.23", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got%+v want%d", mode, got, tc.want)
				}
			}
		})
	}
}
