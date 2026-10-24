package diago

import (
	"go/ast"
	"go/token"
)

// Synchronous literal calls return without joining goroutines launched by the
// callee. Those launches also need events in the caller CFG, not just the
// separate CFG for the literal's body.
//
// recordSyncNode orders operand calls before an ordinary GoStmt event. Replay
// connects each synchronous call occurrence to a context seeded from that
// call's current free bindings. Projection consumes only those target frames;
// constructor snapshots must not leak into actual invocations.
//
// frame.launches includes ordinary and projected launches. Each event retains
// its original GoStmt identity and its own converged payload. Projection never
// mutates source graphs, so repeated finishes don't accumulate events. The source-indexed
// path below is retained for standalone recorders without binding replay.

// recordSyncNode indexes evaluation events, never a literal's body. In
// particular a CallExpr key denotes an ACTUAL synchronous invocation; the
// outer call of go/defer deliberately has no such key. Calls in its operands
// are ordinary synchronous calls. Call effects are indexed after operands and
// projected launches will be inserted immediately after those effects.
func (c *loopFlowConflicts) recordSyncNode(g *loopConflictFrame, node ast.Node, start int) int {
	last := start
	index := func(n ast.Node) {
		g.nodes[n] = append(g.nodes[n], last)
		if g.recording != nil {
			g.recording.nodes[n] = append(g.recording.nodes[n], last)
		}
		for _, v := range c.births[n] {
			g.births[v] = append(g.births[v], last)
		}
	}
	var visit func(ast.Node)
	// Locate destinations before the RHS, but index their eventual stores
	// afterwards. Only pointer/header/index operands load values here; a
	// struct/array root's identifier write is part of the later store too.
	var address func(ast.Expr, *[]ast.Node)
	address = func(expr ast.Expr, stores *[]ast.Node) {
		if expr == nil {
			return
		}
		switch n := expr.(type) {
		case *ast.ParenExpr:
			address(n.X, stores)
		case *ast.Ident:
		case *ast.StarExpr:
			visit(n.X)
		case *ast.SelectorExpr:
			if c.s.pointerValue(n.X) {
				visit(n.X)
			} else {
				address(n.X, stores)
			}
		case *ast.IndexExpr:
			if c.s.referenceIndex(n.X) {
				visit(n.X)
			} else {
				address(n.X, stores)
			}
			visit(n.Index)
		default:
			visit(expr)
			return
		}
		*stores = append(*stores, expr)
	}
	operands := func(call *ast.CallExpr) {
		if call == nil {
			return
		}
		visit(call.Fun)
		if c.s.deferredUnevaluated(call) {
			return
		}
		for _, arg := range call.Args {
			visit(arg)
		}
	}
	visit = func(n ast.Node) {
		if n == nil {
			return
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			index(n)
			return
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				visit(n.X)
				left, known := c.s.deferredTruth(n.X)
				skip := known && (n.Op == token.LAND && !left || n.Op == token.LOR && left)
				if !skip {
					before := last
					if !known {
						last = g.point()
						g.edge(before, last)
					}
					visit(n.Y)
					if !known {
						join := g.point()
						g.edge(last, join)
						g.edge(before, join)
						last = join
					}
				}
				index(n)
				return
			}
			loopChildren(n, visit)
		case *ast.GoStmt:
			operands(n.Call)
		case *ast.DeferStmt:
			operands(n.Call)
		case *ast.CallExpr:
			operands(n)
			p := g.point()
			g.edge(last, p)
			last = p
			index(n)
			if g.recording != nil {
				g.recording.calls = append(g.recording.calls, loopContextCall{call: n, point: p})
			}
			// Keep subsequent stores/operands after the projected escape,
			// including a destination in the very same assignment statement.
			last = g.point()
			g.edge(p, last)
			return
		case *ast.AssignStmt:
			var stores []ast.Node
			for _, lhs := range n.Lhs {
				address(lhs, &stores)
			}
			for _, rhs := range n.Rhs {
				visit(rhs)
			}
			for _, store := range stores {
				index(store)
			}
		case *ast.IncDecStmt:
			var stores []ast.Node
			address(n.X, &stores)
			for _, store := range stores {
				index(store)
			}
		default:
			loopChildren(n, visit)
		}
		index(n)
	}
	visit(node)
	return last
}

type loopSyncLaunch struct {
	stmt   *ast.GoStmt
	point  int
	values loopValue
	// An asynchronous edge activates its execution context, but is never
	// projected as a synchronous call in the launching frame.
	target *loopConflictFrame
}

type loopSyncEscapeFrame struct {
	graph    *loopConflictFrame
	launches []loopSyncLaunch
}

type loopSyncEscapes struct {
	// Guard products are borrowed scratch graphs, consumed before the next
	// product is built. Never retain one graph per deferred invocation pair.
	guarded  []func(func(loopSyncEscapeFrame))
	frames   []loopSyncEscapeFrame
	children map[*ast.FuncLit]map[*ast.GoStmt]loopValue
	callee   func(ast.Expr) *ast.FuncLit
	looped   map[*ast.GoStmt]bool
}

// forEachFrame is the conflict collector's iteration API. The callback must
// finish using a frame before returning: guarded products share scratch space.
func (e *loopSyncEscapes) forEachFrame(visit func(loopSyncEscapeFrame)) {
	for _, frame := range e.frames {
		visit(frame)
	}
	for _, guarded := range e.guarded {
		guarded(visit)
	}
}

type loopSyncCall struct {
	literal *ast.FuncLit
	points  []int
	inLoop  bool
}

// projectSyncEscapes follows direct literal invocation edges only. Literal
// construction alone, deferred calls and asynchronously invoked bodies do not
// make synchronous edges. Immutable local aliases are supported; general
// callee-origin/reassignment analysis is intentionally not guessed here.
func (c *loopFlowConflicts) projectSyncEscapes() *loopSyncEscapes {
	if c != nil && c.refined {
		return c.projectContextEscapes()
	}
	e := &loopSyncEscapes{children: map[*ast.FuncLit]map[*ast.GoStmt]loopValue{}, looped: map[*ast.GoStmt]bool{}}
	if c == nil {
		return e
	}
	e.callee = c.syncLiteralCallee()
	values := map[*ast.GoStmt]loopValue{}
	owners := map[*ast.GoStmt]*ast.FuncLit{}
	for _, launch := range c.s.launches {
		if values[launch.stmt] == nil {
			values[launch.stmt] = loopValue{}
		}
		loopMerge(values[launch.stmt], launch.values)
		owners[launch.stmt] = launch.owner
	}
	calls := map[*loopConflictFrame][]loopSyncCall{}
	edges := map[*ast.FuncLit][]*ast.FuncLit{}
	for _, g := range c.frames {
		for stmt := range g.gos {
			owner := owners[stmt]
			if e.children[owner] == nil {
				e.children[owner] = map[*ast.GoStmt]loopValue{}
			}
			e.children[owner][stmt] = values[stmt]
		}
		for node, points := range g.nodes {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				continue
			}
			lit := e.callee(call.Fun)
			if lit == nil {
				continue
			}
			calls[g] = append(calls[g], loopSyncCall{literal: lit, points: points, inLoop: len(c.s.index.locations[call].loops) != 0})
			owner := c.s.index.locations[call].owner
			edges[owner] = append(edges[owner], lit)
		}
	}
	// Known immutable callees form a DAG. Memoized summaries visit each edge
	// once, without comparing launch pairs. Apply lifetime filtering at EVERY
	// invocation boundary, including aliases defined outside the wrapper.
	done := map[*ast.FuncLit]bool{}
	var summarize func(*ast.FuncLit) map[*ast.GoStmt]loopValue
	summarize = func(owner *ast.FuncLit) map[*ast.GoStmt]loopValue {
		if done[owner] {
			return e.children[owner]
		}
		done[owner] = true
		if e.children[owner] == nil {
			e.children[owner] = map[*ast.GoStmt]loopValue{}
		}
		for _, callee := range edges[owner] {
			for stmt, value := range summarize(callee) {
				merged := loopSyncOutside(value, callee)
				loopMerge(merged, e.children[owner][stmt])
				e.children[owner][stmt] = merged
			}
		}
		return e.children[owner]
	}
	for owner := range edges {
		summarize(owner)
	}
	for _, original := range c.frames {
		frame := loopSyncEscapeFrame{graph: original}
		for stmt, point := range original.gos {
			frame.launches = append(frame.launches, loopSyncLaunch{stmt: stmt, point: point, values: values[stmt]})
		}
		for _, call := range calls[original] {
			var children []loopSyncLaunch
			for stmt, value := range e.children[call.literal] {
				outside := loopSyncOutside(value, call.literal)
				if len(outside) != 0 {
					if call.inLoop {
						e.looped[stmt] = true
					}
					children = append(children, loopSyncLaunch{stmt: stmt, values: outside})
				}
			}
			if len(children) == 0 {
				continue
			}
			if frame.graph == original {
				frame.graph = loopSyncCloneGraph(original)
			}
			for _, point := range call.points {
				g := frame.graph
				next := append([]int(nil), g.points[point].next...)
				// Fan out, do not put escaping siblings in an artificial
				// sequence: mutually exclusive child launches must not race
				// merely because they share a callsite. Their own CFG proves
				// body-internal conflicts; caller backedges still connect them.
				for _, child := range children {
					child.point = g.point()
					g.edge(point, child.point)
					for _, successor := range next {
						g.edge(child.point, successor)
					}
					frame.launches = append(frame.launches, child)
				}
			}
		}
		e.frames = append(e.frames, frame)
	}
	return e
}

// contextKey is order-independent and visits only live capture contents. Hash
// collisions are checked structurally; values and source variable identities
// remain immutable. Identical calls share one CFG instead of rebuilding an
// entire worker body at every callsite.
func (c *loopFlowConflicts) contextKey(seed map[*loopVariable]loopValue, actual bool) uint64 {
	id := func(v *loopVariable) uint64 {
		if c.ids[v] == 0 {
			c.ids[v] = uint64(len(c.ids) + 1)
		}
		return c.ids[v]
	}
	mix := func(x uint64) uint64 {
		x ^= x >> 30
		x *= 0xbf58476d1ce4e5b9
		x ^= x >> 27
		x *= 0x94d049bb133111eb
		return x ^ (x >> 31)
	}
	var key uint64
	if actual {
		key = 1
	}
	for v, value := range seed {
		if len(value) == 0 {
			continue
		}
		contents := uint64(0)
		for dep, mode := range value {
			contents ^= mix(id(dep)*0x9e3779b97f4a7c15 ^ mix(uint64(mode)))
		}
		key ^= mix(id(v)*0x9e3779b97f4a7c15 ^ mix(contents))
	}
	return key
}

func loopContextBindingsEqual(a, b map[*loopVariable]loopValue) bool {
	for v, av := range a {
		bv := b[v]
		if len(av) != len(bv) {
			return false
		}
		for dep, mode := range av {
			if bv[dep] != mode {
				return false
			}
		}
	}
	for v, bv := range b {
		if len(bv) != len(a[v]) {
			return false
		}
	}
	return true
}

// Construction seeds are a fallback for bodies whose execution is not known.
// Known go callees, like synchronous callees, use actual entry bindings.
// Actual invocation contexts must
// never acquire the constructor's stale binding state or its descendants.
func (c *loopFlowConflicts) activeContextFrames() []*loopConflictFrame {
	invoked := map[*ast.FuncLit]bool{}
	for _, frame := range c.frames {
		if frame.actual {
			invoked[frame.owner] = true
		}
		for _, launch := range frame.launches {
			if launch.target != nil {
				invoked[launch.target.owner] = true
			}
		}
	}
	seen := map[*loopConflictFrame]bool{}
	var active []*loopConflictFrame
	var visit func(*loopConflictFrame)
	visit = func(frame *loopConflictFrame) {
		if seen[frame] {
			return
		}
		seen[frame] = true
		active = append(active, frame)
		for _, call := range frame.calls {
			visit(call.target)
		}
		for _, launch := range frame.launches {
			if launch.target != nil {
				visit(launch.target)
			}
		}
		for _, child := range frame.constructors {
			if !invoked[child.owner] {
				visit(child)
			}
		}
	}
	for _, frame := range c.frames {
		if frame.root {
			visit(frame)
		}
	}
	return active
}

func (c *loopFlowConflicts) publishContextLaunches() {
	values := map[*ast.GoStmt]loopValue{}
	for _, frame := range c.activeContextFrames() {
		for _, launch := range frame.launches {
			if values[launch.stmt] == nil {
				values[launch.stmt] = loopValue{}
			}
			loopMerge(values[launch.stmt], launch.values)
		}
	}
	// Source-level values are for reporting only. Conflict proofs consume the
	// occurrence payloads below, NEVER this union across execution sites.
	for i := range c.s.launches {
		c.s.launches[i].values = values[c.s.launches[i].stmt]
	}
}

func (c *loopFlowConflicts) projectContextEscapes() *loopSyncEscapes {
	e := &loopSyncEscapes{children: map[*ast.FuncLit]map[*ast.GoStmt]loopValue{}, callee: c.callee, looped: map[*ast.GoStmt]bool{}}
	active := c.activeContextFrames()
	summaries := map[*loopConflictFrame]map[*ast.GoStmt]loopValue{}
	var summarize func(*loopConflictFrame) map[*ast.GoStmt]loopValue
	summarize = func(frame *loopConflictFrame) map[*ast.GoStmt]loopValue {
		if prior := summaries[frame]; prior != nil {
			return prior
		}
		out := map[*ast.GoStmt]loopValue{}
		summaries[frame] = out
		merge := func(stmt *ast.GoStmt, value loopValue) {
			if out[stmt] == nil {
				out[stmt] = loopValue{}
			}
			loopMerge(out[stmt], value)
		}
		for _, launch := range frame.launches {
			merge(launch.stmt, launch.values)
		}
		for _, call := range frame.calls {
			for stmt, value := range summarize(call.target) {
				merge(stmt, loopSyncOutside(value, call.target.owner))
			}
		}
		return out
	}
	for _, original := range active {
		// Async inheritance remains source-indexed, but its values come only
		// from active execution contexts, never an obsolete constructor frame.
		if e.children[original.owner] == nil {
			e.children[original.owner] = map[*ast.GoStmt]loopValue{}
		}
		for stmt, value := range summarize(original) {
			if e.children[original.owner][stmt] == nil {
				e.children[original.owner][stmt] = loopValue{}
			}
			loopMerge(e.children[original.owner][stmt], value)
		}
		frame := loopSyncEscapeFrame{graph: original, launches: append([]loopSyncLaunch(nil), original.launches...)}
		for _, call := range original.calls {
			if call.guard != nil {
				continue
			}
			var children []loopSyncLaunch
			for stmt, value := range summarize(call.target) {
				// Loop attribution describes execution, not escaping storage.
				// Even a callee-local capture can have a body-internal proof.
				if len(c.s.index.locations[call.call].loops) != 0 {
					e.looped[stmt] = true
				}
				outside := loopSyncOutside(value, call.target.owner)
				if len(outside) != 0 {
					children = append(children, loopSyncLaunch{stmt: stmt, values: outside})
				}
			}
			if len(children) == 0 {
				continue
			}
			if frame.graph == original {
				frame.graph = loopSyncCloneGraph(original)
			}
			g := frame.graph
			next := append([]int(nil), g.points[call.point].next...)
			for _, child := range children {
				child.point = g.point()
				g.edge(call.point, child.point)
				for _, successor := range next {
					g.edge(child.point, successor)
				}
				frame.launches = append(frame.launches, child)
			}
		}
		install := func(dst *loopSyncEscapeFrame, stmt *ast.DeferStmt, point func(int, func(int))) {
			for _, event := range original.delayed[stmt] {
				point(event.point, func(p int) {
					dst.graph.effects[p] = append(dst.graph.effects[p], event.effects...)
					if event.target != nil {
						for child, value := range summarize(event.target) {
							if len(c.s.index.locations[stmt].loops) != 0 {
								e.looped[child] = true
							}
							outside := loopSyncOutside(value, event.target.owner)
							if len(outside) != 0 {
								dst.launches = append(dst.launches, loopSyncLaunch{stmt: child, point: p, values: outside})
							}
						}
					}
				})
			}
		}
		var statements []*ast.DeferStmt
		for stmt := range original.delayed {
			statements = append(statements, stmt)
		}
		// A guard is redundant only if registration is active on EVERY
		// reachable path to every physical event, since the last owner reset.
		// Straight-line defer stacks therefore use one shared graph, not D²
		// copies of the whole function. Effects/payloads stay occurrence-local.
		var workspace loopDeferredGuardWorkspace
		var conditional []*ast.DeferStmt
		effectsOwned := false
		for _, stmt := range statements {
			owner := c.s.index.locations[stmt].owner
			if !workspace.alwaysActive(frame.graph, original.registrations[stmt], original.resets[owner], original.delayed[stmt]) {
				conditional = append(conditional, stmt)
				continue
			}
			if !effectsOwned {
				frame.graph = loopDeferredProjectionGraph(frame.graph)
				effectsOwned = true
			}
			install(&frame, stmt, func(p int, emit func(int)) { emit(p) })
		}
		e.frames = append(e.frames, frame)
		if len(conditional) != 0 {
			e.guarded = append(e.guarded, func(visit func(loopSyncEscapeFrame)) {
				var scratch loopDeferredGuardWorkspace
				for i, stmt := range conditional {
					first := loopDeferredGuard{registrations: original.registrations[stmt], resets: original.resets[c.s.index.locations[stmt].owner]}
					guarded := scratch.project(frame, first, nil)
					install(&guarded, stmt, scratch.activePoints(1))
					visit(guarded)
					scratch.launches = guarded.launches
					// Two independent guards preserve correlation at joins. The
					// product and metadata storage are reused for every pair.
					for _, other := range conditional[i+1:] {
						launches := false
						for _, s := range []*ast.DeferStmt{stmt, other} {
							for _, event := range original.delayed[s] {
								launches = launches || event.target != nil && len(summarize(event.target)) != 0
							}
						}
						if !launches {
							continue
						}
						second := &loopDeferredGuard{registrations: original.registrations[other], resets: original.resets[c.s.index.locations[other].owner]}
						pair := scratch.project(frame, first, second)
						install(&pair, stmt, scratch.activePoints(1))
						install(&pair, other, scratch.activePoints(2))
						visit(pair)
						scratch.launches = pair.launches
					}
				}
			})
		}
	}
	return e
}

// Callee locals/parameters are new allocations on EVERY invocation, even
// across a caller CFG backedge. Expanded references to caller-owned storage
// remain in the value; no values or allocation identities are synthesized.
func loopSyncOutside(value loopValue, callee *ast.FuncLit) loopValue {
	outside := loopValue{}
	for v, mode := range value {
		if v.owner != nil && v.owner.Pos() >= callee.Pos() && v.owner.End() <= callee.End() {
			continue
		}
		outside[v] = mode
	}
	return outside
}

// A definition with an initializer is a known callee only if its binding is
// never assigned or address-taken. An escaped address could replace it without
// any syntactic assignment to the identifier. This index is linear in accesses;
// memoization avoids rewalking alias chains at every invocation.
func (c *loopFlowConflicts) syncLiteralCallee() func(ast.Expr) *ast.FuncLit {
	unstable := map[*loopVariable]bool{}
	for _, access := range c.s.index.direct {
		if access.mode&(loopWrite|loopRetain) != 0 {
			unstable[access.variable] = true
		}
	}
	resolved := map[*loopVariable]*ast.FuncLit{}
	seen := map[*loopVariable]bool{}
	var resolve func(ast.Expr) *ast.FuncLit
	resolve = func(expr ast.Expr) *ast.FuncLit {
		switch expr := loopUnparen(expr).(type) {
		case *ast.FuncLit:
			return expr
		case *ast.Ident:
			v := c.s.index.ids[expr]
			if v == nil || v.ident == nil || unstable[v] || !v.valueInitializerKnown {
				return nil
			}
			if !seen[v] {
				seen[v] = true
				resolved[v] = resolve(v.valueInitializer)
			}
			return resolved[v]
		}
		return nil
	}
	return resolve
}

func loopSyncCloneGraph(g *loopConflictFrame) *loopConflictFrame {
	copy := *g
	copy.points = make([]loopConflictPoint, len(g.points))
	for i, p := range g.points {
		copy.points[i] = loopConflictPoint{next: append([]int(nil), p.next...), prev: append([]int(nil), p.prev...)}
	}
	return &copy
}

// inheritConflicts extends the existing variable-specific async delegation
// proof through synchronous wrappers as well. Only actual reachable launch /
// invocation edges occur in children. A conflict for one variable must never
// contaminate another capture, nor an unused function definition.
func (e *loopSyncEscapes) inheritConflicts(h *loopConflictHazards) {
	var queue []*ast.GoStmt
	queued := map[*ast.GoStmt]bool{}
	for stmt, external := range h.external {
		if len(external) != 0 {
			queue = append(queue, stmt)
			queued[stmt] = true
		}
	}
	for head := 0; head < len(queue); head++ {
		stmt := queue[head]
		queued[stmt] = false
		lit := e.callee(stmt.Call.Fun)
		if lit == nil {
			continue
		}
		for child, values := range e.children[lit] {
			changed := false
			for v, external := range h.external[stmt] {
				external = loopExternalConflicts(values[v], external)
				if external == 0 {
					continue
				}
				if h.external[child] == nil {
					h.external[child] = map[*loopVariable]uint8{}
				}
				if h.external[child][v]&external != external {
					h.external[child][v] |= external
					h.direct[child] = true
					changed = true
				}
			}
			if changed && !queued[child] {
				queue = append(queue, child)
				queued[child] = true
			}
		}
	}
}
