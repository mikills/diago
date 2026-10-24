package diago

import (
	"go/ast"
	"go/token"
	"go/types"
)

// Private slots live in the same lattice as ordinary bindings, but never occur
// in launch values. This gives range snapshots and local address aliases the
// same reachability, joins, strong overwrites, and nested-frame seeds as values.
// In particular, a closure's captured variables are NOT pointer alias evidence.
type loopFlow struct {
	s          *loopScanner
	aliases    map[*loopVariable]*loopVariable
	effects    *loopFlowEffects
	conflicts  *loopFlowConflicts
	deferred   *loopDeferredFlow
	prepared   map[*ast.BlockStmt]*loopDeferredCFG
	simulated  map[*ast.BlockStmt]map[uint64][]*loopDeferredSimulation
	simulating *loopDeferredSimulation
}

type loopFlowRange struct {
	stmt       *ast.RangeStmt // original syntax, including type/owner identities
	entry      ast.Expr       // private evaluation identity for an adapted occurrence
	iterator   *ast.FuncLit
	key, value *loopVariable
}

func (f *loopFlow) aliasSlot(v *loopVariable) *loopVariable {
	if f.aliases[v] == nil {
		f.aliases[v] = &loopVariable{}
	}
	return f.aliases[v]
}

// Private address metadata: a collapsed subobject/fallback root cannot prove a
// whole-cell destination, even when its type happens to match the stored value.
const loopAddressPartial loopAccess = 1 << 18

func loopAddressRoot(s *loopScanner, expr ast.Expr, w *loopWalk) loopValue {
	if roots, handled := s.promotedFieldAddressRoots(expr, w); handled {
		return roots
	}
	if v := s.root(expr); v != nil {
		mode := w.referenceMode(v, loopRead)
		if _, whole := loopUnparen(expr).(*ast.Ident); !whole {
			mode |= loopAddressPartial
		}
		return loopValue{v: mode}
	}
	return nil
}

func loopPartialAddress(value loopValue) loopValue {
	out := loopValue{}
	for v, mode := range value {
		out[v] = mode | loopAddressPartial
	}
	return out
}

func (f *loopFlow) addressAliases(e ast.Expr, w *loopWalk) loopValue {
	switch e := loopUnparen(e).(type) {
	case *ast.StarExpr:
		return loopLoadedAddressAliases(f.addressAliases(e.X, w), w, f.aliases)
	case *ast.Ident:
		if v := f.s.index.ids[e]; v != nil {
			return w.values[f.aliasSlot(v)]
		}
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			if star, ok := loopUnparen(e.X).(*ast.StarExpr); ok {
				return f.addressAliases(star.X, w)
			}
			return loopAddressRoot(f.s, e.X, w)
		}
	case *ast.CallExpr:
		if len(e.Args) == 1 && f.s.conversion(e.Fun) {
			return f.addressAliases(e.Args[0], w)
		}
	}
	return nil
}

// Aggregate bindings deliberately have sparse, field/index-insensitive summaries.
// Stores weakly add retained contents; a whole-variable assignment still kills
// the summary. Known &local aliases update that local, not the pointer slot.
// Unknown pointees fall back to the pointer's own summary. No call-result alias
// inference or general heap/object identity is attempted here.
func (f *loopFlow) storeRoots(e ast.Expr, w *loopWalk) loopValue {
	switch e := loopUnparen(e).(type) {
	case *ast.Ident:
		if v := f.s.index.ids[e]; v != nil {
			return loopValue{v: w.referenceMode(v, loopRead)}
		}
	case *ast.SelectorExpr:
		if targets := f.addressAliases(e.X, w); len(targets) != 0 {
			return loopPartialAddress(targets)
		}
		return loopPartialAddress(f.storeRoots(e.X, w))
	case *ast.IndexExpr:
		if targets := f.addressAliases(e.X, w); len(targets) != 0 {
			return loopPartialAddress(targets)
		}
		return loopPartialAddress(f.storeRoots(e.X, w))
	case *ast.StarExpr:
		// The destination is the cell addressed by X, not the pointer value
		// loaded from that cell by evaluating the entire StarExpr.
		if targets := f.addressAliases(e.X, w); len(targets) != 0 {
			return targets
		}
		return loopPartialAddress(f.storeRoots(e.X, w))
	}
	return nil
}

func loopFlowWeakStore(roots, value loopValue, w *loopWalk) {
	if len(value) == 0 {
		return
	}
	for root, mode := range roots {
		if loopEpochCurrentMode(mode) == 0 {
			continue // a retained old address cannot overwrite the new allocation
		}
		merged := loopValue{}
		loopMerge(merged, w.values[root])
		loopMerge(merged, value)
		w.values[root] = merged // never mutate a shared snapshot
	}
}

func (f *loopFlow) assign(lhs ast.Expr, value, aliases, roots, keys loopValue, w *loopWalk) {
	if id, ok := loopUnparen(lhs).(*ast.Ident); ok {
		if v := f.s.index.ids[id]; v != nil {
			w.values[v] = value
			w.values[f.aliasSlot(v)] = aliases
		}
	} else {
		targets := loopWholeCellStoreTargets(f.s, lhs, roots)
		for target, mode := range targets {
			loopStoreCallableBinding(target, mode, len(roots) == 1, value, w)
			loopStoreCallableBinding(f.aliasSlot(target), mode, len(roots) == 1, aliases, w)
		}
		if f.s.retains(lhs) {
			partial := loopValue{}
			for root, mode := range roots {
				if _, whole := targets[root]; !whole {
					partial[root] = mode
				}
			}
			loopFlowWeakStore(partial, value, w)
		}
		// A map owns its keys as well as its elements. Scalar element stores
		// still retain pointer-bearing keys, unlike array/slice indexes.
		loopFlowWeakStore(roots, keys, w)
	}
}

func (f *loopFlow) storeKeys(lhs ast.Expr, w *loopWalk) loopValue {
	if index, ok := loopUnparen(lhs).(*ast.IndexExpr); ok && f.mapIndex(index) {
		return f.s.value(index.Index, w)
	}
	return nil
}

func (f *loopFlow) mapIndex(index *ast.IndexExpr) bool {
	if info := f.s.env.ctx.types; info != nil {
		if typ := info.TypeOf(index.X); typ != nil {
			_, ok := typ.Underlying().(*types.Map)
			return ok
		}
	}
	_, ok := f.rangeSyntax(index.X, map[*loopVariable]bool{}).(*ast.MapType)
	return ok
}

// transfer intentionally does not call walk: that would duplicate launches and
// seed loops and literal frames from lexical rather than reachable bindings.
func (f *loopFlow) transfer(node ast.Node, w *loopWalk, a *loopFlowAST) {
	s := f.s
	if rng := a.entries[node]; rng != nil {
		w.values[rng.key], w.values[rng.value] = f.rangeValues(rng.stmt.X, w)
		return
	}
	if rng := a.steps[node]; rng != nil {
		lhs := []ast.Expr{rng.stmt.Key, rng.stmt.Value}
		values := []loopValue{w.values[rng.key], w.values[rng.value]}
		// Iterator yield nodes snapshot all three lattices with ordinary
		// assignments. Body entry must copy their metadata as well as values.
		var addresses, origins [2]loopValue
		if rng.iterator != nil {
			addresses = [2]loopValue{w.values[f.aliasSlot(rng.key)], w.values[f.aliasSlot(rng.value)]}
		}
		if f.deferred != nil && rng.iterator != nil {
			origins = [2]loopValue{w.values[f.deferred.companion(rng.key)], w.values[f.deferred.companion(rng.value)]}
		}
		roots := []loopValue{f.storeRoots(lhs[0], w), f.storeRoots(lhs[1], w)}
		keys := []loopValue{f.storeKeys(lhs[0], w), f.storeKeys(lhs[1], w)}
		for i, expr := range lhs {
			if expr != nil {
				f.assign(expr, values[i], addresses[i], roots[i], keys[i], w)
				if f.deferred != nil && rng.iterator != nil {
					f.deferred.assignOrigins(expr, origins[i], roots[i], w)
				}
			}
		}
		return
	}
	switch n := node.(type) {
	case *ast.AssignStmt:
		values, aliases := make([]loopValue, len(n.Lhs)), make([]loopValue, len(n.Lhs))
		roots, keys := make([]loopValue, len(n.Lhs)), make([]loopValue, len(n.Lhs))
		// Evaluate RHSs and destination addresses before any writes (swaps and
		// assignments such as p, p.F = q, f must use the old p for the store).
		for i, lhs := range n.Lhs {
			roots[i], keys[i] = f.deferredDestination(lhs, w)
			if i < len(n.Rhs) {
				value := f.deferredExpression(n.Rhs[i], w)
				values[i], aliases[i] = value.value, value.address
			}
		}
		for i, lhs := range n.Lhs {
			f.assign(lhs, values[i], aliases[i], roots[i], keys[i], w)
		}
	case *ast.ValueSpec:
		values, aliases := make([]loopValue, len(n.Names)), make([]loopValue, len(n.Names))
		for i := range n.Names {
			if i < len(n.Values) {
				value := f.deferredExpression(n.Values[i], w)
				values[i], aliases[i] = value.value, value.address
			}
		}
		for i, id := range n.Names {
			f.assign(id, values[i], aliases[i], nil, nil, w)
		}
	case *ast.SendStmt:
		// Locally visible sends supply a conservative channel element summary
		// for subsequent range receives. Calls/external senders remain opaque.
		if s.retains(n.Value) {
			loopFlowWeakStore(f.storeRoots(n.Chan, w), f.deferredExpression(n.Value, w).value, w)
		}
	}
}

// Only destinations evaluated in this statement use the emission cache. Range
// entry/steps and ordinary CFG transfer retain their existing binding rules.
func (f *loopFlow) deferredDestination(expr ast.Expr, w *loopWalk) (loopValue, loopValue) {
	if saved := w.operands[nil]; saved != nil {
		if value, ok := saved.expressions[expr]; ok && value.destination {
			return value.roots, value.keys
		}
	}
	return f.storeRoots(expr, w), f.storeKeys(expr, w)
}

// Origins must consume the same snapshots as values/address proofs. All RHSs
// and destination addresses are collected before any companion slot is written.
func (f *loopFlow) transferDeferredOrigins(node ast.Node, w *loopWalk) {
	if w.operands[nil] == nil {
		f.deferred.transfer(node, w)
		return
	}
	var lhs, rhs []ast.Expr
	switch n := node.(type) {
	case *ast.AssignStmt:
		lhs, rhs = n.Lhs, n.Rhs
	case *ast.ValueSpec:
		for _, name := range n.Names {
			lhs = append(lhs, name)
		}
		rhs = n.Values
	default:
		return
	}
	values, proofs := make([]loopValue, len(lhs)), make([]loopValue, len(lhs))
	for i, expr := range lhs {
		if i < len(rhs) {
			values[i] = f.deferredExpression(rhs[i], w).origins
		}
		proofs[i], _ = f.deferredDestination(expr, w)
	}
	for i, expr := range lhs {
		f.deferred.assignOrigins(expr, values[i], proofs[i], w)
	}
}

// loopWholeCellStoreTargets accepts only local whole cells reached through
// address proof, never collapsed field/index roots or unknown-pointee fallbacks.
func loopWholeCellStoreTargets(s *loopScanner, lhs ast.Expr, proofs loopValue) loopValue {
	if _, ok := loopUnparen(lhs).(*ast.StarExpr); !ok {
		return nil
	}
	var targets loopValue
	for target, mode := range proofs {
		if target.ident == nil || s.index.external[target.ident.Name] == target || mode&loopAddressPartial != 0 {
			continue
		}
		if targets == nil {
			targets = loopValue{}
		}
		targets[target] = mode
	}
	return targets
}

// A single current cell permits a strong overwrite (including empty values).
// Mixed references and multiple targets remain weak; prior-only references must
// not change the current allocation. Always copy before joining shared values.
func loopStoreCallableBinding(slot *loopVariable, mode loopAccess, single bool, value loopValue, w *loopWalk) {
	if loopEpochCurrentMode(mode) == 0 {
		return
	}
	if single && mode&loopPriorRef == 0 {
		w.values[slot] = value
		return
	}
	out := loopValue{}
	loopMerge(out, w.values[slot])
	loopMerge(out, value)
	w.values[slot] = out
}

// rangeShape distinguishes the first (key, except for channels) and second
// iteration values. Never copy collection retention into scalar indexes/keys.
func (f *loopFlow) rangeShape(x ast.Expr) (key, value, channel bool) {
	s := f.s
	if info := s.env.ctx.types; info != nil {
		if typ := info.TypeOf(x); typ != nil {
			typ = typ.Underlying()
			if p, ok := typ.(*types.Pointer); ok {
				typ = p.Elem().Underlying()
			}
			switch typ := typ.(type) {
			case *types.Map:
				return loopRetainedType(typ.Key()), loopRetainedType(typ.Elem()), false
			case *types.Array:
				return false, loopRetainedType(typ.Elem()), false
			case *types.Slice:
				return false, loopRetainedType(typ.Elem()), false
			case *types.Chan:
				return loopRetainedType(typ.Elem()), false, true
			case *types.Basic:
				return false, false, false
			}
		}
	}
	typ := f.rangeSyntax(x, map[*loopVariable]bool{})
	if p, ok := typ.(*ast.StarExpr); ok {
		typ = s.underlyingSyntax(p.X)
	}
	retains := func(typ ast.Expr) bool { return s.retainedSyntax(typ, map[ast.Expr]bool{}) }
	switch typ := typ.(type) {
	case *ast.MapType:
		return retains(typ.Key), retains(typ.Value), false
	case *ast.ArrayType:
		return false, retains(typ.Elt), false
	case *ast.ChanType:
		return retains(typ.Value), false, true
	case *ast.Ident:
		if !retains(typ) {
			return false, false, false
		}
	}
	// Unknown sources cannot justify treating an integer index as retained.
	return false, true, false
}

func (f *loopFlow) rangeValues(x ast.Expr, w *loopWalk) (loopValue, loopValue) {
	keyRetains, valueRetains, channel := f.rangeShape(x)
	key, value := loopValue{}, loopValue{}
	if !keyRetains && !valueRetains {
		return key, value
	}
	if lit := f.s.directLiteral(x); lit != nil && !channel {
		// Literal maps can separate key and value contents exactly. Array and
		// slice literal keys are indexes, not elements, even when explicitly keyed.
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if keyRetains {
					loopMerge(key, f.deferredExpression(kv.Key, w).value)
				}
				elt = kv.Value
			}
			if valueRetains {
				loopMerge(value, f.deferredExpression(elt, w).value)
			}
		}
		return key, value
	}
	contents := f.deferredExpression(x, w).value
	// A pointer-to-array range copies elements, not the address of the array
	// variable itself. Resolve locally known addresses to their current contents.
	if targets := f.addressAliases(x, w); len(targets) != 0 {
		contents = loopValue{}
		for target := range targets {
			loopMerge(contents, w.values[target])
		}
	}
	// Named aggregates deliberately use the coarse current binding summary.
	// Retention filtering above still keeps scalar map values/keys and indexes safe.
	if keyRetains {
		loopMerge(key, contents)
	}
	if valueRetains {
		loopMerge(value, contents)
	}
	return key, value
}
