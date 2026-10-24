package diago

import (
	"go/ast"
	"go/token"
	"go/types"
)

// loopFlowEffects replaces binding-dependent lexical effects, not bindings.
// Known synchronous closures contribute their retained access modes; calls do
// not assign captured bindings or infer return values. Literal bodies belong
// to separate CFG frames, including literals constructed by go/defer operands.
//
// Wiring in refineLaunchBindings:
//
//	effects := s.beginFlowEffects()
//	flow := &loopFlow{..., effects: effects}
//	effects.aliases = flow.aliases
//	// visit/refine all reachable frames
//	effects.finishFlowEffects()
//
// In loopFlow.refine's POST-CONVERGENCE replay, before transfer, call
// f.effects.recordFlowEffects(node, w). Share the recorder with recursive
// literal-frame refinements. Never call it during the fixed-point iteration.
// Ordinary identifier accesses remain in index.direct: the separate direct-use
// liveness pass must filter those before report. This recorder rebuilds region
// writes from reachable nodes as well, so stale lexical effects cannot survive
// through region.writes even when a shared capture bypasses direct-use queries.
// No scanner field is needed; loopFlow needs effects *loopFlowEffects.
// index.locations supplies the original node's owner/loop context. Synthetic
// range targets are recovered from index.regions, without changing AST identity.
type loopFlowEffects struct {
	s         *loopScanner
	aliases   map[*loopVariable]*loopVariable // loopFlow's private address-proof slots
	writes    map[*loopRegion]map[*loopVariable]bool
	actual    map[ast.Node][]loopDirectAccess
	ranges    map[ast.Node]loopLocation
	iteration map[ast.Node]*loopRegion
	direct    []loopDirectAccess
	seen      map[loopFlowEffectKey]int

	// Replay installs an occurrence-local sink. The source-indexed union is
	// still needed for abstract capture summaries, but is not a physical use.
	physical func(loopDirectAccess)
	states   map[ast.Node]*loopWalk // ordered operand environments in deferred replay

	// Execution summaries are context-local, independently of physical sites.
	// Inlined iterator bodies supply their real effects here; their abstract
	// summary-only edge remains solely in the source-global capture collector.
	collect      func(loopDirectAccess)
	invoke       func(*ast.CallExpr, *loopWalk) loopValue
	callee       func(ast.Expr) *ast.FuncLit
	lexical      map[ast.Node][]loopDirectAccess
	summaries    map[*loopConflictFrame]loopValue
	frameEffects map[*loopConflictFrame][]loopDirectAccess
	frame        *loopConflictFrame
	conflicts    *loopFlowConflicts
}

type loopFlowEffectKey struct {
	node     ast.Node
	variable *loopVariable
	owner    *ast.FuncLit
	pos      token.Pos
}

func (s *loopScanner) beginFlowEffects() *loopFlowEffects {
	e := &loopFlowEffects{
		s: s, writes: map[*loopRegion]map[*loopVariable]bool{},
		actual: map[ast.Node][]loopDirectAccess{}, ranges: map[ast.Node]loopLocation{},
		iteration: map[ast.Node]*loopRegion{}, seen: map[loopFlowEffectKey]int{},
		lexical: map[ast.Node][]loopDirectAccess{}, summaries: map[*loopConflictFrame]loopValue{},
		frameEffects: map[*loopConflictFrame][]loopDirectAccess{},
	}
	// A may-analysis replay must replace, not union with, lexical summaries.
	// Keep actual identifier accesses for the independently owned liveness pass.
	direct := s.index.direct[:0]
	for _, access := range s.index.direct {
		if access.effect {
			continue
		}
		direct = append(direct, access)
		e.lexical[access.node] = append(e.lexical[access.node], access)
		if access.mode&loopWrite != 0 {
			e.actual[access.node] = append(e.actual[access.node], access)
		}
	}
	s.index.direct = direct
	for _, l := range s.index.regions {
		if rng, ok := l.node.(*ast.RangeStmt); ok {
			var loops []*loopRegion
			for parent := l; parent != nil; parent = parent.parent {
				loops = append(loops, parent)
			}
			for i, j := 0, len(loops)-1; i < j; i, j = i+1, j-1 {
				loops[i], loops[j] = loops[j], loops[i]
			}
			for _, target := range []ast.Expr{rng.Key, rng.Value} {
				if target != nil {
					e.ranges[target] = loopLocation{owner: l.owner, loops: loops}
					if rng.Tok == token.DEFINE {
						e.iteration[target] = l
					}
				}
			}
		}
	}
	return e
}

func (e *loopFlowEffects) context(node ast.Node, w *loopWalk) loopWalk {
	next := *w // values are read-only throughout effect replay
	if loc, ok := e.ranges[node]; ok {
		next.owner, next.loops = loc.owner, loc.loops
	} else if loc, ok := e.s.index.locations[node]; ok {
		next.owner, next.loops = loc.owner, loc.loops
	}
	return next
}

func (e *loopFlowEffects) write(v *loopVariable, owner *ast.FuncLit, loops []*loopRegion) {
	for _, l := range loops {
		if l.owner != owner {
			continue
		}
		if e.writes[l] == nil {
			e.writes[l] = map[*loopVariable]bool{}
		}
		e.writes[l][v] = true
	}
}

func (e *loopFlowEffects) access(node ast.Node, v *loopVariable, mode loopAccess, pos token.Pos, w *loopWalk) {
	if e.frame != nil && e.frame.execution != nil {
		mode = e.frame.execution.referenceMode(v, mode)
	}
	access := loopDirectAccess{node: node, variable: v, owner: w.owner, mode: mode, pos: pos, effect: true}
	if e.collect != nil {
		e.collect(access)
	}
	if e.frame != nil {
		e.frameEffects[e.frame] = append(e.frameEffects[e.frame], access)
	}
	if e.physical != nil {
		e.physical(loopDirectAccess{node: node, variable: v, owner: w.owner, mode: mode, pos: pos, effect: true})
	}
	key := loopFlowEffectKey{node: node, variable: v, owner: w.owner, pos: pos}
	if i, ok := e.seen[key]; ok {
		e.direct[i].mode = loopEpochMergeMode(e.direct[i].mode, mode)
	} else {
		e.seen[key] = len(e.direct)
		e.direct = append(e.direct, loopDirectAccess{
			node: node, variable: v, owner: w.owner, mode: mode, pos: pos, effect: true,
		})
	}
	if mode&loopWrite != 0 {
		e.write(v, w.owner, w.loops)
	}
}

// addressAliases is the read-only counterpart of loopFlow.addressAliases.
// Ordinary retained values also contain callback captures and aggregate contents:
// none of those prove that the pointer addresses the captured local's storage.
// Do not allocate private slots during replay or infer aliases through heap fields.
func (e *loopFlowEffects) addressAliases(expr ast.Expr, w *loopWalk) loopValue {
	switch expr := loopUnparen(expr).(type) {
	case *ast.StarExpr:
		return loopLoadedAddressAliases(e.addressAliases(expr.X, w), w, e.aliases)
	case *ast.Ident:
		if slot := e.aliases[e.s.index.ids[expr]]; slot != nil {
			return w.values[slot]
		}
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			if star, ok := loopUnparen(expr.X).(*ast.StarExpr); ok {
				return e.addressAliases(star.X, w)
			}
			if roots, handled := e.s.promotedFieldAddressRoots(expr.X, w); handled {
				return roots
			}
			return loopAddressRoot(e.s, expr.X, w)
		}
	case *ast.CallExpr:
		if len(expr.Args) == 1 && e.s.conversion(expr.Fun) {
			return e.addressAliases(expr.Args[0], w)
		}
	}
	return nil
}

// indirectAccess records the data access, separately from evaluating its
// address. A slice/map element is not stored in its header variable; an array
// element or struct field can be stored in an explicitly addressed local.
func (e *loopFlowEffects) indirectAccess(expr ast.Expr, mode loopAccess, w *loopWalk, pos token.Pos) {
	var pointer ast.Expr
	var base ast.Expr
	switch expr := loopUnparen(expr).(type) {
	case *ast.StarExpr:
		pointer = expr.X
	case *ast.SelectorExpr:
		if hint := e.s.fieldReceiverHint(expr); hint.field && hint.throughPointer {
			for v, targetMode := range e.fieldPathTargets(expr, hint, w) {
				e.access(expr, v, mode|targetMode&(loopPriorRef|loopEpochMixedRef), pos, w)
			}
			return
		}
		if e.s.pointerValue(expr.X) {
			pointer = expr.X
		} else {
			base = expr.X
		}
	case *ast.IndexExpr:
		if e.s.pointerValue(expr.X) {
			pointer = expr.X
		} else if !e.s.referenceIndex(expr.X) {
			base = expr.X
		}
	}
	if pointer != nil {
		for v, targetMode := range e.addressAliases(pointer, w) {
			// Preserve reference provenance, not the alias summary's executable
			// modes. In particular a read-only alias can still be written through.
			e.access(loopUnparen(expr), v, mode|targetMode&(loopPriorRef|loopEpochMixedRef), pos, w)
		}
	} else if base != nil {
		e.indirectAccess(base, mode, w, pos)
	}
}

// addressOperands evaluates only what is needed to locate storage. &v and
// &p.N do not load the addressed data; &(*pp).N still loads the pointer *pp.
func (e *loopFlowEffects) addressOperands(expr ast.Expr, w *loopWalk) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		// Keep lexical writes/implicit range definitions in region summaries;
		// recording an identifier never synthesizes a pointee read.
		e.recordFlowEffects(expr, w)
	case *ast.StarExpr:
		e.recordFlowEffects(expr.X, w)
	case *ast.SelectorExpr:
		if hint := e.s.fieldReceiverHint(expr); hint.field && hint.throughPointer {
			e.recordFlowEffects(expr.X, w)
			base := expr.X
			// Locating the final field does not read the field's storage.
			for _, field := range hint.path[:len(hint.path)-1] {
				if field.indirect {
					e.methodPathPointeeRead(expr, base, loopHeaderRead, w)
				}
				base = loopReceiverFieldExpr(base, field, expr.Sel.Pos())
			}
		} else if e.s.pointerValue(expr.X) {
			e.recordFlowEffects(expr.X, w)
		} else {
			e.addressOperands(expr.X, w)
		}
	case *ast.IndexExpr:
		if e.s.referenceIndex(expr.X) {
			e.recordFlowEffects(expr.X, w)
		} else {
			e.addressOperands(expr.X, w)
		}
		e.recordFlowEffects(expr.Index, w)
	default:
		e.recordFlowEffects(expr, w)
	}
}

func (e *loopFlowEffects) fieldRead(expr *ast.SelectorExpr) bool {
	if info := e.s.env.ctx.types; info != nil {
		if selection := info.Selections[expr]; selection != nil {
			return selection.Kind() == types.FieldVal
		}
	}
	if e.s.fieldReceiverHint(expr).field {
		return true
	}
	// SkipObjectResolution leaves references to file-level named structs
	// without Obj links. Recover only their field shape, not heap aliases or
	// method bodies; selecting a pointer-receiver method does not load data.
	typ := e.s.syntaxType(expr.X)
	seen := map[ast.Expr]bool{}
	for typ != nil && !seen[typ] {
		seen[typ] = true
		typ = loopUnparen(typ)
		if pointer, ok := typ.(*ast.StarExpr); ok {
			typ = loopUnparen(pointer.X)
		}
		if _, _, ok := e.s.fieldSyntax(typ, expr.Sel.Name); ok {
			return true
		}
		id, named := typ.(*ast.Ident)
		if !named || e.s.env.ctx.file == nil {
			break
		}
		var declared ast.Expr
		for _, decl := range e.s.env.ctx.file.Decls {
			if group, ok := decl.(*ast.GenDecl); ok && group.Tok == token.TYPE {
				for _, spec := range group.Specs {
					if spec := spec.(*ast.TypeSpec); spec.Name.Name == id.Name {
						declared = spec.Type
					}
				}
			}
		}
		typ = declared
	}
	return false
}

func (e *loopFlowEffects) recordFlowEffects(node ast.Node, w *loopWalk) {
	if e == nil || node == nil {
		return
	}
	if e.states != nil {
		state := e.states[node]
		if state == nil {
			return // not visited on any actual expression-execution edge
		}
		w = state
	}
	next := e.context(node, w)
	w = &next
	// Literal construction never executes its body in the constructing frame.
	if _, literal := node.(*ast.FuncLit); literal {
		return
	}
	for _, access := range e.actual[node] {
		e.write(access.variable, access.owner, w.loops)
	}
	if e.collect != nil {
		for _, access := range e.lexical[node] {
			mode := access.mode
			if e.frame != nil && e.frame.execution != nil {
				mode = e.frame.execution.referenceMode(access.variable, mode)
			}
			value := loopValue{access.variable: mode}
			// Known callee execution comes from its actual context below,
			// never from the immutable slot's source-global capture union.
			if expr, ok := node.(ast.Expr); !ok || e.callee == nil || e.callee(expr) == nil {
				value = e.s.expand(value, w)
			}
			for v, mode := range value {
				e.collect(loopDirectAccess{variable: v, mode: mode})
			}
		}
	}
	// := range definitions have implicit writes but no lexical identifier
	// accesses. The adapter puts the original targets on reachable body entries.
	if l := e.iteration[node]; l != nil {
		if id, ok := node.(*ast.Ident); ok {
			if v := e.s.index.ids[id]; v != nil {
				e.write(v, l.owner, []*loopRegion{l})
			}
		}
	}
	switch n := node.(type) {
	case *ast.GoStmt:
		e.operands(n.Call, w)
		return
	case *ast.DeferStmt:
		e.operands(n.Call, w)
		return
	case *ast.ReturnStmt:
		for _, result := range n.Results {
			e.recordFlowEffects(result, w)
		}
		// Explicit return expressions assign named results before returning.
		// Bare returns only read them. Synthetic void returns have no indexed
		// owner and must not inherit the root declaration's result list.
		if _, indexed := e.s.index.locations[n]; indexed {
			var results *ast.FieldList
			if w.owner != nil {
				results = w.owner.Type.Results
			} else if decl := e.s.env.ctx.decl; decl != nil {
				results = decl.Type.Results
			}
			if results != nil {
				mode := loopRead
				if len(n.Results) != 0 {
					mode |= loopWrite
				}
				for _, field := range results.List {
					for _, name := range field.Names {
						if variable := e.s.index.ids[name]; variable != nil {
							e.access(n, variable, mode, n.End(), w)
						}
					}
				}
			}
		}
	case *ast.CallExpr:
		if e.s.deferredUnevaluated(n) {
			return
		}
		value := loopValue(nil)
		if e.invoke != nil {
			value = e.invoke(n, w)
		}
		if value == nil {
			value = e.s.expand(e.s.invokeValue(n.Fun, w), w)
		}
		for v, mode := range value {
			e.access(n, v, mode, n.End(), w)
		}
	case *ast.AssignStmt:
		// All destination addresses use the pre-assignment bindings, including
		// parallel assignments such as p, *p = q, 1. transfer runs afterwards.
		// The range adapter's synthetic assignment has no RHS: ast.End
		// would panic. Its target positions precede the real loop body.
		var pos token.Pos
		if len(n.Rhs) != 0 {
			pos = n.End()
		} else if len(n.Lhs) != 0 {
			pos = n.Lhs[len(n.Lhs)-1].End()
		}
		mode := loopWrite
		if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
			mode |= loopRead // compound assignment loads the destination too
		}
		for _, lhs := range n.Lhs {
			context := e.context(lhs, w)
			e.indirectAccess(lhs, mode, &context, pos)
			e.addressOperands(lhs, &context)
		}
		for _, rhs := range n.Rhs {
			e.recordFlowEffects(rhs, w)
		}
		return
	case *ast.IncDecStmt:
		e.indirectAccess(n.X, loopRead|loopWrite, w, n.End())
		e.addressOperands(n.X, w)
		return
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			e.addressOperands(n.X, w)
			return
		}
	case *ast.StarExpr:
		e.indirectAccess(n, loopRead, w, n.End())
		e.addressOperands(n, w)
		return
	case *ast.SelectorExpr:
		if e.methodSelectionReads(n, w) {
			return
		}
		if e.fieldRead(n) {
			e.indirectAccess(n, loopRead, w, n.End())
			e.addressOperands(n, w)
			return
		}
	case *ast.IndexExpr:
		e.indirectAccess(n, loopRead, w, n.End())
		e.addressOperands(n, w)
		return
	}
	e.s.executionChildren(node, func(child ast.Node) { e.recordFlowEffects(child, w) })
}

// Method binding executes the promotion path now. Each pointer crossed to
// fetch another embedded field is a data load, even for a pointer receiver;
// only loading the final receiver's pointee is deferred until invocation.
// Explicit local address proofs are the only source of concrete pointee roots.
func (e *loopFlowEffects) methodSelectionReads(expr *ast.SelectorExpr, w *loopWalk) bool {
	hint := e.s.methodReceiverHint(expr)
	if !hint.known {
		return false
	}
	if e.s.receiverImplicitAddress(expr) {
		e.addressOperands(expr.X, w)
	} else {
		e.recordFlowEffects(expr.X, w)
	}
	base := expr.X
	for _, field := range hint.path {
		if field.indirect {
			e.methodPathPointeeRead(expr, base, loopHeaderRead, w)
		}
		base = loopReceiverFieldExpr(base, field, expr.Sel.Pos())
	}
	if !hint.pointerReceiver && hint.receiverPointer {
		e.methodPathPointeeRead(expr, base, loopRead, w)
	}
	return true
}

func (e *loopFlowEffects) methodPathPointeeRead(node *ast.SelectorExpr, pointer ast.Expr, mode loopAccess, w *loopWalk) {
	for v, targetMode := range e.addressAliases(pointer, w) {
		e.access(node, v, mode|targetMode&(loopPriorRef|loopEpochMixedRef), node.End(), w)
	}
}

func (s *loopScanner) iteratorCallable(expr ast.Expr) bool {
	if info := s.env.ctx.types; info != nil && info.TypeOf(expr) != nil {
		_, function := info.TypeOf(expr).Underlying().(*types.Signature)
		return function
	}
	_, function := s.syntaxType(expr).(*ast.FuncType)
	return function
}

// Inlining changes physical ordering, but not the fact that the enclosing
// frame executes the iterator. Keep a source-global summary-only execution
// edge so opaque literal callers retain those effects without creating a
// fictitious pre-yield access. Known execution contexts collect the inlined
// body's actual effects instead of reimporting this abstract union.
func (e *loopFlowEffects) iteratorSummary(expr ast.Expr, w *loopWalk) {
	if e == nil {
		return
	}
	next := e.context(expr, w)
	for v, mode := range e.s.expand(e.s.invokeValue(expr, &next), &next) {
		e.access(expr, v, mode, expr.End(), &next)
		key := loopFlowEffectKey{node: expr, variable: v, owner: next.owner, pos: expr.End()}
		e.direct[e.seen[key]].summaryOnly = true
		if e.frame != nil {
			accesses := e.frameEffects[e.frame]
			accesses[len(accesses)-1].summaryOnly = true
		}
	}
}

// A function-valued range expression is invoked synchronously by the language,
// even though its AST has no CallExpr. Summarize that call in the caller frame.
func (e *loopFlowEffects) rangeInvocation(expr ast.Expr, w *loopWalk) {
	if e == nil {
		return
	}
	if !e.s.iteratorCallable(expr) {
		return
	}
	next := e.context(expr, w)
	for v, mode := range e.s.expand(e.s.invokeValue(expr, &next), &next) {
		e.access(expr, v, mode, expr.End(), &next)
	}
}

func (e *loopFlowEffects) operands(call *ast.CallExpr, w *loopWalk) {
	// Only the outer go/defer call is asynchronous/delayed. Calls evaluating
	// its function expression and arguments still execute synchronously.
	e.s.executionOperands(call, func(node ast.Node) { e.recordFlowEffects(node, w) })
}

func (e *loopFlowEffects) finishFlowEffects() {
	if e.conflicts != nil {
		// Keep source-indexed summaries only for active execution contexts.
		// Physical consumers still use frame.effects, never this union.
		e.direct = nil
		e.seen = map[loopFlowEffectKey]int{}
		for _, frame := range e.conflicts.activeContextFrames() {
			for _, access := range e.frameEffects[frame] {
				key := loopFlowEffectKey{node: access.node, variable: access.variable, owner: access.owner, pos: access.pos}
				if i, ok := e.seen[key]; ok {
					e.direct[i].mode = loopEpochMergeMode(e.direct[i].mode, access.mode)
				} else {
					e.seen[key] = len(e.direct)
					e.direct = append(e.direct, access)
				}
			}
		}
	}
	for _, l := range e.s.index.regions {
		l.writes = e.writes[l]
		if l.writes == nil {
			l.writes = map[*loopVariable]bool{}
		}
	}
	e.s.index.direct = append(e.s.index.direct, e.direct...)
}
