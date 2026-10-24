package diago

import (
	"fmt"
	"go/ast"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This fixture isolates projection cardinality from binding simulation. Every
// event keeps its own physical point, even when the invocation context is shared.
func loopDeferredProjectionFixture(n int, conditional bool) (*loopFlowConflicts, *loopConflictFrame) {
	v := &loopVariable{}
	lit := &ast.FuncLit{Type: &ast.FuncType{}, Body: &ast.BlockStmt{Lbrace: 1, Rbrace: 2}}
	child := &loopConflictFrame{owner: lit, actual: true, points: make([]loopConflictPoint, 1)}
	child.launches = []loopSyncLaunch{{stmt: &ast.GoStmt{}, point: 0, values: loopValue{v: loopWrite}}}
	g := &loopConflictFrame{root: true, registrations: map[*ast.DeferStmt][]int{}, delayed: map[*ast.DeferStmt][]loopDeferredPhysical{}, effects: map[int][]loopDirectAccess{}}
	g.point()
	var stmts []*ast.DeferStmt
	last := 0
	for i := 0; i < n; i++ {
		stmt := &ast.DeferStmt{Call: &ast.CallExpr{Fun: lit}}
		stmts = append(stmts, stmt)
		reg, join := g.point(), g.point()
		g.edge(last, reg)
		g.edge(reg, join)
		if conditional {
			g.edge(last, join)
		}
		g.registrations[stmt] = []int{reg}
		g.calls = append(g.calls, loopContextCall{call: stmt.Call, target: child, guard: stmt})
		last = join
	}
	for i := n - 1; i >= 0; i-- {
		p := g.point()
		g.edge(last, p)
		g.delayed[stmts[i]] = []loopDeferredPhysical{{point: p, target: child, completed: true, effects: []loopDirectAccess{{variable: v, mode: loopRead, effect: true}}}}
		last = p
	}
	s := &loopScanner{index: &loopIndex{locations: map[ast.Node]loopLocation{}}}
	return &loopFlowConflicts{s: s, frames: []*loopConflictFrame{g, child}, refined: true}, g
}

func TestLoopDeferredProjectionLinearCardinality(t *testing.T) {
	for _, n := range []int{80, 120, 240, 480} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c, source := loopDeferredProjectionFixture(n, false)
			for pass := 0; pass < 2; pass++ {
				e := c.projectContextEscapes()
				if len(e.frames) != 2 || len(e.guarded) != 0 {
					t.Fatalf("straight-line stack retains products: frames=%d generators=%d", len(e.frames), len(e.guarded))
				}
				if len(e.frames[0].launches) != n || len(e.frames[0].graph.effects) != n {
					t.Fatal("projection dropped physical launches/effects")
				}
				if &e.frames[0].graph.points[0] != &source.points[0] {
					t.Fatal("unconditional projection copied immutable edges")
				}
				if len(source.effects) != 0 || len(source.launches) != 0 {
					t.Fatal("projection mutated the source")
				}
			}
		})
	}
}

func TestLoopDeferredProjectionStreamsConditionalPairs(t *testing.T) {
	const n = 20
	c, source := loopDeferredProjectionFixture(n, true)
	e := c.projectContextEscapes()
	if len(e.frames) != 2 || len(e.guarded) != 1 {
		t.Fatalf("guard products were retained eagerly: frames=%d generators=%d", len(e.frames), len(e.guarded))
	}
	graphs := map[*loopConflictFrame]bool{}
	count := 0
	e.guarded[0](func(frame loopSyncEscapeFrame) {
		count++
		graphs[frame.graph] = true
		if len(frame.graph.points) > 4*len(source.points) {
			t.Fatal("product exceeds two independent guards")
		}
		if len(frame.launches) == 0 || len(frame.graph.effects) == 0 {
			t.Fatal("guarded projection lost delayed payloads")
		}
	})
	if count != n+n*(n-1)/2 || len(graphs) != 1 {
		t.Fatalf("solved %d products using %d graph objects", count, len(graphs))
	}
	if len(source.effects) != 0 {
		t.Fatal("scratch installation contaminated source effects")
	}
}

func TestLoopDeferredProjectionReusesAllocations(t *testing.T) {
	c, source := loopDeferredProjectionFixture(80, true)
	var guards []loopDeferredGuard
	for stmt := range source.delayed {
		guards = append(guards, loopDeferredGuard{registrations: source.registrations[stmt]})
	}
	frame := loopSyncEscapeFrame{graph: c.frames[0]}
	var w loopDeferredGuardWorkspace
	// Warm every product, including metadata slices whose reachable states
	// differ by registration. Check allocations, never wall-clock timing.
	for i := range guards {
		w.project(frame, guards[i], &guards[(i+1)%len(guards)])
	}
	allocs := testing.AllocsPerRun(5, func() {
		for i := range guards {
			w.project(frame, guards[i], &guards[(i+1)%len(guards)])
		}
	})
	if allocs > 2 {
		t.Fatalf("warm guard products allocate per graph/point: %.0f allocations", allocs)
	}
}

func TestLoopDeferredProjectionGuardSemantics(t *testing.T) {
	for _, tc := range []struct {
		name         string
		edges        [][2]int
		regs, resets []int
		want         bool
	}{
		{"dominates", [][2]int{{0, 1}, {1, 2}, {2, 3}}, []int{1}, nil, true},
		{"exclusive registration", [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}}, []int{1}, nil, false},
		{"reset kills registration", [][2]int{{0, 1}, {1, 2}, {2, 3}}, []int{1}, []int{2}, false},
		{"registration after reset", [][2]int{{0, 1}, {1, 2}, {2, 3}}, []int{2}, []int{1}, true},
		{"first loop entry matters", [][2]int{{0, 2}, {2, 3}, {3, 1}, {1, 2}}, []int{1}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &loopConflictFrame{points: make([]loopConflictPoint, 4)}
			for _, edge := range tc.edges {
				g.edge(edge[0], edge[1])
			}
			var w loopDeferredGuardWorkspace
			if got := w.alwaysActive(g, tc.regs, tc.resets, []loopDeferredPhysical{{point: 3}}); got != tc.want {
				t.Fatalf("always active = %v, want %v", got, tc.want)
			}
			old, mapping := loopDeferredGuardGraph(loopSyncEscapeFrame{graph: g}, tc.regs, tc.resets)
			got := w.project(loopSyncEscapeFrame{graph: g}, loopDeferredGuard{tc.regs, tc.resets}, nil)
			if !reflect.DeepEqual(old.graph.points, got.graph.points) {
				t.Fatal("reusable single-guard product differs from reference")
			}
			for p := range mapping {
				if mapping[p][0] != w.mapping[p][0] || mapping[p][1] != w.mapping[p][1] {
					t.Fatal("mapping differs from reference")
				}
			}
		})
	}
}

func TestLoopDeferredProjectionPairMatchesReference(t *testing.T) {
	// Registrations on exclusive branches, an owner reset, and a backedge.
	// Compare the compact product against two applications of the original
	// guard builder, independently of numbering/BFS traversal order.
	g := &loopConflictFrame{points: make([]loopConflictPoint, 6)}
	for _, edge := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}, {3, 4}, {4, 5}, {5, 0}} {
		g.edge(edge[0], edge[1])
	}
	first, second := loopDeferredGuard{[]int{1}, []int{0}}, loopDeferredGuard{[]int{2}, []int{0}}
	one, mapping := loopDeferredGuardGraph(loopSyncEscapeFrame{graph: g}, first.registrations, first.resets)
	mapped := func(points []int) []int {
		var out []int
		for _, p := range points {
			for _, q := range mapping[p] {
				if q >= 0 {
					out = append(out, q)
				}
			}
		}
		return out
	}
	reference, nested := loopDeferredGuardGraph(one, mapped(second.registrations), mapped(second.resets))
	var w loopDeferredGuardWorkspace
	got := w.project(loopSyncEscapeFrame{graph: g}, first, &second)
	if len(got.graph.points) != len(reference.graph.points) {
		t.Fatal("pair reachability differs")
	}
	oldToNew := make([]int, len(reference.graph.points))
	for p := range mapping {
		for a, q := range mapping[p] {
			for b := 0; b < 2; b++ {
				old := -1
				if q >= 0 {
					old = nested[q][b]
				}
				point := w.mapping[p][a|b<<1]
				if (old < 0) != (point < 0) {
					t.Fatal("pair state reachability differs")
				}
				if old >= 0 {
					oldToNew[old] = point
				}
			}
		}
	}
	for old, p := range reference.graph.points {
		var expected []int
		for _, next := range p.next {
			expected = append(expected, oldToNew[next])
		}
		actual := append([]int(nil), got.graph.points[oldToNew[old]].next...)
		sort.Ints(expected)
		sort.Ints(actual)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("pair product edges differ")
		}
	}
}

func TestLoopDeferredProjectionPublicSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         int
	}{
		{"exclusive registrations", `func work(xs []int,b bool){for range xs{n:=0;func(){if b{defer func(){go func(){n++}()}()}else{defer func(){go func(){n++}()}()}}()}}`, 0},
		{"conditional siblings", `func work(xs []int,b bool){for range xs{n:=0;func(){if b{defer func(){go func(){n++}()}();defer func(){go func(){n++}()}()}}()}}`, 2},
		{"unconditional and conditional siblings", `func work(xs []int,b bool){for range xs{n:=0;func(){defer func(){go func(){n++}()}();if b{defer func(){go func(){n++}()}()}}()}}`, 2},
		{"escape visible after call", `func work(xs []int){for range xs{n:=0;func(){defer func(){go func(){n++}()}()}();use(n)}}`, 1},
		{"named result read after defer", `func work(xs []int){for range xs{func()(n int){defer func(){go func(){n++}()}();return}()}}`, 1},
		{"saved operands and lifo", `func work(xs []int){for range xs{n,m:=0,0;p:=&n;func(){defer func(p *int){go func(){(*p)++}()}(p);defer func(){p=&m}()}();use(n)}}`, 1},
		{"slot copy is not data read", `func work(xs []int){for range xs{p:=new(int);func(){defer func(){go func(){(*p)++}()}()}();q:=p;_=q}}`, 0},
	} {
		for _, mode := range []string{"typed", "lexical", "partial"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				got := analyzeLoopRegression(t, loopTestPrelude+tc.source, "go1.23", mode)
				if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
					t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
				}
			})
		}
	}
}

func TestLoopDeferredProjectionRetainsOnlyLiveExecutionSeeds(t *testing.T) {
	const n = 40
	s, body := loopLiveTestScanner(t, "v:=0;"+strings.Repeat("defer func(){go func(){v++}()}();", n), true)
	s.discover(body, &loopWalk{})
	c, effects := s.beginFlowConflicts(), s.beginFlowEffects()
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: effects, conflicts: c}
	effects.aliases = f.aliases
	f.refine(body, nil)
	checked := 0
	for _, byKey := range f.simulated {
		for _, bucket := range byKey {
			for _, simulation := range bucket {
				checked++
				if len(simulation.execution.seed) > 8 {
					t.Fatalf("simulation retained unrelated saved registrations: %d bindings", len(simulation.execution.seed))
				}
			}
		}
	}
	for _, frame := range c.frames {
		if frame.execution != nil && len(frame.execution.seed) > 8 {
			t.Fatalf("execution frame retained unrelated saved registrations: %d bindings", len(frame.execution.seed))
		}
	}
	if checked < n {
		t.Fatal("fixture did not execute every deferred body")
	}
}

func BenchmarkLoopDeferredProjectionOnly(b *testing.B) {
	for _, n := range []int{240, 480} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			c, _ := loopDeferredProjectionFixture(n, false)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e := c.projectContextEscapes()
				if len(e.frames) != 2 || len(e.guarded) != 0 {
					b.Fatal("retained guard products")
				}
			}
		})
	}
}

func loopDeferredProjectionSource(n int) string {
	return loopTestPrelude + "func work(xs []int){for range xs{n:=0;func(){\n" + strings.Repeat("defer func(){go func(){n++}()}()\n", n) + "}()}}"
}

func BenchmarkLoopDeferredGuardProjection(b *testing.B) {
	for _, n := range []int{80, 120, 240, 480, 960} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			source := loopDeferredProjectionSource(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got := analyzeLoopRegression(b, source, "go1.23", "typed")
				if countRules(got)["goroutine-in-loop"] != n {
					b.Fatalf("lost deferred launch findings: %+v", got)
				}
			}
		})
	}
}
