package diago

import "testing"

func TestRootParameterIdentity(t *testing.T) {
	for _, source := range []string{
		`package p
func work(p int){f:=func(){println(p)};p,x:=0,0;for j:=0;j<3;j++{p++;go f()};_=x}`,
		`package p
func work()(p int){f:=func(){println(p)};p,x:=0,0;for j:=0;j<3;j++{p++;go f()};_=x;return}`,
		`package p
type item struct{}
func(p *item)work(){f:=func(){println(p)};p,x:=new(item),0;for j:=0;j<3;j++{p=new(item);go f()};_=x}`,
	} {
		for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
			got := analyzeLoopRegression(t, source, "1.22", mode)
			if countRules(got)["goroutine-in-loop"] != 1 {
				t.Errorf("%s: got %+v for %s", mode, got, source)
			}
		}
	}
}
