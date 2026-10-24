package diago

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestLoopDeferredPersistentSnapshots(t *testing.T) {
	v, u := &loopVariable{}, &loopVariable{}
	var root *loopDeferredBindings
	keys := []uint32{0, 15, 16, 255, 256, 65535, 1 << 24, 1 << 31, ^uint32(0)}
	var versions []*loopDeferredBindings
	for _, key := range keys {
		versions = append(versions, root)
		root = loopDeferredPut(root, key, loopValue{v: loopRead}, 28)
	}
	for i, before := range versions {
		for j, key := range keys {
			if got := len(loopDeferredGet(before, key)) != 0; got != (j < i) {
				t.Fatalf("snapshot %d changed at key %d", i, key)
			}
		}
	}
	all := map[uint32]loopValue{}
	loopDeferredEach(root, 0, 28, func(key uint32, value loopValue) { all[key] = value })
	if len(all) != len(keys) {
		t.Fatal("iteration lost sparse keys")
	}
	for _, key := range keys {
		if all[key][v] != loopRead {
			t.Fatal("iteration changed a value")
		}
	}
	if got := loopDeferredPut(root, 12345, nil, 28); got != root {
		t.Fatal("absent deletion copied the tree")
	}
	changed := loopDeferredPut(root, 0, loopValue{v: loopRead | loopPriorRef, u: loopWrite}, 28)
	joined := loopDeferredJoin(root, changed, 28)
	want := loopValue{v: loopRead | loopPriorRef | loopEpochMixedRef, u: loopWrite}
	if !reflect.DeepEqual(loopDeferredGet(joined, 0), want) {
		t.Fatal("join lost epoch alternatives")
	}
	if loopDeferredGet(root, 0)[v] != loopRead {
		t.Fatal("join mutated its input")
	}
	if loopDeferredJoin(joined, root, 28) != joined {
		t.Fatal("subset join did not share its input")
	}
	aged := loopDeferredAge(joined, map[*loopVariable]bool{v: true}, 28)
	if loopDeferredGet(aged, 0)[v] != loopRead|loopPriorRef {
		t.Fatal("saved references did not age")
	}
	if !reflect.DeepEqual(loopDeferredGet(joined, 0), want) {
		t.Fatal("aging mutated a snapshot")
	}
	if loopDeferredAge(aged, map[*loopVariable]bool{v: true}, 28) != aged {
		t.Fatal("stable aging copied the tree")
	}
	for _, key := range keys {
		aged = loopDeferredPut(aged, key, nil, 28)
	}
	if aged != nil {
		t.Fatal("consumed operands retained tree nodes")
	}
}

func TestLoopDeferredEventBindingsShareUntilChanged(t *testing.T) {
	v, u := &loopVariable{}, &loopVariable{}
	before := map[*loopVariable]loopValue{v: {u: loopRead}}
	incoming := map[*loopVariable]loopValue{v: {u: loopRead | loopPriorRef}}
	joined, changed := loopDeferredJoinBindings(before, incoming)
	if !changed || joined[v][u] != loopRead|loopPriorRef|loopEpochMixedRef {
		t.Fatal("event join lost allocation alternatives")
	}
	if before[v][u] != loopRead || incoming[v][u] != loopRead|loopPriorRef {
		t.Fatal("event join mutated immutable inputs")
	}
	allocs := testing.AllocsPerRun(5, func() {
		_, changed := loopDeferredJoinBindings(joined, before)
		if changed {
			panic("subset changed a binding state")
		}
	})
	if allocs != 0 {
		t.Fatalf("unchanged event joins copy source cells: %.0f allocations", allocs)
	}
}

func TestLoopDeferredPersistentLinearHistory(t *testing.T) {
	for _, n := range []int{120, 240, 480, 960} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var roots []*loopDeferredBindings
			var root *loopDeferredBindings
			v := &loopVariable{}
			for i := 0; i < n; i++ {
				root = loopDeferredPut(root, uint32(i), loopValue{v: loopRead}, 28)
				roots = append(roots, root) // retain EVERY CFG boundary
			}
			seen := map[*loopDeferredBindings]bool{}
			var visit func(*loopDeferredBindings)
			visit = func(node *loopDeferredBindings) {
				if node == nil || seen[node] {
					return
				}
				seen[node] = true
				for _, child := range node.children {
					visit(child)
				}
			}
			for _, r := range roots {
				visit(r)
			}
			if len(seen) > 9*n {
				t.Fatalf("history copied operand tables: %d nodes for %d boundaries", len(seen), n)
			}
			allocs := testing.AllocsPerRun(2, func() {
				var r *loopDeferredBindings
				for i := 0; i < n; i++ {
					r = loopDeferredPut(r, uint32(i), loopValue{v: loopRead}, 28)
				}
				if r == nil {
					panic("lost root")
				}
			})
			if allocs > float64(12*n) {
				t.Fatalf("persistent updates allocate per history: %.0f", allocs)
			}
		})
	}
}

func TestLoopDeferredDynamicOperandsCompactLiveness(t *testing.T) {
	for _, n := range []int{120, 240, 480} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			s, body := loopLiveTestScanner(t, "v:=0;p:=&v;b:=true;"+strings.Repeat("defer func(p *int){go func(){(*p)++}()}(p);if b{use(v)};", n), true)
			f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}}
			f.deferred = newLoopDeferredFlow(s, f.aliases)
			prepared := f.prepareDeferredCFG(body, nil)
			for _, roots := range f.loopFlowLiveness(prepared.graph, prepared.adapter, nil) {
				if len(roots) > 12 {
					t.Fatalf("dynamic snapshots retained as CFG roots: %d", len(roots))
				}
			}
		})
	}
}

func BenchmarkLoopDeferredDynamicAcrossCFGBlocks(b *testing.B) {
	for _, n := range []int{120, 240, 480, 960} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			source := loopTestPrelude + "func work(xs []int){for range xs{n:=0;p:=&n;func(){" + strings.Repeat("defer func(p *int){go func(){(*p)++}()}(p);if len(xs)>1{use(n)};", n) + "}()}}"
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := analyzeLoopRegression(b, source, "go1.23", "typed"); countRules(got)["goroutine-in-loop"] != n {
					b.Fatal("lost dynamic launches")
				}
			}
		})
	}
}
