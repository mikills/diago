package diago

import "testing"

func TestLoopFlowRetainedRangesAndStores(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"range function literal", `for _,f:=range []func(){func(){use(i)}} {go f()}`, 1},
		{"range named function", `f:=func(){use(i)}; for _,g:=range []func(){f} {go g()}`, 1},
		{"range named slice", `fs:=[]func(){func(){use(i)}}; for _,f:=range fs {go f()}`, 1},
		{"range array", `for _,f:=range [1]func(){func(){use(i)}} {go f()}`, 1},
		{"range pointer array literal", `for _,f:=range &[1]func(){func(){use(i)}} {go f()}`, 1},
		{"range pointer array alias", `a:=[1]func(){func(){use(i)}}; p:=&a; for _,f:=range p {go f()}`, 1},
		{"range pointer array snapshot", `a:=[1]func(){func(){use(i)}}; for _,f:=range &a {a=[1]func(){}; go f()}`, 1},
		{"range pointer value", `for _,p:=range []*int{&i} {go use(p)}`, 1},
		{"range aggregate value", `for _,s:=range []struct{F func()}{{func(){use(i)}}} {go s.F()}`, 1},
		{"range scalar aggregate field", `for _,s:=range []struct{P *int; N int}{{&i,i}} {go use(s.N)}`, 0},
		{"range map function", `for _,f:=range map[int]func(){0:func(){use(i)}} {go f()}`, 1},
		{"range named map function", `m:=map[int]func(){0:func(){use(i)}}; for _,f:=range m {go f()}`, 1},
		{"range map pointer key", `for p:=range map[*int]int{&i:0} {go use(p)}`, 1},
		{"range map scalar key", `for k:=range map[int]*int{0:&i} {go use(k)}`, 0},
		{"range map scalar value", `for _,v:=range map[*int]int{&i:i} {go use(v)}`, 0},
		{"range named map scalar key", `m:=map[int]*int{0:&i}; for k:=range m {go use(k)}`, 0},
		{"range named map scalar value", `m:=map[*int]int{&i:i}; for _,v:=range m {go use(v)}`, 0},
		{"range literal map key is not value", `for _,f:=range map[*int]func(){&i:func(){}} {go f()}`, 0},
		{"range literal map value is not key", `var j int; for p:=range map[*int]func(){&j:func(){use(i)}} {go use(p)}`, 0},
		{"range channel function", `ch:=make(chan func(),1); ch<-func(){use(i)}; close(ch); for f:=range ch {go f()}`, 1},
		{"range declared channel function", `var ch chan func(); ch=make(chan func(),1); ch<-func(){use(i)}; close(ch); for f:=range ch {go f()}`, 1},
		{"range channel pointer", `ch:=make(chan *int,1); ch<- &i; close(ch); for p:=range ch {go use(p)}`, 1},
		{"range channel aggregate", `ch:=make(chan struct{F func()},1); ch<-struct{F func()}{func(){use(i)}}; close(ch); for s:=range ch {go s.F()}`, 1},
		{"range scalar channel", `ch:=make(chan int,1); ch<-i; close(ch); for v:=range ch {go use(v)}`, 0},
		{"range channel overwrite", `ch:=make(chan func(),1); ch<-func(){use(i)}; ch=make(chan func()); for f:=range ch {go f()}`, 0},
		{"range index is not element", `for k:=range []*int{&i} {go use(k)}`, 0},
		{"range scalar elements", `for _,v:=range []int{i} {go use(v)}`, 0},
		{"range source evaluated once capturing", `fs:=[]func(){func(){use(i)}}; for _,f:=range fs {fs=nil; go f()}`, 1},
		{"range source evaluated once safe", `fs:=[]func(){func(){}}; for _,f:=range fs {fs=[]func(){func(){use(i)}}; go f()}`, 0},
		{"range target body strong overwrite", `for _,f:=range []func(){func(){use(i)}} {f=func(){}; go f()}`, 0},
		{"range source strong overwrite", `fs:=[]func(){func(){use(i)}}; fs=[]func(){func(){}}; for _,f:=range fs {go f()}`, 0},
		{"range actual overwrite", `f:=func(){use(i)}; for _,f=range []func(){func(){}} {go f()}`, 0},
		{"range nil overwrite", `f:=func(){use(i)}; for _,f=range []func(){nil} {go f()}`, 0},
		{"range lexical shadow", `fs:=[]func(){func(){use(i)}}; {fs:=[]func(){func(){}}; for _,f:=range fs {go f()}}; _=fs`, 0},
		{"range nested scalar aggregate", `for _,ss:=range [][]struct{P *int; N int}{{{&i,i}}} {for _,s:=range ss {go use(s.N)}}`, 0},
		{"range nested functions", `for _,fs:=range [][]func(){{func(){use(i)}}} {for _,f:=range fs {go f()}}`, 1},
		{"range named types", `type F func(); type FS []F; fs:=FS{func(){use(i)}}; for _,f:=range fs {go f()}`, 1},
		{"range stored element", `fs:=make([]func(),1); fs[0]=func(){use(i)}; for _,f:=range fs {go f()}`, 1},
		{"direct capturing field", `var s struct{F func()}; s.F=func(){use(i)}; go s.F()`, 1},
		{"branch field store", `var s struct{F func()}; if i==0 {s.F=func(){use(i)}}; go s.F()`, 1},
		{"branch field root overwrite", `var s struct{F func()}; if i==0 {s.F=func(){use(i)}; s=struct{F func()}{}}; go s.F()`, 0},
		{"named function field", `var s struct{F func()}; f:=func(){use(i)}; s.F=f; go s.F()`, 1},
		{"overwritten named function field", `var s struct{F func()}; f:=func(){use(i)}; f=func(){}; s.F=f; go s.F()`, 0},
		{"named pointer field", `var s struct{P *int}; p:=&i; s.P=p; go use(s.P)`, 1},
		{"indexed worker", `fs:=make([]func(),1); fs[0]=func(){use(i)}; go fs[0]()`, 1},
		{"array worker", `var fs [1]func(); fs[0]=func(){use(i)}; go fs[0]()`, 1},
		{"map worker", `fs:=make(map[int]func()); fs[0]=func(){use(i)}; go fs[0]()`, 1},
		{"map retains stored key", `m:=make(map[*int]int); m[&i]=0; go use(m)`, 1},
		{"map stored scalar value is safe", `m:=make(map[*int]int); m[&i]=0; go use(m[&i])`, 0},
		{"map stored key snapshot", `var j int; p:=&i; m:=make(map[*int]int); p,m[p]=&j,0; go use(m)`, 1},
		{"nested fields", `var s struct{Inner struct{F func()}}; s.Inner.F=func(){use(i)}; go s.Inner.F()`, 1},
		{"nested field index", `var s struct{Workers [1]struct{F func()}}; s.Workers[0].F=func(){use(i)}; go s.Workers[0].F()`, 1},
		{"alias store", `var s struct{F func()}; p:=&s; p.F=func(){use(i)}; go s.F()`, 1},
		{"alias chain store", `var s struct{F func()}; p:=&s; q:=p; q.F=func(){use(i)}; go s.F()`, 1},
		{"alias nested field store", `var s struct{Inner struct{F func()}}; p:=&s; p.Inner.F=func(){use(i)}; go s.Inner.F()`, 1},
		{"alias explicit dereference", `var s struct{F func()}; p:=&s; (*p).F=func(){use(i)}; go s.F()`, 1},
		{"alias whole pointee store", `var s struct{F func()}; p:=&s; *p=struct{F func()}{func(){use(i)}}; go s.F()`, 1},
		{"alias array store", `var fs [1]func(); p:=&fs; p[0]=func(){use(i)}; go fs[0]()`, 1},
		{"alias branch joins", `var a,b struct{F func()}; p:=&a; if i==0 {p=&b}; p.F=func(){use(i)}; go a.F(); go b.F()`, 2},
		{"alias pointer reassigned", `var a,b struct{F func()}; p:=&a; p=&b; p.F=func(){use(i)}; go a.F()`, 0},
		{"alias pointer safe overwrite", `var s struct{F func()}; p:=&s; p.F=func(){use(i)}; s=struct{F func()}{func(){}}; go p.F()`, 0},
		{"alias address evaluated before writes", `var a,b struct{F func()}; p:=&a; p,p.F=&b,func(){use(i)}; go a.F()`, 1},
		{"alias copied aggregate snapshot", `var s struct{F func()}; other:=s; p:=&s; p.F=func(){use(i)}; go other.F()`, 0},
		{"whole aggregate overwrite", `var s struct{F func()}; s.F=func(){use(i)}; s=struct{F func()}{func(){}}; go s.F()`, 0},
		{"whole slice overwrite", `fs:=make([]func(),1); fs[0]=func(){use(i)}; fs=[]func(){func(){}}; go fs[0]()`, 0},
		{"weak field overwrite is coarse", `var s struct{F func()}; s.F=func(){use(i)}; s.F=func(){}; go s.F()`, 1},
		{"safe scalar field store", `var s struct{N int}; s.N=i; go use(s.N)`, 0},
		{"safe scalar indexed store", `ns:=make([]int,1); ns[0]=i; go use(ns[0])`, 0},
		{"safe scalar map store", `ns:=make(map[int]int); ns[0]=i; go use(ns[0])`, 0},
		{"safe scalar field selection", `var s struct{N int; F func()}; s.F=func(){use(i)}; s.N=i; go use(s.N)`, 0},
		{"safe scalar alias store", `var s struct{N int; F func()}; p:=&s; p.N=i; go p.F()`, 0},
		{"stored function observes current captures", `var s struct{F func()}; f:=func(){use(i)}; s.F=func(){f()}; f=func(){}; go s.F()`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					source := loopTestPrelude + `func work(){var i int; for i=0;i<3;i++ {` + tc.body + `}}`
					findings := analyzeLoopRegression(t, source, "go1.22", mode)
					if got := countRules(findings)["goroutine-in-loop"]; got != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", got, tc.want, findings)
					}
				})
			}
		})
	}
}
