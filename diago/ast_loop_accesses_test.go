package diago

import "testing"

func TestDirectAccessBranches(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"opposing parent write", `for _,v:=range xs {if v==0 {go func(){use(v)}()} else {v++}}`, 0},
		{"opposing parent read", `for _,v:=range xs {if v==0 {go func(){v++}()} else {use(v)}}`, 0},
		{"later common read", `for _,v:=range xs {if v==0 {go func(){v++}()};use(v)}`, 1},
		{"later common write", `for _,v:=range xs {if v==0 {go func(){use(v)}()};v++}`, 1},
		{"switch conditions precede body", `for _,v:=range xs {switch {case v==0:go func(){v++}();case v==1:}}`, 0},
		{"select values precede body", `ready:=make(chan struct{});out:=make(chan int);for _,v:=range xs {select{case <-ready:go func(){v++}();case out<-v:}}`, 0},
		{"select assignment only selected arm", `ready:=make(chan struct{});out:=make(chan int);for _,v:=range xs {select{case <-ready:go func(){use(v)}();case v=<-out:}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.22", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got %+v, want %d", mode, got, tc.want)
				}
			}
		})
	}
}
