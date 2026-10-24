package diago

import "testing"

func TestPointerCaptureWrites(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"fresh header no copy after break", `for i:=0;i<3;i++{go func(){i++}();break}`, 0},
		{"fresh header no copy after return", `for i:=0;i<3;i++{go func(){i++}();return}`, 0},
		{"fresh header copy on continue", `for i:=0;i<3;i++{go func(){i++}();continue}`, 1},
		{"fresh pointer header mutation is not slot mutation", `for p,n:=new(item),0;n<3;p,n=new(item),n+1{go func(){p.N++}()}`, 0},
		{"shared slice mutation", `v:=[]int{0};for range xs{go func(){v[0]++}()}`, 1},
		{"shared indirect mutation", `v:=0; p:=&v; for range xs {go func(){(*p)++}()}`, 1},
		{"shared indirect assignment", `v:=0; p:=&v; for range xs {go func(){*p=1}()}`, 1},
		{"uninvoked closure does not mutate", `v:=0; for range xs {f:=func(){v++};_=f;go func(){use(v)}()}`, 0},
		{"uninvoked nested mutator does not mutate", `v:=0; mut:=func(){v++}; for range xs {f:=func(){mut()};_=f;go func(){use(v)}()}`, 0},
		{"read only", `v:=0; p:=&v; for range xs {go func(){use(*p)}()}`, 0},
		{"fresh exclusive mutation", `for range xs {v:=0; p:=&v; go func(){(*p)++}()}`, 0},
		{"address of pointer field", `var v struct{N int};p:=&v;for range xs{go use(&p.N);v.N++}`, 1},
		{"address of dereference", `v:=0;p:=&v;for range xs{go use(&*p);v++}`, 1},
		{"implicit pointer field store", `for range xs{v:=struct{N int}{};p:=&v;go func(){use(v.N)}();p.N++}`, 1},
		{"implicit pointer array store", `for range xs{v:=[1]int{};p:=&v;go func(){use(v[0])}();p[0]++}`, 1},
		{"read only pointer address", `var v struct{N int};p:=&v;for range xs{go use(&p.N)}`, 0},
		{"parent indirect mutation", `v:=0; p:=&v; for range xs {go func(){use(v)}(); (*p)++}`, 1},
		{"fresh parent indirect mutation", `for range xs {v:=0; p:=&v; go func(){use(v)}(); (*p)++}`, 1},
		{"fresh parent mutation before launch", `for range xs {v:=0; p:=&v; (*p)++; go func(){use(v)}()}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.22", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got %v, want %d", mode, got, tc.want)
				}
			}
		})
	}
}
