package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"testing"

	"golang.org/x/tools/go/cfg"
)

func TestLoopPointerCellLiveAddressConstraints(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			s, body := loopLiveTestScanner(t, `a,b:=0,0;p,q:=&a,&a;pp:=&p;ppp:=&pp;**ppp=&b;r:=*pp;*(&pp)=&q;use(r)`, typed)
			vars := map[string]*loopVariable{}
			for id, v := range s.index.ids {
				vars[id.Name] = v
			}
			graph := cfg.New(body, func(*ast.CallExpr) bool { return true })
			got := s.loopFlowLiveAddresses(graph, nil)
			for _, pair := range [][2]string{{"p", "a"}, {"p", "b"}, {"r", "a"}, {"r", "b"}, {"pp", "p"}, {"pp", "q"}} {
				if !got[vars[pair[0]]][vars[pair[1]]] {
					t.Errorf("missing may-address %s -> %s", pair[0], pair[1])
				}
			}
		})
	}
}

func TestLoopPointerCellLiveLaunchEquivalence(t *testing.T) {
	for _, body := range []string{
		`var p *int;pp:=&p;ppp:=&pp;f:=func(){};if i==0{f=func(){use(p)}}else{**ppp=&i};go f()`,
		`var p *int;pp:=&p;ppp:=&pp;f:=func(){};if i==0{f=func(){use(p)}}else{r:=*ppp;*r=&i};go f()`,
		`var p *int;pp:=&p;ppp:=&pp;f:=func(){};if i==0{f=func(){use(p)}}else{**ppp=&i};if i==1{use(i)};go f()`,
		`var p *int;pp:=&p;ppp:=&pp;f:=func(){};if i==0{**ppp=&i}else{f=func(){use(**ppp)}};go f()`,
		`f:=func(){};p:=&f;pp:=&p;var q *int;g:=func(){};if i==0{**pp=func(){use(q)}}else{q=&i;g=func(){f()}};go g()`,
	} {
		for _, typed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/typed=%v", body, typed), func(t *testing.T) {
				s, block := loopLiveTestScanner(t, "var i int;for i=0;i<3;i++{"+body+"}", typed)
				want, _, _ := loopLiveTestRun(s, block, false)
				got, _, _ := loopLiveTestRun(s, block, true)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("pruning changed launch summaries: got %v want %v", got, want)
				}
			})
		}
	}
}

func TestLoopPointerCellStoreMultipleTargetsWeak(t *testing.T) {
	z := newDeferredFixture(t, `func work(){a,b,c:=0,0,0;p,q:=&a,&a;pp:=&p;*pp=&b}`)
	for _, node := range z.body.List[:3] {
		z.step(node)
	}
	p, q, a, b, c, pp := z.vars["p"], z.vars["q"], z.vars["a"], z.vars["b"], z.vars["c"], z.vars["pp"]
	// Different values in the alias and retained lattices catch accidental use
	// of a payload/callback capture map as an address proof.
	z.w.values[q] = loopValue{c: loopRead}
	z.w.values[z.f.aliasSlot(q)] = loopValue{c: loopRead}
	z.w.values[z.f.aliasSlot(pp)] = loopValue{p: loopRead, q: loopRead}
	before := deepDeferredBindings(z.w.values)
	z.step(z.body.List[3])
	for target, old := range map[*loopVariable]*loopVariable{p: a, q: c} {
		for _, slot := range []*loopVariable{target, z.f.aliasSlot(target)} {
			if len(z.w.values[slot]) != 2 || z.w.values[slot][old] != loopRead || z.w.values[slot][b] != loopRead {
				t.Fatalf("weak store lost alternatives: %v", z.w.values[slot])
			}
		}
	}
	if before[z.f.aliasSlot(p)][a] != loopRead || before[z.f.aliasSlot(q)][c] != loopRead {
		t.Fatal("weak store mutated a shared snapshot")
	}
}
