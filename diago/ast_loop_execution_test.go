package diago

import "testing"

func TestLoopExecutionEvents(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"address only does not read", `var v int;for range xs{go func(){_= &v}();v++}`, 0},
		{"sync address only does not read", `v:=0;for range xs{go func(){v++}();func(){_= &v}()}`, 1},
		{"fresh sync address only does not read", `for range xs{v:=0;go func(){v++}();func(){_= &v}()}`, 0},
		{"address indexes still read", `v:=[3]int{};var i int;for i=0;i<3;i++{go func(){_= &v[i]}()}`, 1},
		{"outside writer before loop reader", `v:=0;go func(){v++}();for range xs{go func(){use(v)}()}`, 1},
		{"outside reader after loop writer", `v:=0;for range xs{go func(){v++}()};go func(){use(v)}()`, 1},
		{"outside launches are not diagnostics", `v:=0;go func(){v++}();go func(){use(v)}()`, 0},
		{"immutable sync worker declared outside loop", `v:=0;f:=func(){go func(){v++}()};for range xs{f()}`, 1},
		{"named worker delegates outside lexical loop", `var i int;f:=func(){go func(){use(i)}()};for i=0;i<3;i++{go f()}`, 1},
		{"function range invokes synchronous writer", `for range xs{v:=0;go func(){use(v)}();for range func(yield func()bool){v++;yield()} {}}`, 1},
		{"iterator effects escape async wrapper", `v:=0;for range xs{go func(){for range func(yield func()bool){v++;yield()} {}}()}`, 1},
		{"iterator effects escape sync wrapper", `for range xs{v:=0;go func(){use(v)}();func(){for range func(yield func()bool){v++;yield()} {}}()}`, 1},
		{"named iterator effects escape async wrapper", `v:=0;it:=func(yield func()bool){v++;yield()};for range xs{go func(){for range it {}}()}`, 1},
		{"callback copied through known aggregate pointer", `type job struct{F func()};v:=0;obj:=job{F:func(){v++}};p:=&obj;consume:=func(j job){j.F()};for range xs{go consume(*p)}`, 1},
		{"address dereference cancellation", `for range xs{v:=0;p:=&v;q:=&*p;go func(){use(v)}();*q=1}`, 1},
		{"direct canceled address store", `for range xs{v:=0;p:=&v;go func(){use(v)}();*(&*p)=1}`, 1},
		{"mixed reference retains current epoch", `var previous *int;for n,v:=range xs{if previous==nil{previous=&v;continue};p:=previous;if n==1{p=&v};go func(){use(v)}();*p=1;break}`, 1},
		{"prior-only reference remains separate", `var previous *int;for _,v:=range xs{if previous==nil{previous=&v;continue};p:=previous;go func(){use(v)}();*p=1;break}`, 0},
		{"async worker uses current reader", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;read()};read=func(){};for range xs{go worker();v++}`, 0},
		{"nested iterator repeated invocation", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}()}`, 1},
		{"double dereference writes known local", `for range xs{v:=0;p:=&v;pp:=&p;go func(){use(v)}();**pp=1}`, 1},
		{"double dereference reads known local", `for range xs{v:=0;p:=&v;pp:=&p;go func(){v=1}();use(**pp)}`, 1},
		{"double dereference uses current pointer", `for range xs{v,u:=0,0;p:=&v;pp:=&p;p=&u;go func(){use(v)}();**pp=1}`, 0},
		{"loaded pointer snapshot preserves target", `for range xs{v,u:=0,0;p:=&v;pp:=&p;r:=*pp;p=&u;go func(){use(v)}();*r=1}`, 1},
		{"function range has no implicit writes", `for range xs{v:=0;go func(){use(v)}();for range func(yield func()bool){yield()} {}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.23", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got%+v want%d", mode, got, tc.want)
				}
			}
		})
	}
}
