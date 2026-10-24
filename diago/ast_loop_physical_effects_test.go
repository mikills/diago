package diago

import (
	"go/ast"
	"testing"
)

func TestLoopPhysicalStoreOrdering(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"pointer store after RHS", `for range xs{v:=0;p:=&v;*p=func()int{go func(){use(v)}();return 1}()}`, 1},
		{"struct store after RHS", `for range xs{s:=struct{N int}{};s.N=func()int{go func(){use(s)}();return 1}()}`, 1},
		{"array store after RHS", `for range xs{a:=[1]int{};a[0]=func()int{go func(){use(a)}();return 1}()}`, 1},
		{"pointer field store after RHS", `for range xs{s:=struct{N int}{};p:=&s;p.N=func()int{go func(){use(s)}();return 1}()}`, 1},
		{"dereferenced struct root store", `for range xs{s:=struct{N int}{};p:=&s;(*p).N=func()int{go func(){use(s)}();return 1}()}`, 1},
		{"pointer array store after RHS", `for range xs{a:=[1]int{};p:=&a;p[0]=func()int{go func(){use(a)}();return 1}()}`, 1},
		{"compound pointer store", `for range xs{v:=0;p:=&v;*p+=func()int{go func(){use(v)}();return 1}()}`, 1},
		{"compound struct store", `for range xs{s:=struct{N int}{};s.N+=func()int{go func(){use(s)}();return 1}()}`, 1},
		{"compound array store", `for range xs{a:=[1]int{};a[0]+=func()int{go func(){use(a)}();return 1}()}`, 1},
		{"index call before increment store", `for range xs{a:=[1]int{};a[func()int{go func(){use(a)}();return 0}()]++}`, 1},
		{"index call before compound store", `for range xs{a:=[1]int{};a[func()int{go func(){use(a)}();return 0}()]+=1}`, 1},
		{"pointer header before RHS", `for range xs{v:=0;p:=&v;*p=func()int{go func(){p=nil}();return 1}()}`, 0},
		{"field header before RHS", `for range xs{s:=struct{N int}{};p:=&s;p.N=func()int{go func(){p=nil}();return 1}()}`, 0},
		{"array pointer header before RHS", `for range xs{a:=[1]int{};p:=&a;p[0]=func()int{go func(){p=nil}();return 1}()}`, 0},
		{"index operand before RHS", `for range xs{a:=[1]int{};i:=0;a[i]=func()int{go func(){i++}();return 1}()}`, 0},
		{"pointer header before index call", `for range xs{a:=[1]int{};p:=&a;p[func()int{go func(){p=nil}();return 0}()]++}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s := loopSyncTestScanner(t, tc.body, "1.23", mode)
					s.report()
					if got := len(*s.env.findings); got != tc.want {
						t.Fatalf("findings=%d, want %d: %+v", got, tc.want, *s.env.findings)
					}
				})
			}
		})
	}
}

func TestLoopPhysicalEffectsExecutionContexts(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"inactive capturing constructor", `v:=0;read:=func(){use(v)};worker:=func(){for{go func(){v++}();read();break}};read=func(){};worker()`, 0},
		{"actual capturing reader", `v:=0;read:=func(){};worker:=func(){for{go func(){v++}();read();break}};read=func(){use(v)};worker()`, 1},
		{"inactive pointer constructor", `v,other:=0,0;p:=&v;worker:=func(){for{go func(){use(v)}();*p=1;break}};p=&other;worker()`, 0},
		{"actual pointer target", `v,other:=0,0;p:=&other;worker:=func(){for{go func(){use(v)}();*p=1;break}};p=&v;worker()`, 1},
		{"nested inactive reader", `v:=0;read:=func(){use(v)};inner:=func(){for{go func(){v++}();read();break}};worker:=func(){inner()};read=func(){};worker()`, 0},
		{"nested actual reader", `v:=0;read:=func(){};inner:=func(){for{go func(){v++}();read();break}};worker:=func(){inner()};read=func(){use(v)};worker()`, 1},
		{"iterator first read before launch", `for range xs{v:=0;read:=func(){use(v)};iter:=func(yield func()bool){read();yield()};for range iter{};read=func(){};go func(){v++}();for range iter{}}`, 0},
		{"iterator second read after launch", `for range xs{v:=0;read:=func(){};iter:=func(yield func()bool){read();yield()};for range iter{};read=func(){use(v)};go func(){v++}();for range iter{}}`, 1},
		{"iterator first store before launch", `for range xs{v,other:=0,0;p:=&v;iter:=func(yield func()bool){*p=1;yield()};for range iter{};p=&other;go func(){use(v)}();for range iter{}}`, 0},
		{"iterator second store after launch", `for range xs{v,other:=0,0;p:=&other;iter:=func(yield func()bool){*p=1;yield()};for range iter{};p=&v;go func(){use(v)}();for range iter{}}`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s := loopSyncTestScanner(t, tc.body, "1.23", mode)
					s.report()
					if got := len(*s.env.findings); got != tc.want {
						t.Fatalf("findings=%d, want %d: %+v", got, tc.want, *s.env.findings)
					}
				})
			}
		})
	}
}

func TestLoopPhysicalEffectSinkRestored(t *testing.T) {
	s := loopSyncTestScanner(t, `for range xs{v:=0;worker:=func(){go func(){use(v)}()};worker();v++}`, "1.23", "lexical")
	c := s.beginFlowConflicts()
	e := s.beginFlowEffects()
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: e, conflicts: c}
	e.aliases = f.aliases
	called := false
	e.physical = func(loopDirectAccess) { called = true }
	f.refine(s.rootNode.(*ast.BlockStmt), nil)
	if called {
		t.Fatal("replay leaked physical effects into the enclosing sink")
	}
	e.physical(loopDirectAccess{})
	if !called {
		t.Fatal("recursive refinement failed to restore the enclosing sink")
	}
}
