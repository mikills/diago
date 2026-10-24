package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// CFG templates and binding simulations are shared by convergence and replay.
// Simulations never publish effects, findings, launches or execution contexts.
// In particular a constructor frame is not a substitute for executing a defer.
type loopDeferredCFG struct {
	graph   *cfg.CFG
	adapter *loopFlowAST
	plan    *loopDeferredPlan
}
type loopDeferredSimulation struct {
	seed             map[*loopVariable]loopValue
	execution        *loopDeferredInvocation
	output           map[*loopVariable]loopValue
	computing, dirty bool
	dependents       map[*loopDeferredSimulation]bool
}
type loopDeferredOperands struct {
	callee, origins             loopValue
	args, addresses, argOrigins []loopValue
	// The nil call key holds statement-local expression emissions. These never
	// enter CFG bindings or the persistent defer-registration tree.
	expressions map[ast.Expr]loopDeferredExpression
}

type loopDeferredExpression struct {
	value, address, origins loopValue
	roots, keys             loopValue
	destination             bool
}

type loopDeferredPhysical struct {
	point     int
	effects   []loopDirectAccess
	target    *loopConflictFrame
	completed bool
}

// Projection metadata is mutable; the source graph (including physical
// effects) is not. Edges and all other indexes can stay shared here.
func loopDeferredProjectionGraph(source *loopConflictFrame) *loopConflictFrame {
	g := *source
	g.effects = make(map[int][]loopDirectAccess, len(source.effects))
	for p, effects := range source.effects {
		g.effects[p] = append([]loopDirectAccess(nil), effects...)
	}
	return &g
}

type loopDeferredGuard struct {
	registrations, resets []int
}

// One workspace per source CFG, not per deferred pair. At most four reachable
// states per source point are needed. Queue, edges, indexes and effect slices
// retain their capacity between solves; no graph is retained by the consumer.
// Bits in flags are register(1,2) and reset(4,8). States are measured at point
// entry, matching loopDeferredGuardGraph (registration completes on departure).
type loopDeferredGuardWorkspace struct {
	mapping  [][4]int
	flags    []uint8
	queue    []int
	states   int
	graph    loopConflictFrame
	launches []loopSyncLaunch
}

func (w *loopDeferredGuardWorkspace) outgoing(point, active int) int {
	flags := int(w.flags[point])
	return active&^(flags>>2) | flags&3
}

func (w *loopDeferredGuardWorkspace) walk(source *loopConflictFrame, first loopDeferredGuard, second *loopDeferredGuard) {
	n := len(source.points)
	if cap(w.mapping) < n {
		w.mapping = make([][4]int, n)
		w.flags = make([]uint8, n)
	}
	w.mapping, w.flags = w.mapping[:n], w.flags[:n]
	for i := range w.mapping {
		w.mapping[i] = [4]int{-1, -1, -1, -1}
		w.flags[i] = 0
	}
	w.states = 2
	guards := [2]*loopDeferredGuard{&first, second}
	for i, guard := range guards {
		if guard == nil {
			continue
		}
		for _, p := range guard.registrations {
			w.flags[p] |= 1 << i
		}
		for _, p := range guard.resets {
			w.flags[p] |= 4 << i
		}
	}
	if second != nil {
		w.states = 4
	}
	w.queue = w.queue[:0]
	if n == 0 {
		return
	}
	w.mapping[0][0] = 0
	w.queue = append(w.queue, 0)
	for head := 0; head < len(w.queue); head++ {
		current := w.queue[head]
		p, active := current/4, current%4
		active = w.outgoing(p, active)
		for _, next := range source.points[p].next {
			if w.mapping[next][active] < 0 {
				w.mapping[next][active] = len(w.queue)
				w.queue = append(w.queue, next*4+active)
			}
		}
	}
}

func (w *loopDeferredGuardWorkspace) alwaysActive(source *loopConflictFrame, registrations, resets []int, events []loopDeferredPhysical) bool {
	w.walk(source, loopDeferredGuard{registrations: registrations, resets: resets}, nil)
	for _, event := range events {
		if w.mapping[event.point][0] >= 0 || w.mapping[event.point][1] < 0 {
			return false
		}
	}
	return true
}

func loopDeferredClearIndex[K comparable, V any](index map[K][]V) {
	for key, values := range index {
		index[key] = values[:0]
	}
}

func (w *loopDeferredGuardWorkspace) project(source loopSyncEscapeFrame, first loopDeferredGuard, second *loopDeferredGuard) loopSyncEscapeFrame {
	w.walk(source.graph, first, second)
	g := &w.graph
	if g.nodes == nil {
		g.nodes = map[ast.Node][]int{}
		g.gos = map[*ast.GoStmt]int{}
		g.effects = map[int][]loopDirectAccess{}
		g.copies = map[*loopVariable][]int{}
		g.births = map[*loopVariable][]int{}
		g.iterations = map[*loopVariable][]int{}
	}
	// Include unused capacity: a smaller previous product must not discard
	// adjacency buffers belonging to states needed by a later product.
	g.points = g.points[:cap(g.points)]
	for i := range g.points {
		g.points[i].next = g.points[i].next[:0]
		g.points[i].prev = g.points[i].prev[:0]
	}
	for len(g.points) < len(w.queue) {
		g.points = append(g.points, loopConflictPoint{})
	}
	g.points = g.points[:len(w.queue)]
	for from, current := range w.queue {
		p, active := current/4, current%4
		active = w.outgoing(p, active)
		for _, next := range source.graph.points[p].next {
			g.edge(from, w.mapping[next][active])
		}
	}
	loopDeferredClearIndex(g.nodes)
	loopDeferredClearIndex(g.effects)
	loopDeferredClearIndex(g.copies)
	loopDeferredClearIndex(g.births)
	loopDeferredClearIndex(g.iterations)
	clear(g.gos)
	for node, oldPoints := range source.graph.nodes {
		for _, old := range oldPoints {
			w.eachPoint(old, 0, func(p int) { g.nodes[node] = append(g.nodes[node], p) })
		}
	}
	for old, accesses := range source.graph.effects {
		w.eachPoint(old, 0, func(p int) { g.effects[p] = append(g.effects[p], accesses...) })
	}
	for stmt, old := range source.graph.gos {
		w.eachPoint(old, 0, func(p int) { g.gos[stmt] = p })
	}
	for _, index := range []struct{ src, dst map[*loopVariable][]int }{
		{source.graph.copies, g.copies}, {source.graph.births, g.births}, {source.graph.iterations, g.iterations},
	} {
		for v, oldPoints := range index.src {
			for _, old := range oldPoints {
				w.eachPoint(old, 0, func(p int) { index.dst[v] = append(index.dst[v], p) })
			}
		}
	}
	w.launches = w.launches[:0]
	for _, launch := range source.launches {
		w.eachPoint(launch.point, 0, func(p int) {
			copy := launch
			copy.point = p
			w.launches = append(w.launches, copy)
		})
	}
	g.execution = source.graph.execution
	return loopSyncEscapeFrame{graph: g, launches: w.launches}
}

func (w *loopDeferredGuardWorkspace) eachPoint(old, required int, emit func(int)) {
	for active := 0; active < w.states; active++ {
		if p := w.mapping[old][active]; p >= 0 && active&required == required {
			emit(p)
		}
	}
}

func (w *loopDeferredGuardWorkspace) activePoints(required int) func(int, func(int)) {
	return func(old int, emit func(int)) { w.eachPoint(old, required, emit) }
}

func (w *loopWalk) referenceMode(v *loopVariable, mode loopAccess) loopAccess {
	if w.reference != nil {
		return w.reference.referenceMode(v, mode)
	}
	return mode
}

func loopDeferredExecutionEqual(a, b *loopDeferredInvocation) bool {
	if a == nil || b == nil {
		return a == b
	}
	return loopContextBindingsEqual(map[*loopVariable]loopValue{nil: a.callee}, map[*loopVariable]loopValue{nil: b.callee})
}

func (f *loopFlow) prepareDeferredCFG(body *ast.BlockStmt, owner *ast.FuncLit) *loopDeferredCFG {
	if f.prepared == nil {
		f.prepared = map[*ast.BlockStmt]*loopDeferredCFG{}
	}
	if prior := f.prepared[body]; prior != nil {
		return prior
	}
	a := &loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}, index: f.s.index, copySteps: map[ast.Node]*loopRegion{}, deferred: f.deferred, owner: owner, exports: map[*loopVariable]bool{}}
	ast.Inspect(body, func(n ast.Node) bool {
		if _, lit := n.(*ast.FuncLit); lit {
			return false
		}
		if rng, ok := n.(*ast.RangeStmt); ok {
			f.rangeTargetTypes(rng)
		}
		return true
	})
	for v := range f.s.index.captures[owner] {
		a.exports[v], a.exports[f.aliasSlot(v)], a.exports[f.deferred.companion(v)] = true, true, true
	}
	graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
	a.expandIterators(graph)
	var births map[ast.Node][]*loopVariable
	if f.conflicts != nil {
		births = f.conflicts.births
	}
	a.finishSelectCFG(graph, births)
	p := f.deferred.plan(graph, owner)
	p.factorOperands(graph)
	a.deferredPlan = p
	out := &loopDeferredCFG{graph: graph, adapter: a, plan: p}
	f.prepared[body] = out
	return out
}

func (f *loopFlow) deferredSeed(inv loopDeferredInvocation) map[*loopVariable]loopValue {
	needed := map[*loopVariable]bool{}
	for v := range f.s.index.captures[inv.literal] {
		needed[v] = true
	}
	if inv.literal.Type.Params != nil {
		for _, field := range inv.literal.Type.Params.List {
			for _, name := range field.Names {
				needed[f.s.index.ids[name]] = true
			}
		}
	}
	return loopPruneBindings(inv.seed, needed, f.aliases, f.deferred.companions)
}

func (f *loopFlow) simulateDeferred(inv loopDeferredInvocation) map[*loopVariable]loopValue {
	seed := f.deferredSeed(inv)
	// The cache must not retain the caller's entire pending defer stack
	// through its execution descriptor. Only live callee bindings are used.
	inv.seed = seed
	if f.simulated == nil {
		f.simulated = map[*ast.BlockStmt]map[uint64][]*loopDeferredSimulation{}
	}
	body := inv.literal.Body
	if f.simulated[body] == nil {
		f.simulated[body] = map[uint64][]*loopDeferredSimulation{}
	}
	key := f.conflicts.contextKey(seed, true)
	var result *loopDeferredSimulation
	for _, prior := range f.simulated[body][key] {
		if loopContextBindingsEqual(prior.seed, seed) && loopDeferredExecutionEqual(prior.execution, &inv) {
			result = prior
			break
		}
	}
	if result == nil {
		result = &loopDeferredSimulation{seed: seed, execution: &inv, dirty: true, dependents: map[*loopDeferredSimulation]bool{}}
		f.simulated[body][key] = append(f.simulated[body][key], result)
	}
	if f.simulating != nil {
		result.dependents[f.simulating] = true
	}
	if result.computing || !result.dirty {
		return result.output
	}
	parent := f.simulating
	f.simulating = result
	result.computing = true
	defer func() { f.simulating = parent; result.computing = false }()
	for result.dirty {
		result.dirty = false
		output := f.solveDeferred(inv, seed)
		completed := output != nil && result.output == nil
		if completed {
			result.output = map[*loopVariable]loopValue{}
		}
		changed := loopJoinBindings(result.output, output)
		if completed || changed {
			// A recursive callee initially exposes the bottom approximation,
			// not a permanent proof that it cannot return. Invalidate only its
			// dependent contexts, including cycles, until their bindings settle.
			seen := map[*loopDeferredSimulation]bool{result: true}
			queue := []*loopDeferredSimulation{result}
			for head := 0; head < len(queue); head++ {
				for dependent := range queue[head].dependents {
					dependent.dirty = true
					if !seen[dependent] {
						seen[dependent] = true
						queue = append(queue, dependent)
					}
				}
			}
		}
	}
	return result.output
}

func (f *loopFlow) solveDeferred(inv loopDeferredInvocation, seed map[*loopVariable]loopValue) map[*loopVariable]loopValue {
	prepared := f.prepareDeferredCFG(inv.literal.Body, inv.literal)
	graph, a, p := prepared.graph, prepared.adapter, prepared.plan
	live := f.loopFlowLiveness(graph, a, seed)
	entry := &loopWalk{values: loopCloneValues(seed), aliases: f.aliases, persistentDeferred: p.persistent}
	f.deferred.enter(inv.literal, entry)
	inputs := make([]map[*loopVariable]loopValue, len(graph.Blocks))
	savedInputs := make([]*loopDeferredBindings, len(graph.Blocks))
	inputs[0] = loopPruneBindings(entry.values, live[0], f.aliases, f.deferred.companions)
	var output map[*loopVariable]loopValue
	queue := []*cfg.Block{graph.Blocks[0]}
	queued := make([]bool, len(graph.Blocks))
	queued[0] = true
	for head := 0; head < len(queue); head++ {
		block := queue[head]
		queued[block.Index] = false
		w := &loopWalk{values: loopPruneBindings(inputs[block.Index], live[block.Index], f.aliases, f.deferred.companions), aliases: f.aliases, reference: &inv, persistentDeferred: p.persistent, deferredBindings: savedInputs[block.Index]}
		for _, node := range block.Nodes {
			f.advanceLoopEpoch(node, w, a)
			f.transferDeferredNode(p, node, w, a, &inv)
			if w.values == nil {
				break
			}
			w.values = p.executeExit(node, w, f.aliasSlot, f.deferredRunner(nil, nil, nil), false)
			if w.values == nil {
				break
			}
		}
		if w.values == nil {
			continue
		}
		if len(block.Succs) == 0 {
			if output == nil {
				output = map[*loopVariable]loopValue{}
			}
			loopJoinBindings(output, w.values)
		}
		for _, next := range block.Succs {
			first := inputs[next.Index] == nil
			if first {
				inputs[next.Index] = map[*loopVariable]loopValue{}
			}
			changed := loopJoinBindings(inputs[next.Index], loopPruneBindings(w.values, live[next.Index], f.aliases, f.deferred.companions))
			joined := loopDeferredJoin(savedInputs[next.Index], w.deferredBindings, 28)
			changed = changed || joined != savedInputs[next.Index]
			savedInputs[next.Index] = joined
			if (first || changed) && !queued[next.Index] {
				queue = append(queue, next)
				queued[next.Index] = true
			}
		}
	}
	return output
}

// deferredAddressExports finds caller cells, not retained contents. A parameter
// can belong to this invocation while its address proof names an ancestor's
// cell. Pruned seeds already close transitive pointer chains, so inspect only
// their present bindings, never the global alias index. New addresses created
// here name lexical captures; updated pointer cells in output can add further
// proven referents. Callback captures/data references alone are NOT proofs.
func (f *loopFlow) deferredAddressExports(owner *ast.FuncLit, state map[*loopVariable]loopValue) loopValue {
	out := loopValue{}
	for v := range state {
		for target, mode := range state[f.aliases[v]] {
			if target.ident == nil || f.s.index.external[target.ident.Name] == target {
				continue
			}
			// Never hand callee locals/parameters (including nested callees')
			// bindings into ancestor frames, even when their addresses escape.
			for ancestor := f.s.index.locations[owner].owner; ; ancestor = f.s.index.locations[ancestor].owner {
				if target.owner == ancestor {
					out[target] = loopEpochMergeMode(out[target], mode)
					break
				}
				if ancestor == nil {
					break
				}
			}
		}
	}
	return out
}

func (f *loopFlow) exportDeferred(inv loopDeferredInvocation, before, output map[*loopVariable]loopValue) map[*loopVariable]loopValue {
	captures := f.s.index.captures[inv.literal]
	references := f.deferredAddressExports(inv.literal, inv.seed)
	loopMerge(references, f.deferredAddressExports(inv.literal, output))
	for v := range captures {
		// Direct captures use the saved callable's epoch, not a possibly old
		// address to the same source cell carried by another parameter.
		references[v] = inv.referenceMode(v, loopRead)
	}
	for v, reference := range references {
		value := output[v]
		if _, captured := captures[v]; !captured {
			// Address-only aggregate handback keeps the coarse may-contents
			// contract. Do not promote a collapsed object summary to the
			// strong callable/pointer-cell identity used by its companions.
			switch f.s.syntaxType(v.ident).(type) {
			case *ast.StructType, *ast.ArrayType, *ast.MapType:
				value = loopValue{}
				loopMerge(value, before[v])
				loopMerge(value, output[v])
			}
		}
		before[v] = loopDeferredWriteback(reference, before[v], value)
		for _, slot := range []*loopVariable{f.aliasSlot(v), f.deferred.companion(v)} {
			before[slot] = loopDeferredWriteback(reference, before[slot], output[slot])
		}
	}
	return before
}

func (f *loopFlow) deferredRunner(frame *loopConflictFrame, exit ast.Node, points map[*ast.DeferStmt]int) loopDeferredRunner {
	return func(inv loopDeferredInvocation, state map[*loopVariable]loopValue, replay bool) map[*loopVariable]loopValue {
		if inv.literal == nil {
			if replay {
				var accesses []loopDirectAccess
				sink := f.effects.physical
				f.effects.physical = func(a loopDirectAccess) { accesses = append(accesses, a) }
				f.effects.recordOpaqueDeferredInvocation(inv, exit, exit.End(), &loopWalk{values: state, aliases: f.aliases})
				f.effects.physical = sink
				frame.delayed[inv.stmt] = append(frame.delayed[inv.stmt], loopDeferredPhysical{point: points[inv.stmt], effects: accesses, completed: true})
			}
			return state
		}
		// Keep execution contexts independent of unrelated saved registrations.
		// state below remains the full caller state for LIFO writeback.
		inv.seed = f.deferredSeed(inv)
		output := f.simulateDeferred(inv)
		if replay {
			target := f.refineDeferredContext(inv.literal.Body, f.deferredSeed(inv), inv.literal, true, &inv)
			point := points[inv.stmt]
			frame.calls = append(frame.calls, loopContextCall{call: inv.stmt.Call, point: point, target: target, guard: inv.stmt})
			var accesses []loopDirectAccess
			for v, mode := range loopSyncOutside(f.effects.summaries[target], target.owner) {
				accesses = append(accesses, loopDirectAccess{node: exit, variable: v, mode: mode, effect: true})
				if f.effects.collect != nil {
					f.effects.collect(loopDirectAccess{variable: v, mode: mode})
				}
			}
			frame.delayed[inv.stmt] = append(frame.delayed[inv.stmt], loopDeferredPhysical{point: point, effects: accesses, target: target, completed: output != nil})
		}
		if output == nil {
			return map[*loopVariable]loopValue{f.deferred.blocked: {f.deferred.blocked: loopRetain}}
		}
		return f.exportDeferred(inv, state, output)
	}
}

// Binding effects of ordinary calls remain deliberately uninferred. Only calls
// actually executing inside a deferred invocation use this binding interpreter.
func (f *loopFlow) transferDeferredNode(p *loopDeferredPlan, node ast.Node, w *loopWalk, a *loopFlowAST, execution *loopDeferredInvocation) {
	if execution != nil {
		f.deferredCalls(node, w)
		if w.values == nil {
			return
		}
	}
	f.transferDeferredOrigins(node, w)
	p.transfer(node, w, f.addressAliases)
	f.transfer(node, w, a)
	f.transferDeferredReturn(p, node, w)
}

func (f *loopFlow) deferredCalls(node ast.Node, w *loopWalk) {
	f.evaluateDeferredCalls(node, w, nil)
}

// Observe evaluation-time environments, not one pre-statement environment for
// every call. Callee and each argument are saved before evaluating the next
// operand; free cells are still loaded when the invocation actually executes.
func (f *loopFlow) evaluateDeferredCalls(node ast.Node, w *loopWalk, observe func(ast.Node, *loopWalk)) {
	w.operands = map[*ast.CallExpr]*loopDeferredOperands{nil: {expressions: map[ast.Expr]loopDeferredExpression{}}}
	var visit func(ast.Node)
	operands := func(call *ast.CallExpr) {
		visit(call.Fun)
		if f.s.deferredUnevaluated(call) {
			return
		}
		callee := f.deferredExpression(call.Fun, w)
		saved := &loopDeferredOperands{callee: callee.value, origins: callee.origins}
		for _, arg := range call.Args {
			visit(arg)
		}
		// Retain every emitted argument. Bare non-call argument loads can
		// additionally occur after a later call; completed operands and the
		// saved callee itself must never be replaced by that later state.
		f.joinDeferredUnorderedLoads(call.Args, w)
		for _, arg := range call.Args {
			value := f.deferredExpression(arg, w)
			saved.args = append(saved.args, value.value)
			saved.addresses = append(saved.addresses, value.address)
			saved.argOrigins = append(saved.argOrigins, value.origins)
		}
		w.operands[call] = saved
	}
	visit = func(n ast.Node) {
		if n == nil || w.values == nil {
			return
		}
		if observe != nil {
			observe(n, w)
		}
		if expr, ok := n.(ast.Expr); ok {
			// A non-call load can be unordered relative to calls inside this
			// expression. Keep both possible loads, but never reopen a completed
			// RHS/argument/aggregate when a later operand executes.
			unordered := f.deferredUnorderedLoad(expr)
			var before loopDeferredExpression
			if unordered {
				before = f.currentDeferredExpression(expr, w)
			}
			defer func() {
				if w.values != nil {
					value := f.emitDeferredExpression(expr, w)
					if unordered {
						value.value = loopDeferredUnion(value.value, before.value)
						value.address = loopDeferredUnion(value.address, before.address)
						value.origins = loopDeferredUnion(value.origins, before.origins)
					}
					w.operands[nil].expressions[expr] = value
				}
			}()
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				visit(lhs)
				if w.values == nil {
					return
				}
				value := f.deferredExpression(lhs, w)
				value.roots, value.keys = f.storeRoots(lhs, w), f.storeKeys(lhs, w)
				value.destination = true
				w.operands[nil].expressions[lhs] = value
			}
			for _, rhs := range n.Rhs {
				visit(rhs)
			}
			f.joinDeferredUnorderedLoads(n.Rhs, w)
			return
		case *ast.ValueSpec:
			loopChildren(n, visit)
			f.joinDeferredUnorderedLoads(n.Values, w)
			return
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				visit(n.X) // the left operand executes exactly once
				if w.values == nil {
					return
				}
				left, known := f.s.deferredTruth(n.X)
				if known && (n.Op == token.LAND && !left || n.Op == token.LOR && left) {
					return
				}
				if known {
					visit(n.Y)
					return
				}
				// The RHS can strongly update captured cells, but only on its
				// execution edge. Preserve the skipped edge, including origins
				// and private address/epoch slots, at the expression join.
				skipped := loopCloneValues(w.values)
				visit(n.Y)
				loopJoinBindings(skipped, w.values)
				w.values = skipped
				return
			}
		case *ast.GoStmt:
			operands(n.Call)
			if observe != nil && w.values != nil {
				observe(n, w)
			}
			return
		case *ast.DeferStmt:
			operands(n.Call)
			if observe != nil && w.values != nil {
				observe(n, w)
			}
			return
		case *ast.CallExpr:
			operands(n)
			if w.values == nil {
				return
			}
			if observe != nil {
				observe(n, w)
			}
			targets, opaque := f.deferred.callTargets(n, w)
			if len(targets) == 0 {
				return
			}
			out := map[*loopVariable]loopValue{}
			completed := opaque
			if opaque {
				loopJoinBindings(out, w.values)
			}
			for _, lit := range targets {
				inv := f.deferredCallInvocation(n, lit, w)
				output := f.simulateDeferred(inv)
				if output == nil {
					continue
				}
				completed = true
				loopJoinBindings(out, f.exportDeferred(inv, loopCloneValues(w.values), output))
			}
			if completed {
				w.values = out
			} else {
				w.values = nil
			}
			return
		}
		loopChildren(n, visit)
	}
	visit(node)
}

// deferredExpression reads an emitted value, never a reconstructed RHS in the
// post-statement environment. The fallback is also used by ordinary CFGs,
// whose calls deliberately have no inferred binding effects.
func (f *loopFlow) deferredExpression(expr ast.Expr, w *loopWalk) loopDeferredExpression {
	if saved := w.operands[nil]; saved != nil {
		if value, ok := saved.expressions[expr]; ok {
			return value
		}
	}
	return f.currentDeferredExpression(expr, w)
}

func (f *loopFlow) currentDeferredExpression(expr ast.Expr, w *loopWalk) loopDeferredExpression {
	out := loopDeferredExpression{
		value: f.s.value(expr, w), address: f.addressAliases(expr, w),
	}
	if f.deferred != nil && w.operands[nil] != nil {
		out.origins = f.deferred.origins(expr, w)
	}
	return out
}

func loopDeferredUnion(a, b loopValue) loopValue {
	out := loopValue{}
	loopMerge(out, a)
	loopMerge(out, b)
	return out
}

// Calls have lexical order, but ordinary loads within an indexing/selection
// expression can occur on either side of a nested call. A may-join is required
// here; it is NOT permission to reload a previously completed composite RHS.
func (f *loopFlow) deferredUnorderedLoad(expr ast.Expr) bool {
	switch loopUnparen(expr).(type) {
	case *ast.IndexExpr, *ast.SelectorExpr, *ast.SliceExpr, *ast.StarExpr, *ast.UnaryExpr, *ast.TypeAssertExpr:
	default:
		return false
	}
	return f.deferredExpressionHasCall(expr)
}

func (f *loopFlow) deferredExpressionHasCall(expr ast.Expr) bool {
	found := false
	f.s.inspectExecuted(expr, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if call, ok := n.(*ast.CallExpr); ok && !f.s.deferredUnevaluated(call) {
			found = true
		}
		return !found
	})
	return found
}

// Ordinary loads (including array/struct literal elements) can be delayed
// relative to a sibling call. A map construction, however, has already copied
// its elements before a later RHS call. Keep both permissible ordinary loads
// without reopening completed maps/calls. The saved callee is never included:
// it has already been copied before argument evaluation.
func (f *loopFlow) joinDeferredUnorderedLoads(exprs []ast.Expr, w *loopWalk) {
	if w.values == nil {
		return
	}
	laterCall := false
	for i := len(exprs) - 1; i >= 0; i-- {
		expr := exprs[i]
		if laterCall {
			w.operands[nil].expressions[expr] = f.deferredDelayedLoad(expr, w)
		}
		laterCall = laterCall || f.deferredExpressionHasCall(expr)
	}
}

func (f *loopFlow) deferredDelayedLoad(expr ast.Expr, w *loopWalk) loopDeferredExpression {
	before := f.deferredExpression(expr, w)
	var after loopDeferredExpression
	switch e := loopUnparen(expr).(type) {
	case *ast.CompositeLit:
		if f.s.index.mapLiteral(e) {
			return before
		}
		// Array/struct/slice operands may load late, but a nested map still
		// owns its already-copied elements. Never re-evaluate the whole tree.
		after = before
		after.value = loopValue{}
		for _, elt := range e.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			loopMerge(after.value, f.deferredDelayedLoad(elt, w).value)
		}
	case *ast.CallExpr:
		if len(e.Args) != 1 || !f.s.conversion(e.Fun) {
			return before // ordinary call results remain opaque
		}
		after = f.deferredDelayedLoad(e.Args[0], w)
	case *ast.SelectorExpr, *ast.IndexExpr:
		var base ast.Expr
		if sel, ok := e.(*ast.SelectorExpr); ok {
			base = sel.X
		} else {
			base = e.(*ast.IndexExpr).X
		}
		if lit := f.s.directLiteral(base); lit != nil {
			if f.s.index.mapLiteral(lit) {
				return before
			}
			if projected, ok := f.s.projection(expr); ok {
				after = f.deferredDelayedLoad(projected, w)
			} else {
				after = f.deferredDelayedLoad(lit, w)
			}
		} else {
			after = f.currentDeferredExpression(expr, w)
		}
	case *ast.Ident, *ast.StarExpr, *ast.SliceExpr, *ast.TypeAssertExpr:
		after = f.currentDeferredExpression(expr, w)
	default:
		return before
	}
	before.value = loopDeferredUnion(before.value, after.value)
	before.address = loopDeferredUnion(before.address, after.address)
	before.origins = loopDeferredUnion(before.origins, after.origins)
	return before
}

func (f *loopFlow) emitDeferredExpression(expr ast.Expr, w *loopWalk) loopDeferredExpression {
	// Aggregate construction copies each emitted element, not the element's
	// source cell after subsequent calls. Reuse original AST identities for
	// projections, origin interning, and type information.
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return f.deferredExpression(e.X, w)
	case *ast.CompositeLit:
		out := loopDeferredExpression{value: loopValue{}, origins: loopValue{f.deferred.opaque: loopRetain}}
		var elements []ast.Expr
		isMap := f.s.index.mapLiteral(e)
		for _, elt := range e.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if isMap {
					elements = append(elements, kv.Key)
				}
				elt = kv.Value
			}
			elements = append(elements, elt)
		}
		f.joinDeferredUnorderedLoads(elements, w)
		for _, elt := range elements {
			loopMerge(out.value, f.deferredExpression(elt, w).value)
		}
		return out
	case *ast.CallExpr:
		if len(e.Args) == 1 && f.s.conversion(e.Fun) {
			return f.deferredExpression(e.Args[0], w)
		}
	case *ast.SelectorExpr, *ast.IndexExpr:
		if projected, ok := f.s.projection(expr); ok {
			if saved := w.operands[nil]; saved != nil {
				if value, ok := saved.expressions[projected]; ok {
					return value
				}
			}
		}
	}
	return f.currentDeferredExpression(expr, w)
}

func (d *loopDeferredFlow) callTargets(call *ast.CallExpr, w *loopWalk) (targets []*ast.FuncLit, opaque bool) {
	if saved := w.operands[call]; saved != nil {
		for token := range saved.origins {
			if lit := d.literals[token]; lit != nil {
				targets = append(targets, lit)
			} else {
				opaque = true
			}
		}
		return targets, opaque || len(targets) == 0
	}
	return d.callees(call.Fun, w)
}

func (f *loopFlow) deferredCallInvocation(call *ast.CallExpr, lit *ast.FuncLit, w *loopWalk) loopDeferredInvocation {
	// Reuse the saved-operand binder without registering a synthetic defer.
	s := &loopDeferredSlot{}
	state := loopCloneValues(w.values)
	s.callee, s.origins = &loopVariable{}, &loopVariable{}
	saved := w.operands[call]
	state[s.callee] = f.s.value(call.Fun, w)
	if saved != nil {
		state[s.callee] = saved.callee
	}
	state[s.origins] = loopValue{f.deferred.literal(lit): loopRetain}
	for i, arg := range call.Args {
		value, address, origin := &loopVariable{}, &loopVariable{}, &loopVariable{}
		s.args = append(s.args, value)
		s.addresses = append(s.addresses, address)
		s.argOrigins = append(s.argOrigins, origin)
		state[value] = f.s.value(arg, w)
		state[address] = f.addressAliases(arg, w)
		state[origin] = f.deferred.origins(arg, w)
		if saved != nil {
			state[value], state[address], state[origin] = saved.args[i], saved.addresses[i], saved.argOrigins[i]
		}
	}
	return (&loopDeferredPlan{flow: f.deferred}).invocations(s, state, f.aliasSlot)[0]
}

func (f *loopFlow) replayDeferredExit(p *loopDeferredPlan, node ast.Node, w *loopWalk, frame *loopConflictFrame, site *loopConflictSite) {
	if _, ok := p.exits[node]; !ok {
		return
	}
	events := p.events(node, w)
	g := frame
	points := map[*ast.DeferStmt]int{}
	for stmt := range events.next {
		points[stmt] = g.point()
	}
	// Terminal events need a point even when they have no predecessor entries.
	for stmt := range events.done {
		if _, ok := points[stmt]; !ok {
			points[stmt] = g.point()
		}
	}
	for _, stmt := range events.entries {
		if _, ok := points[stmt]; !ok {
			points[stmt] = g.point()
		}
	}
	start := site.after
	next := append([]int(nil), g.points[start].next...)
	for _, to := range next {
		prev := g.points[to].prev[:0]
		for _, from := range g.points[to].prev {
			if from != start {
				prev = append(prev, from)
			}
		}
		g.points[to].prev = prev
	}
	g.points[start].next = nil
	done := g.point()
	for _, to := range next {
		g.edge(done, to)
	}
	if events.empty {
		g.edge(start, done)
	}
	for _, stmt := range events.entries {
		g.edge(start, points[stmt])
	}
	for stmt, older := range events.next {
		for _, prior := range older {
			g.edge(points[stmt], points[prior])
		}
	}
	for stmt := range events.done {
		g.edge(points[stmt], done)
	}
	if frame.delayed == nil {
		frame.delayed = map[*ast.DeferStmt][]loopDeferredPhysical{}
	}
	w.values = p.executeExit(node, w, f.aliasSlot, f.deferredRunner(frame, node, points), true)
	for stmt, point := range points {
		completed := false
		for _, event := range frame.delayed[stmt] {
			if event.point == point && event.completed {
				completed = true
			}
		}
		if !completed {
			frame.cutAfter(point)
		}
	}
	if w.values != nil {
		sink := f.effects.physical
		f.effects.physical = func(a loopDirectAccess) { frame.effects[done] = append(frame.effects[done], a) }
		p.recordReturnAfter(node, w, f.effects)
		f.effects.physical = sink
	}
}
