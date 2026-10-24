package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// refineLaunchBindings runs after discovery and before reporting. Discovery
// supplies launch/loop/owner identities and defer diagnostics; the CFG supplies
// reachable binding states and synchronous effects through an intraprocedural
// may-analysis. Whole-variable assignments
// are strong updates; CFG edges join values and aggregate stores weakly extend
// sparse summaries. A safe whole-variable overwrite kills an earlier capture
// even when another assignment can reach the loop's back edge.
//
// Like value/expand, this is not a general interprocedural call/return analysis:
// calls do not supply binding side effects or inferred return values. Known
// range-function iterators are the explicit synchronous coroutine exception.
// Known immutable literal callees get CFG contexts seeded at actual calls;
// construction seeds remain a fallback for other nested literal frames.
func (s *loopScanner) refineLaunchBindings(root ast.Node) {
	if root == nil || len(s.launches) == 0 {
		return
	}
	if s.rootNode == nil {
		s.rootNode = root
	}
	for i := range s.launches {
		// An unreachable launch must not keep the lexical walk's old values.
		s.launches[i].values = nil
	}
	for _, region := range s.index.regions {
		region.copyReachable = false
	}
	s.index.reachable = map[ast.Node]bool{}
	conflicts := s.beginFlowConflicts()
	effects := s.beginFlowEffects()
	flow := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: effects, conflicts: conflicts}
	effects.aliases = flow.aliases
	flow.deferred = newLoopDeferredFlow(s, flow.aliases)
	var visit func(ast.Node)
	visit = func(n ast.Node) {
		switch n := n.(type) {
		case *ast.BlockStmt:
			flow.refine(n, nil)
		case *ast.FuncDecl:
			if n.Body != nil {
				flow.refine(n.Body, nil)
			}
		case *ast.FuncLit:
			frame := flow.refineContext(n.Body, nil, n, false)
			frame.root = true
		case ast.Stmt:
			flow.refine(&ast.BlockStmt{List: []ast.Stmt{n}}, nil)
		default:
			loopChildren(n, visit)
		}
	}
	visit(root)
	conflicts.publishContextLaunches()
	effects.finishFlowEffects()
	s.filterFlowReachability()
	s.flowConflicts = conflicts.finishFlowConflicts()
}

func (f *loopFlow) refine(body *ast.BlockStmt, seed map[*loopVariable]loopValue) {
	f.refineContext(body, seed, nil, false)
}

func (f *loopFlow) refineContext(body *ast.BlockStmt, seed map[*loopVariable]loopValue, owner *ast.FuncLit, actual bool) *loopConflictFrame {
	return f.refineDeferredContext(body, seed, owner, actual, nil)
}

func (f *loopFlow) refineDeferredContext(body *ast.BlockStmt, seed map[*loopVariable]loopValue, owner *ast.FuncLit, actual bool, execution *loopDeferredInvocation) *loopConflictFrame {
	s := f.s
	c := f.conflicts
	// Recursive actual invocations share the collector, not the physical
	// occurrence being replayed in their caller.
	physical, collect, effectFrame, states := f.effects.physical, f.effects.collect, f.effects.frame, f.effects.states
	f.effects.states = nil
	defer func() {
		f.effects.physical, f.effects.collect, f.effects.frame, f.effects.states = physical, collect, effectFrame, states
	}()
	if c.contexts == nil {
		c.contexts = map[*ast.BlockStmt]map[uint64][]*loopConflictFrame{}
		c.ids = map[*loopVariable]uint64{}
		c.callee = c.syncLiteralCallee()
		c.refined = true
		f.effects.conflicts, f.effects.callee = c, c.callee
		f.effects.invoke = func(call *ast.CallExpr, w *loopWalk) loopValue {
			targets, opaque := f.invocationContexts(call, w, true)
			if len(targets) == 0 {
				return nil
			}
			value := loopValue{}
			if opaque {
				loopMerge(value, s.expand(s.invokeValue(call.Fun, w), w))
			}
			for _, target := range targets {
				loopMerge(value, loopSyncOutside(f.effects.summaries[target], target.owner))
			}
			return value
		}
	}
	if f.deferred == nil {
		f.deferred = newLoopDeferredFlow(s, f.aliases)
	}
	key := c.contextKey(seed, actual)
	for _, prior := range c.contexts[body][key] {
		if prior.actual == actual && loopContextBindingsEqual(prior.seed, seed) && loopDeferredExecutionEqual(prior.execution, execution) {
			return prior
		}
	}
	prepared := f.prepareDeferredCFG(body, owner)
	adapter, graph, plan := prepared.adapter, prepared.graph, prepared.plan
	if adapter.iteratorIncomplete != nil {
		s.finding(adapter.iteratorIncomplete, "iterator-analysis-incomplete", "iterator control-flow expansion exceeded its analysis budget; goroutine-in-loop results may be incomplete")
	}
	frame := c.recordGraph(graph, adapter)
	frame.owner, frame.actual, frame.root, frame.seed = owner, actual, owner == nil, seed
	frame.execution = execution
	if c.contexts[body] == nil {
		c.contexts[body] = map[uint64][]*loopConflictFrame{}
	}
	c.contexts[body][key] = append(c.contexts[body][key], frame)
	live := f.loopFlowLiveness(graph, adapter, seed)
	inputs := make([]map[*loopVariable]loopValue, len(graph.Blocks))
	savedInputs := make([]*loopDeferredBindings, len(graph.Blocks))
	entry := &loopWalk{values: loopCloneValues(seed), persistentDeferred: plan.persistent}
	f.deferred.enter(owner, entry)
	inputs[0] = loopPruneBindings(entry.values, live[0], f.aliases, f.deferred.companions)
	queue := []*cfg.Block{graph.Blocks[0]}
	queued := make([]bool, len(graph.Blocks))
	queued[0] = true
	for len(queue) != 0 {
		block := queue[0]
		queue = queue[1:]
		queued[block.Index] = false
		w := &loopWalk{values: loopPruneBindings(inputs[block.Index], live[block.Index], f.aliases, f.deferred.companions), aliases: f.aliases, reference: execution, persistentDeferred: plan.persistent, deferredBindings: savedInputs[block.Index]}
		for _, node := range block.Nodes {
			f.advanceLoopEpoch(node, w, adapter)
			f.transferDeferredNode(plan, node, w, adapter, execution)
			if w.values == nil {
				break
			}
			w.values = plan.executeExit(node, w, f.aliasSlot, f.deferredRunner(nil, nil, nil), false)
			if w.values == nil {
				break
			}
		}
		if w.values == nil {
			continue
		}
		for _, next := range block.Succs {
			first := inputs[next.Index] == nil
			if first {
				inputs[next.Index] = map[*loopVariable]loopValue{}
			}
			outgoing := loopPruneBindings(w.values, live[next.Index], f.aliases, f.deferred.companions)
			changed := loopJoinBindings(inputs[next.Index], outgoing)
			joined := loopDeferredJoin(savedInputs[next.Index], w.deferredBindings, 28)
			changed = changed || joined != savedInputs[next.Index]
			savedInputs[next.Index] = joined
			// Even an empty state must propagate reachability.
			if (first || changed) && !queued[next.Index] {
				queue = append(queue, next)
				queued[next.Index] = true
			}
		}
	}

	// Replay only after convergence: intermediate loop states must not pollute
	// launch snapshots or the entry states of nested function frames.
	literals := map[*ast.FuncLit]map[*loopVariable]loopValue{}
	summary := loopValue{}
	f.effects.summaries[frame] = summary
	f.effects.frame = frame
	f.effects.collect = func(access loopDirectAccess) {
		summary[access.variable] = loopEpochMergeMode(summary[access.variable], access.mode)
	}
	for _, block := range graph.Blocks {
		if inputs[block.Index] == nil {
			continue
		}
		w := &loopWalk{values: loopPruneBindings(inputs[block.Index], live[block.Index], f.aliases, f.deferred.companions), aliases: f.aliases, reference: execution, persistentDeferred: plan.persistent, deferredBindings: savedInputs[block.Index]}
		for nodeIndex, node := range block.Nodes {
			site := frame.sites[block][nodeIndex]
			f.effects.physical = func(access loopDirectAccess) {
				for _, point := range site.nodes[access.node] {
					frame.effects[point] = append(frame.effects[point], access)
				}
			}
			f.advanceLoopEpoch(node, w, adapter)
			f.effects.states = nil
			if execution != nil {
				trace := *w
				trace.values = loopCloneValues(w.values)
				f.effects.states = map[ast.Node]*loopWalk{}
				f.evaluateDeferredCalls(node, &trace, func(n ast.Node, current *loopWalk) {
					snapshot := *current
					snapshot.values = loopCloneValues(current.values)
					f.effects.states[n] = &snapshot
				})
				w.operands = trace.operands
			}
			s.recordFlowReachability(node)
			if _, _, returning := plan.returnInfo(node); returning {
				plan.recordReturnBefore(node, w, f.effects)
			} else {
				f.effects.recordFlowEffects(node, w)
			}
			if rng := adapter.entries[node]; rng != nil {
				if adapter.iteratorEntries[node] {
					// This abstract execution edge has no physical pre-yield use.
					sink, collector := f.effects.physical, f.effects.collect
					// The inlined body supplies this context's execution
					// summary with its real binding states. The abstract edge
					// is still needed by source-global capture refinement, not
					// by the physical or context-local execution collectors.
					f.effects.physical, f.effects.collect = nil, nil
					f.effects.iteratorSummary(rng.stmt.X, w)
					f.effects.physical, f.effects.collect = sink, collector
				} else {
					f.effects.rangeInvocation(rng.stmt.X, w)
				}
			}
			if region := adapter.copySteps[node]; region != nil {
				region.copyReachable = true
			}
			if goStmt, ok := node.(*ast.GoStmt); ok {
				environment := w
				if state := f.effects.states[node]; state != nil {
					environment = state
				}
				targets, opaque := f.invocationContexts(goStmt.Call, environment, false)
				for _, target := range targets {
					value := loopSyncOutside(f.effects.summaries[target], target.owner)
					loopMerge(summary, value)
					frame.launches = append(frame.launches, loopSyncLaunch{stmt: goStmt, point: site.launch, values: value, target: target})
				}
				if opaque {
					value := f.goPayload(goStmt.Call, environment)
					loopMerge(summary, value)
					frame.launches = append(frame.launches, loopSyncLaunch{stmt: goStmt, point: site.launch, values: value})
				}
			}
			for _, call := range site.calls {
				if execution != nil && f.effects.states[call.call] == nil {
					continue // excluded/unevaluated expression, not a fallback call
				}
				environment := w
				if state := f.effects.states[call.call]; state != nil {
					environment = state
				}
				targets, _ := f.invocationContexts(call.call, environment, true)
				for _, target := range targets {
					call.target = target
					frame.calls = append(frame.calls, call)
				}
			}
			s.inspectExecuted(node, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok {
					if execution != nil && f.effects.states[lit] == nil {
						return false
					}
					if literals[lit] == nil {
						literals[lit] = map[*loopVariable]loopValue{}
					}
					needed := map[*loopVariable]bool{}
					for v := range s.index.captures[lit] {
						needed[v] = true
					}
					values := w.values
					if state := f.effects.states[lit]; state != nil {
						values = state.values
					}
					loopJoinBindings(literals[lit], loopPruneBindings(values, needed, f.aliases, f.deferred.companions))
					return false // its body executes in a separate frame
				}
				return true
			})
			f.transferDeferredNode(plan, node, w, adapter, execution)
			if w.values == nil {
				frame.cutAfter(site.after)
				break
			}
			f.replayDeferredExit(plan, node, w, frame, site)
			if w.values == nil {
				break
			}
		}
	}
	for lit, entry := range literals {
		if !adapter.inlinedIterators[lit] {
			child := f.refineContext(lit.Body, entry, lit, false)
			frame.constructors = append(frame.constructors, child)
		}
	}
	return frame
}

// goPayload copies the callee/argument values at their evaluation boundaries,
// then follows reference cells in the environment after synchronous operands
// have completed. In particular &p is a live cell, while p and f are copies.
func (f *loopFlow) goPayload(call *ast.CallExpr, w *loopWalk) loopValue {
	value := loopValue{}
	if saved := w.operands[call]; saved != nil {
		loopMerge(value, saved.callee)
		for _, arg := range saved.args {
			loopMerge(value, arg)
		}
		for v, mode := range value {
			value[v] = loopActivateCallableMode(mode)
		}
	} else {
		loopMerge(value, f.s.invokeValue(call.Fun, w))
		for _, arg := range call.Args {
			loopMerge(value, f.s.invokeValue(arg, w))
		}
	}
	return f.s.expand(value, w)
}

// Ordinary calls retain the immutable-literal contract. During deferred
// execution the saved/current callable-origin lattice resolves all alternatives;
// each gets its own actual environment, never a constructor's stale summary.
func (f *loopFlow) invocationContexts(call *ast.CallExpr, w *loopWalk, synchronous bool) ([]*loopConflictFrame, bool) {
	if f.effects.frame != nil && f.effects.frame.execution != nil {
		literals, opaque := f.deferred.callTargets(call, w)
		var targets []*loopConflictFrame
		for _, lit := range literals {
			inv := f.deferredCallInvocation(call, lit, w)
			targets = append(targets, f.refineDeferredContext(lit.Body, f.deferredSeed(inv), lit, synchronous, &inv))
		}
		return targets, opaque
	}
	lit := f.conflicts.callee(call.Fun)
	if lit == nil {
		return nil, true
	}
	return []*loopConflictFrame{f.invocationLiteralContext(call, lit, w, synchronous)}, false
}

func (f *loopFlow) invocationLiteralContext(call *ast.CallExpr, lit *ast.FuncLit, w *loopWalk, synchronous bool) *loopConflictFrame {
	needed := map[*loopVariable]bool{}
	for v := range f.s.index.captures[lit] {
		needed[v] = true
	}
	entry := loopPruneBindings(w.values, needed, f.aliases, f.deferred.companions)
	arg := 0
	if lit.Type.Params != nil {
		for _, field := range lit.Type.Params.List {
			_, variadic := field.Type.(*ast.Ellipsis)
			for _, name := range field.Names {
				end := arg + 1
				packed := variadic && !call.Ellipsis.IsValid()
				if packed || end > len(call.Args) {
					end = len(call.Args)
				}
				value, aliases, origins := loopValue{}, loopValue{}, loopValue{}
				for ; arg < end; arg++ {
					loopMerge(value, f.s.value(call.Args[arg], w))
					loopMerge(origins, f.deferred.origins(call.Args[arg], w))
					// Packing arguments creates a new slice of their VALUES, not
					// an address alias to the first argument's pointee. An explicit
					// ellipsis instead passes the existing slice value unchanged.
					if !packed {
						loopMerge(aliases, f.addressAliases(call.Args[arg], w))
					}
				}
				// Blank names have no indexed cell, but still consume their
				// tuple position (including grouped/repeated blanks).
				if v := f.s.index.ids[name]; v != nil {
					entry[v] = value
					entry[f.aliasSlot(v)] = aliases
					entry[f.deferred.companion(v)] = origins
				}
			}
		}
	}
	// actual distinguishes synchronous projection contexts in the recorder.
	// Async targets are activated by their launch edge instead; they may share
	// an identical constructor seed, but never a different environment.
	return f.refineContext(lit.Body, entry, lit, synchronous)
}

// The pinned cfg version keys labels by ast.Ident.Obj, which is nil with
// parser.SkipObjectResolution. Clone control nodes and supply function-local
// label objects, without mutating the caller's AST or its identity index.
// Expressions and ordinary statements retain their original identities.
//
// Also normalize two approximations in that CFG: range iteration assignments
// belong on every body entry, and select receive assignments belong only on the
// selected edge, not in the common channel-evaluation block.
type loopFlowAST struct {
	labels    map[string]*ast.Object
	entries   map[ast.Node]*loopFlowRange
	steps     map[ast.Node]*loopFlowRange
	index     *loopIndex
	copySteps map[ast.Node]*loopRegion

	// Synthetic select nodes retain their source identities for declaration
	// births, but never carry the source receive's executable channel operand.
	selectSources map[ast.Node]ast.Node

	iterators          []*loopFlowIterator
	iteratorEntries    map[ast.Node]bool
	inlinedIterators   map[*ast.FuncLit]bool
	rangeLabels        map[*ast.RangeStmt]string
	iteratorTemplate   bool
	iteratorIncomplete ast.Node
	deferred           *loopDeferredFlow
	deferredPlan       *loopDeferredPlan
	owner              *ast.FuncLit
	exports            map[*loopVariable]bool
}

func (a *loopFlowAST) label(id *ast.Ident) *ast.Ident {
	if id == nil {
		return nil
	}
	obj := a.labels[id.Name]
	if obj == nil {
		obj = ast.NewObj(ast.Lbl, id.Name)
		a.labels[id.Name] = obj
	}
	copy := *id
	copy.Obj = obj
	return &copy
}

func (a *loopFlowAST) block(block *ast.BlockStmt) *ast.BlockStmt {
	if block == nil {
		return nil
	}
	copy := *block
	copy.List = make([]ast.Stmt, len(block.List))
	for i, stmt := range block.List {
		copy.List[i] = a.stmt(stmt)
	}
	return &copy
}

func (a *loopFlowAST) stmt(stmt ast.Stmt) ast.Stmt {
	switch n := stmt.(type) {
	case *ast.BlockStmt:
		return a.block(n)
	case *ast.LabeledStmt:
		copy := *n
		if rng, ok := n.Stmt.(*ast.RangeStmt); ok {
			if a.rangeLabels == nil {
				a.rangeLabels = map[*ast.RangeStmt]string{}
			}
			a.rangeLabels[rng] = n.Label.Name
		}
		copy.Label, copy.Stmt = a.label(n.Label), a.stmt(n.Stmt)
		return &copy
	case *ast.BranchStmt:
		copy := *n
		copy.Label = a.label(n.Label)
		return &copy
	case *ast.IfStmt:
		copy := *n
		copy.Body, copy.Else = a.block(n.Body), a.stmt(n.Else)
		return &copy
	case *ast.ForStmt:
		copy := *n
		copy.Body = a.block(n.Body)
		if a.index != nil {
			if region := a.index.regions[n]; region != nil && len(region.copies) != 0 {
				marker := &ast.EmptyStmt{Semicolon: n.For}
				a.copySteps[marker] = region
				post := []ast.Stmt{marker}
				if n.Post != nil {
					post = append(post, n.Post)
				}
				copy.Post = &ast.BlockStmt{List: post}
			}
		}
		return &copy
	case *ast.RangeStmt:
		// Retain RangeStmt (rather than wrapping in a block) so a label on
		// the range still has the correct break/continue destinations.
		copy := *n
		// Plan parents before their callbacks: nested pending returns must run
		// the inner iterator's false continuation before the outer one's.
		rng := &loopFlowRange{stmt: n, entry: n.X, key: &loopVariable{}, value: &loopVariable{}}
		a.planIterator(rng)
		if rng.iterator != nil || a.iteratorTemplate {
			rng.entry = loopIteratorEntry(n.X)
		}
		copy.Body = a.block(n.Body)
		// Evaluated X is snapshotted once, before the range header. For the
		// constant-len exception, an opaque node prevents reads, effects, and
		// literal-constructor seeds. Keep rng.stmt original for target types
		// and body-entry assignments, but do not create an evaluation entry.
		if a.index != nil && !a.index.rangeExpressionEvaluated(n) {
			copy.X = &ast.BasicLit{ValuePos: n.X.Pos(), Kind: token.INT, Value: "0"}
		} else {
			copy.X = rng.entry
			a.entries[rng.entry] = rng
		}
		var lhs []ast.Expr
		for _, expr := range []ast.Expr{n.Key, n.Value} {
			if expr != nil {
				lhs = append(lhs, expr)
			}
		}
		if len(lhs) != 0 {
			step := &ast.AssignStmt{Lhs: lhs, Tok: token.ASSIGN}
			a.steps[step] = rng
			copy.Body.List = append([]ast.Stmt{step}, copy.Body.List...)
		}
		copy.Key, copy.Value = nil, nil
		return &copy
	case *ast.SwitchStmt:
		copy := *n
		copy.Body = a.block(n.Body)
		return &copy
	case *ast.TypeSwitchStmt:
		copy := *n
		copy.Body = a.block(n.Body)
		return &copy
	case *ast.CaseClause:
		copy := *n
		copy.Body = a.block(&ast.BlockStmt{List: n.Body}).List
		return &copy
	case *ast.SelectStmt:
		copy := *n
		copy.Body = a.block(n.Body)
		return &copy
	case *ast.CommClause:
		copy := *n
		copy.Body = a.block(&ast.BlockStmt{List: n.Body}).List
		if channel, selected := a.selectReceive(n.Comm); selected != nil {
			// cfg evaluates every Comm before choosing an arm. Only the
			// channel operand belongs there, never the receive or its LHS.
			evaluation := &ast.ExprStmt{X: channel}
			a.selectSources[evaluation] = a.selectSources[channel]
			copy.Comm = evaluation
			copy.Body = append([]ast.Stmt{selected}, copy.Body...)
		}
		return &copy
	default:
		return stmt
	}
}
