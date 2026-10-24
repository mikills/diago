package diago

import "testing"

func TestPromotedPointerReceivers(t *testing.T) {
	for _, tc := range []struct {
		name, declarations, body string
		want                     int
	}{
		{"embedded pointer", `type wrapper struct{*item}`, `var v wrapper; for _,v=range []wrapper{{&item{}},{&item{}}}{go v.Use()}`, 0},
		{"parenthesized promoted method", `type wrapper struct{*item}`, `var v wrapper; for _,v=range []wrapper{{&item{}},{&item{}}}{go (v.Use)()}`, 0},
		{"nested embedded pointer", `type child struct{*item}; type wrapper struct{child}`, `var v wrapper; for _,v=range []wrapper{{child{&item{}}},{child{&item{}}}}{go v.Use()}`, 0},
		{"embedded value", `type wrapper struct{item}`, `var v wrapper; for _,v=range []wrapper{{},{}}{go v.Use()}`, 1},
		{"pointer to wrapper", `type wrapper struct{item}`, `var v *wrapper; for _,v=range []*wrapper{{},{}}{go v.Use()}`, 0},
		{"shared embedded storage", `type wrapper struct{*item}`, `var it item; v:=wrapper{&it}; for range xs{go v.Use();it.N++}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+tc.declarations+";func work(xs []int){"+tc.body+"}", "1.22", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Fatalf("%s: got %v, want %d", mode, got, tc.want)
				}
			}
		})
	}
}
