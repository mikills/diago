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
	r := &loopSyncRecorder{c: c, g: g, last: start}
	r.visit(node)
	return r.last
}

// loopSyncRecorder carries one recordSyncNode walk. Every event is indexed at
// last, the CFG point the evaluation has reached.
type loopSyncRecorder struct {
	c    *loopFlowConflicts
	g    *loopConflictFrame
	last int
}

func (r *loopSyncRecorder) index(n ast.Node) {
	r.g.nodes[n] = append(r.g.nodes[n], r.last)
	if r.g.recording != nil {
		r.g.recording.nodes[n] = append(r.g.recording.nodes[n], r.last)
	}
	for _, v := range r.c.births[n] {
		r.g.births[v] = append(r.g.births[v], r.last)
	}
}

func (r *loopSyncRecorder) visit(n ast.Node) {
	if n == nil {
		return
	}
	switch n := n.(type) {
	case *ast.FuncLit:
		// Index the construction only. The body is a separate frame.
	case *ast.BinaryExpr:
		r.binary(n)
	case *ast.GoStmt:
		r.operands(n.Call)
	case *ast.DeferStmt:
		r.operands(n.Call)
	case *ast.CallExpr:
		r.call(n)
		return
	case *ast.AssignStmt:
		r.assign(n)
	case *ast.IncDecStmt:
		r.indexStores(r.address(n.X, nil))
	default:
		loopChildren(n, r.visit)
	}
	r.index(n)
}

// binary branches around the right operand of && and ||, unless the left
// operand's truth is known. Other operators evaluate both operands in order.
func (r *loopSyncRecorder) binary(n *ast.BinaryExpr) {
	if n.Op != token.LAND && n.Op != token.LOR {
		loopChildren(n, r.visit)
		return
	}
	r.visit(n.X)
	left, known := r.c.s.deferredTruth(n.X)
	if known && (n.Op == token.LAND && !left || n.Op == token.LOR && left) {
		return
	}
	before := r.last
	if !known {
		r.last = r.g.point()
		r.g.edge(before, r.last)
	}
	r.visit(n.Y)
	if !known {
		join := r.g.point()
		r.g.edge(r.last, join)
		r.g.edge(before, join)
		r.last = join
	}
}

func (r *loopSyncRecorder) operands(call *ast.CallExpr) {
	if call == nil {
		return
	}
	r.visit(call.Fun)
	if r.c.s.deferredUnevaluated(call) {
		return
	}
	for _, arg := range call.Args {
		r.visit(arg)
	}
}

func (r *loopSyncRecorder) call(n *ast.CallExpr) {
	r.operands(n)
	p := r.g.point()
	r.g.edge(r.last, p)
	r.last = p
	r.index(n)
	if r.g.recording != nil {
		r.g.recording.calls = append(r.g.recording.calls, loopContextCall{call: n, point: p})
	}
	// Keep subsequent stores/operands after the projected escape,
	// including a destination in the very same assignment statement.
	r.last = r.g.point()
	r.g.edge(p, r.last)
}

func (r *loopSyncRecorder) assign(n *ast.AssignStmt) {
	var stores []ast.Node
	for _, lhs := range n.Lhs {
		stores = r.address(lhs, stores)
	}
	for _, rhs := range n.Rhs {
		r.visit(rhs)
	}
	r.indexStores(stores)
}

// Locate destinations before the RHS, but index their eventual stores
// afterwards. Only pointer/header/index operands load values here; a
// struct/array root's identifier write is part of the later store too.
func (r *loopSyncRecorder) address(expr ast.Expr, stores []ast.Node) []ast.Node {
	if expr == nil {
		return stores
	}
	switch n := expr.(type) {
	case *ast.ParenExpr:
		stores = r.address(n.X, stores)
	case *ast.Ident:
	case *ast.StarExpr:
		r.visit(n.X)
	case *ast.SelectorExpr:
		if r.c.s.pointerValue(n.X) {
			r.visit(n.X)
		} else {
			stores = r.address(n.X, stores)
		}
	case *ast.IndexExpr:
		if r.c.s.referenceIndex(n.X) {
			r.visit(n.X)
		} else {
			stores = r.address(n.X, stores)
		}
		r.visit(n.Index)
	default:
		r.visit(expr)
		return stores
	}
	return append(stores, expr)
}

func (r *loopSyncRecorder) indexStores(stores []ast.Node) {
	for _, store := range stores {
		r.index(store)
	}
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
		loopSyncMerge(values, launch.stmt, launch.values)
		owners[launch.stmt] = launch.owner
	}
	for _, g := range c.frames {
		for stmt := range g.gos {
			e.childrenOf(owners[stmt])[stmt] = values[stmt]
		}
	}
	calls, edges := c.syncLiteralCalls(e.callee)
	e.summarizeSyncCallees(edges)
	for _, original := range c.frames {
		e.frames = append(e.frames, e.projectSyncFrame(original, values, calls[original]))
	}
	return e
}

// syncLiteralCalls indexes each frame's direct literal invocations, and the
// owner-to-callee edges they add to the summary DAG.
func (c *loopFlowConflicts) syncLiteralCalls(callee func(ast.Expr) *ast.FuncLit) (map[*loopConflictFrame][]loopSyncCall, map[*ast.FuncLit][]*ast.FuncLit) {
	calls := map[*loopConflictFrame][]loopSyncCall{}
	edges := map[*ast.FuncLit][]*ast.FuncLit{}
	for _, g := range c.frames {
		for node, points := range g.nodes {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				continue
			}
			lit := callee(call.Fun)
			if lit == nil {
				continue
			}
			location := c.s.index.locations[call]
			calls[g] = append(calls[g], loopSyncCall{literal: lit, points: points, inLoop: len(location.loops) != 0})
			edges[location.owner] = append(edges[location.owner], lit)
		}
	}
	return calls, edges
}

// Known immutable callees form a DAG. Memoized summaries visit each edge
// once, without comparing launch pairs. Apply lifetime filtering at EVERY
// invocation boundary, including aliases defined outside the wrapper.
func (e *loopSyncEscapes) summarizeSyncCallees(edges map[*ast.FuncLit][]*ast.FuncLit) {
	done := map[*ast.FuncLit]bool{}
	var summarize func(*ast.FuncLit) map[*ast.GoStmt]loopValue
	summarize = func(owner *ast.FuncLit) map[*ast.GoStmt]loopValue {
		if done[owner] {
			return e.children[owner]
		}
		done[owner] = true
		children := e.childrenOf(owner)
		for _, callee := range edges[owner] {
			for stmt, value := range summarize(callee) {
				merged := loopSyncOutside(value, callee)
				loopMerge(merged, children[stmt])
				children[stmt] = merged
			}
		}
		return children
	}
	for owner := range edges {
		summarize(owner)
	}
}

// projectSyncFrame keeps a frame's ordinary launches and splices in the
// escaping launches of each direct literal call.
func (e *loopSyncEscapes) projectSyncFrame(original *loopConflictFrame, values map[*ast.GoStmt]loopValue, calls []loopSyncCall) loopSyncEscapeFrame {
	frame := loopSyncEscapeFrame{graph: original}
	for stmt, point := range original.gos {
		frame.launches = append(frame.launches, loopSyncLaunch{stmt: stmt, point: point, values: values[stmt]})
	}
	for _, call := range calls {
		children := loopSyncEscaping(e.children[call.literal], call.literal)
		if call.inLoop {
			for _, child := range children {
				e.looped[child.stmt] = true
			}
		}
		frame.splice(original, children, call.points...)
	}
	return frame
}

// childrenOf returns owner's launch summary, creating it on first use.
func (e *loopSyncEscapes) childrenOf(owner *ast.FuncLit) map[*ast.GoStmt]loopValue {
	children := e.children[owner]
	if children == nil {
		children = map[*ast.GoStmt]loopValue{}
		e.children[owner] = children
	}
	return children
}

// markLooped attributes every summarized launch to a looping execution.
func (e *loopSyncEscapes) markLooped(summary map[*ast.GoStmt]loopValue) {
	for stmt := range summary {
		e.looped[stmt] = true
	}
}

// splice inserts children after each call point, each rejoining that point's
// original successors. The source graph is cloned before its first change.
func (f *loopSyncEscapeFrame) splice(original *loopConflictFrame, children []loopSyncLaunch, points ...int) {
	if len(children) == 0 {
		return
	}
	if f.graph == original {
		f.graph = loopSyncCloneGraph(original)
	}
	g := f.graph
	for _, point := range points {
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
			f.launches = append(f.launches, child)
		}
	}
}

// loopSyncEscaping keeps the summarized launches whose payload still refers to
// storage outside callee. Splicing assigns their points.
func loopSyncEscaping(summary map[*ast.GoStmt]loopValue, callee *ast.FuncLit) []loopSyncLaunch {
	var children []loopSyncLaunch
	for stmt, value := range summary {
		if outside := loopSyncOutside(value, callee); len(outside) != 0 {
			children = append(children, loopSyncLaunch{stmt: stmt, values: outside})
		}
	}
	return children
}

func loopSyncMerge(values map[*ast.GoStmt]loopValue, stmt *ast.GoStmt, value loopValue) {
	if values[stmt] == nil {
		values[stmt] = loopValue{}
	}
	loopMerge(values[stmt], value)
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
	p := &loopContextProjection{
		c:         c,
		e:         &loopSyncEscapes{children: map[*ast.FuncLit]map[*ast.GoStmt]loopValue{}, callee: c.callee, looped: map[*ast.GoStmt]bool{}},
		summaries: map[*loopConflictFrame]map[*ast.GoStmt]loopValue{},
	}
	for _, original := range c.activeContextFrames() {
		p.project(original)
	}
	return p.e
}

// loopContextProjection is the state one projectContextEscapes pass shares
// across active frames: the escapes being built and memoized frame summaries.
type loopContextProjection struct {
	c         *loopFlowConflicts
	e         *loopSyncEscapes
	summaries map[*loopConflictFrame]map[*ast.GoStmt]loopValue
}

// summarize unions a frame's own launches with those of its synchronous
// calls, dropping callee-local storage at each call boundary.
func (p *loopContextProjection) summarize(frame *loopConflictFrame) map[*ast.GoStmt]loopValue {
	if prior := p.summaries[frame]; prior != nil {
		return prior
	}
	out := map[*ast.GoStmt]loopValue{}
	p.summaries[frame] = out
	for _, launch := range frame.launches {
		loopSyncMerge(out, launch.stmt, launch.values)
	}
	for _, call := range frame.calls {
		for stmt, value := range p.summarize(call.target) {
			loopSyncMerge(out, stmt, loopSyncOutside(value, call.target.owner))
		}
	}
	return out
}

func (p *loopContextProjection) project(original *loopConflictFrame) {
	// Async inheritance remains source-indexed, but its values come only
	// from active execution contexts, never an obsolete constructor frame.
	children := p.e.childrenOf(original.owner)
	for stmt, value := range p.summarize(original) {
		loopSyncMerge(children, stmt, value)
	}
	frame := p.callFrame(original)
	conditional := p.installUnconditional(&frame, original)
	p.e.frames = append(p.e.frames, frame)
	if len(conditional) != 0 {
		p.e.guarded = append(p.e.guarded, p.guardedProducts(original, frame, conditional))
	}
}

// callFrame copies a frame's launches and splices in the escaping launches of
// each unguarded synchronous call.
func (p *loopContextProjection) callFrame(original *loopConflictFrame) loopSyncEscapeFrame {
	frame := loopSyncEscapeFrame{graph: original, launches: append([]loopSyncLaunch(nil), original.launches...)}
	for _, call := range original.calls {
		if call.guard != nil {
			continue
		}
		summary := p.summarize(call.target)
		// Loop attribution describes execution, not escaping storage.
		// Even a callee-local capture can have a body-internal proof.
		if len(p.c.s.index.locations[call.call].loops) != 0 {
			p.e.markLooped(summary)
		}
		frame.splice(original, loopSyncEscaping(summary, call.target.owner), call.point)
	}
	return frame
}

// installUnconditional installs every defer whose guard is redundant into
// frame and returns the conditional rest, which need guarded products.
func (p *loopContextProjection) installUnconditional(frame *loopSyncEscapeFrame, original *loopConflictFrame) []*ast.DeferStmt {
	// A guard is redundant only if registration is active on EVERY
	// reachable path to every physical event, since the last owner reset.
	// Straight-line defer stacks therefore use one shared graph, not D²
	// copies of the whole function. Effects/payloads stay occurrence-local.
	var workspace loopDeferredGuardWorkspace
	var conditional []*ast.DeferStmt
	effectsOwned := false
	for stmt := range original.delayed {
		guard := p.guard(original, stmt)
		if !workspace.alwaysActive(frame.graph, guard.registrations, guard.resets, original.delayed[stmt]) {
			conditional = append(conditional, stmt)
			continue
		}
		if !effectsOwned {
			frame.graph = loopDeferredProjectionGraph(frame.graph)
			effectsOwned = true
		}
		p.install(frame, original, stmt, func(at int, emit func(int)) { emit(at) })
	}
	return conditional
}

// guardedProducts projects each conditional defer under its own guard, and
// each launching pair of them under both guards.
func (p *loopContextProjection) guardedProducts(original *loopConflictFrame, frame loopSyncEscapeFrame, conditional []*ast.DeferStmt) func(func(loopSyncEscapeFrame)) {
	return func(visit func(loopSyncEscapeFrame)) {
		var scratch loopDeferredGuardWorkspace
		for i, stmt := range conditional {
			first := p.guard(original, stmt)
			guarded := scratch.project(frame, first, nil)
			p.install(&guarded, original, stmt, scratch.activePoints(1))
			visit(guarded)
			scratch.launches = guarded.launches
			// Two independent guards preserve correlation at joins. The
			// product and metadata storage are reused for every pair.
			for _, other := range conditional[i+1:] {
				if !p.launchesAny(original, stmt, other) {
					continue
				}
				second := p.guard(original, other)
				pair := scratch.project(frame, first, &second)
				p.install(&pair, original, stmt, scratch.activePoints(1))
				p.install(&pair, original, other, scratch.activePoints(2))
				visit(pair)
				scratch.launches = pair.launches
			}
		}
	}
}

// guard pairs stmt's registrations with the resets of its owning literal.
func (p *loopContextProjection) guard(original *loopConflictFrame, stmt *ast.DeferStmt) loopDeferredGuard {
	return loopDeferredGuard{registrations: original.registrations[stmt], resets: original.resets[p.c.s.index.locations[stmt].owner]}
}

// launchesAny reports whether a physical event of stmts invokes a target with
// any summarized launch.
func (p *loopContextProjection) launchesAny(original *loopConflictFrame, stmts ...*ast.DeferStmt) bool {
	for _, stmt := range stmts {
		for _, event := range original.delayed[stmt] {
			if event.target != nil && len(p.summarize(event.target)) != 0 {
				return true
			}
		}
	}
	return false
}

// install replays stmt's physical events into dst at the points chosen by
// point: their effects, then the escaping launches of an invoked target.
func (p *loopContextProjection) install(dst *loopSyncEscapeFrame, original *loopConflictFrame, stmt *ast.DeferStmt, point func(int, func(int))) {
	for _, event := range original.delayed[stmt] {
		point(event.point, func(at int) {
			dst.graph.effects[at] = append(dst.graph.effects[at], event.effects...)
			if event.target == nil {
				return
			}
			summary := p.summarize(event.target)
			if len(p.c.s.index.locations[stmt].loops) != 0 {
				p.e.markLooped(summary)
			}
			for _, child := range loopSyncEscaping(summary, event.target.owner) {
				child.point = at
				dst.launches = append(dst.launches, child)
			}
		})
	}
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
