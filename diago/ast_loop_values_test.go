package diago

import "testing"

func TestLoopRetainedValueProjections(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"pointer field positional", `go func(p *int){_=*p}(struct{P *int}{&i}.P)`, 1},
		{"pointer field keyed", `go use(struct{P *int; N int}{P:&i}.P)`, 1},
		{"function field", `go struct{F func()}{func(){use(i)}}.F()`, 1},
		{"pointer composite field", `go use((&struct{P *int}{&i}).P)`, 1},
		{"dereferenced composite field", `go use((*(&struct{P,Q *int}{&i,nil})).Q)`, 0},
		{"asserted composite field", `go use(any(struct{P,Q *int}{&i,nil}).(struct{P,Q *int}).Q)`, 0},
		{"scalar field", `go use(struct{P *int; N int}{&i,0}.N)`, 0},
		{"unrelated pointer field", `go use(struct{P,Q *int}{&i,new(int)}.Q)`, 0},
		{"unrelated function field", `go struct{F,G func()}{func(){use(i)},func(){}}.G()`, 0},
		{"omitted pointer field", `go use(struct{P,Q *int}{P:&i}.Q)`, 0},
		{"nested field", `go use(struct{A struct{P *int}; Q *int}{struct{P *int}{&i},nil}.A.P)`, 1},
		{"nested safe field", `go use(struct{A struct{N int}; P *int}{struct{N int}{0},&i}.A.N)`, 0},
		{"slice index", `go use([]*int{&i}[0])`, 1},
		{"array index", `go use([1]*int{&i}[0])`, 1},
		{"function index", `go []func(){func(){use(i)}}[0]()`, 1},
		{"slice retained", `go use([]*int{&i}[:])`, 1},
		{"function slice retained", `go use([]func(){func(){use(i)}}[:])`, 1},
		{"slice excludes prefix", `go use([]*int{&i,nil}[1:])`, 0},
		{"slice capacity retains suffix", `go use([]*int{nil,&i}[:1])`, 1},
		{"full slice excludes suffix", `go use([]*int{nil,&i}[:1:1])`, 0},
		{"keyed slice excludes prefix", `go use([]*int{0:&i,2:nil}[1:])`, 0},
		{"unrelated index", `go use([]*int{&i,new(int)}[1])`, 0},
		{"keyed array index", `go use([3]*int{2:&i,0:new(int)}[0])`, 0},
		{"zero array element", `go use([3]*int{2:&i}[1])`, 0},
		{"dynamic pointer index", `go use([]*int{&i,nil}[i%2])`, 1},
		{"dynamic scalar index", `go use([]int{read(&i)}[i%1])`, 0},
		{"map pointer value", `go use(map[int]*int{0:&i}[0])`, 1},
		{"unrelated map value", `go use(map[int]*int{0:&i,1:new(int)}[1])`, 0},
		{"scalar map value", `go use(map[*int]int{&i:0}[&i])`, 0},
		{"map key not selected", `go use(map[*int]*int{&i:new(int)}[&i])`, 0},
		{"map dynamic key not selected", `go use(map[*int]*int{&i:new(int)}[nil])`, 0},
		{"whole map retains key", `go use(map[*int]int{&i:0})`, 1},
		{"assert pointer", `go use(any(&i).(*int))`, 1},
		{"assert closure", `go any(func(){use(i)}).(func())()`, 1},
		{"assert scalar", `go use(any(struct{P *int; N int}{&i,0}.N).(int))`, 0},
		{"interface conversion", `go use(interface{}(&i).(*int))`, 1},
		{"call barrier", `go use(read(&i))`, 0},
		{"projected call barrier", `go use(struct{N int}{read(&i)}.N)`, 0},
		{"indexed call barrier", `go use([]int{read(&i)}[0])`, 0},
		{"dereferenced function call barrier", `f:=read; p:=&f; go use((*p)(&i))`, 0},
		{"shadowed conversion call barrier", `int:=read; go use(int(&i))`, 0},
		{"elided struct projection", `go use([]struct{P *int; N int}{{&i,0}}[0].P)`, 1},
		{"elided scalar projection", `go use([]struct{P *int; N int}{{&i,0}}[0].N)`, 0},
		{"aliased aggregate pointer", `a:=struct{P *int; N int}{&i,0}; b:=a; go use(b.P)`, 1},
		{"aliased aggregate scalar", `a:=struct{P *int; N int}{&i,0}; b:=a; go use(b.N)`, 0},
		{"overwritten aggregate", `a:=struct{P *int}{&i}; a=struct{P *int}{}; go use(a.P)`, 0},
		{"aliased function field", `a:=struct{F func()}{func(){use(i)}}; b:=a; go b.F()`, 1},
		{"aliased pointer", `p:=&i; q:=p; go use(q)`, 1},
		{"aliased dereference scalar", `p:=&i; q:=p; go use(*q)`, 0},
		{"aliased slice index", `a:=[]*int{&i}; b:=a; go use(b[0])`, 1},
		{"aliased slice scalar field", `a:=[]struct{P *int; N int}{{&i,0}}; b:=a; go use(b[0].N)`, 0},
		{"aliased scalar map", `a:=map[*int]int{&i:0}; b:=a; go use(b[&i])`, 0},
		{"aliased slice", `a:=[]*int{&i}; b:=a; go use(b[:])`, 1},
		{"aliased assertion", `a:=any(&i); b:=a; go use(b.(*int))`, 1},
		{"aliased scalar assertion", `a:=any(struct{P *int}{&i}); go use(a.(int))`, 0},
		{"named local type pointer", `type box struct{P *int; N int}; a:=box{&i,0}; go use(a.P)`, 1},
		{"named local type scalar", `type box struct{P *int; N int}; a:=box{&i,0}; go use(a.N)`, 0},
		{"named pointer aggregate", `type box struct{P *int; N int}; a:=&box{&i,0}; b:=a; go use(b.P)`, 1},
		{"named pointer aggregate scalar", `type box struct{P *int; N int}; a:=&box{&i,0}; b:=a; go use(b.N)`, 0},
		{"shadowed any is call", `any:=func(p *int) int{return *p}; go use(any(&i))`, 0},
		{"array slice storage", `var a [1]int; go use(a[:]); a[0]++`, 1},
		{"slice index address", `a:=[]int{i}; go use(&a[0])`, 0},
		{"pointer array index address", `a:=&[1]int{i}; go use(&a[0])`, 0},
		{"pointer field address", `a:=&struct{N int}{i}; go use(&a.N)`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					source := loopTestPrelude + `func work(){var i int; for i=0;i<100;i++ {` + tc.body + `}}`
					got := analyzeLoopRegression(t, source, "1.22", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopRetainedValueDeclaredAliases(t *testing.T) {
	for _, tc := range []struct {
		expression string
		want       int
	}{
		{"a.P", 1}, {"a.N", 0}, {"p.P", 1}, {"p.N", 0},
	} {
		for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
			source := loopTestPrelude + `func work(){
				type box struct{P *int; N int}
				var i int
				var a box = box{&i,0}
				var p *box = &a
				_ = p
				for i=0;i<100;i++ {go use(` + tc.expression + `)}
			}`
			got := analyzeLoopRegression(t, source, "1.22", mode)
			if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
				t.Errorf("%s/%s: got %d, want %d: %+v", tc.expression, mode, n, tc.want, got)
			}
		}
	}
}

func TestLoopRetainedValueTypedConversions(t *testing.T) {
	for _, body := range []string{
		`type ptr *int; go use(ptr(&i))`,
		`type callback func(); go callback(func(){use(i)})()`,
	} {
		got := analyzeLoopRegression(t, loopTestPrelude+`func work(){var i int; for i=0;i<100;i++ {`+body+`}}`, "1.22", "typed")
		if n := countRules(got)["goroutine-in-loop"]; n != 1 {
			t.Errorf("%s: got %d warnings, want 1: %+v", body, n, got)
		}
	}
}
