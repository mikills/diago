package diago

import "go/ast"

// loopEpochMixedRef distinguishes a join containing BOTH current and prior
// references from an exclusively prior reference. loopPriorRef alone cannot do
// this: unioning {&v} and {previous} would otherwise silently lose one lifetime.
// Both bits are metadata, never callable execution modes (which start at bit 8).
const loopEpochMixedRef loopAccess = 1 << 6

// loopEpochMergeMode must replace plain mode ORs at value/event joins. In
// particular loopJoinBindings must compare this result, not use a subset test:
// an untagged current read joined into a tagged prior read adds information.
func loopEpochMergeMode(a, b loopAccess) loopAccess {
	out := a | b
	if out&loopPriorRef != 0 && (loopEpochCurrentMode(a) != 0 || loopEpochCurrentMode(b) != 0) {
		out |= loopEpochMixedRef
	}
	return out
}

func loopEpochCurrentMode(mode loopAccess) loopAccess {
	if mode&loopPriorRef != 0 && mode&loopEpochMixedRef == 0 {
		return 0
	}
	return mode &^ (loopPriorRef | loopEpochMixedRef)
}

func loopEpochPriorMode(mode loopAccess) loopAccess {
	if mode&loopPriorRef == 0 {
		return 0
	}
	return mode &^ (loopPriorRef | loopEpochMixedRef)
}

// loopEpochAgeValue ages references, not the referenced variable's contents.
// Existing current AND older references become prior when new storage is born.
// Copy on change is essential: predecessor, range-snapshot, closure, and launch
// values may all share this map. Latent callable bits remain untouched.
func loopEpochAgeValue(value loopValue, born map[*loopVariable]bool) loopValue {
	var out loopValue
	for v, mode := range value {
		if !born[v] || mode == 0 {
			continue
		}
		aged := mode&^loopEpochMixedRef | loopPriorRef
		if aged == mode {
			continue
		}
		if out == nil {
			out = make(loopValue, len(value))
			for target, access := range value {
				out[target] = access
			}
		}
		out[v] = aged
	}
	if out != nil {
		return out
	}
	return value
}

// advanceLoopEpoch is the pre-node hook for BOTH the fixed-point transfer and
// the post-convergence replay (before effects/launch operands are evaluated).
// The same birth index as the conflict graph must be used. The fresh range
// assignment and three-clause copy markers are not ordinary source declarations.
// New &v expressions evaluated after this hook remain current references.
func (f *loopFlow) advanceLoopEpoch(node ast.Node, w *loopWalk, a *loopFlowAST) {
	var births []*loopVariable
	if f.conflicts != nil {
		births = f.conflicts.births[node]
	}
	if a != nil {
		if region := a.copySteps[node]; region != nil {
			for v := range region.copies {
				births = append(births, v)
			}
		}
		if rng := a.steps[node]; rng != nil {
			region := f.s.index.regions[rng.stmt]
			for _, target := range []ast.Expr{rng.stmt.Key, rng.stmt.Value} {
				if id, ok := loopUnparen(target).(*ast.Ident); ok {
					if v := f.s.index.ids[id]; v != nil && region != nil && v.birth == region {
						births = append(births, v)
					}
				}
			}
		}
	}
	if len(births) == 0 {
		return
	}
	born := make(map[*loopVariable]bool, len(births))
	for _, v := range births {
		born[v] = true
	}
	// This includes private address slots in f.aliases. They are real address
	// provenance; captured mutable pointer slots are not themselves aliases.
	for binding, value := range w.values {
		w.values[binding] = loopEpochAgeValue(value, born)
	}
	w.deferredBindings = loopDeferredAge(w.deferredBindings, born, 28)
}

// loopEpochCurrentUses supplies the existing allocation-bounded conflict
// solver with ONLY current-storage accesses. Keep the original uses for
// priorConflicts below. Neither helper mutates shared recorder maps.
func loopEpochCurrentUses(uses *loopConflictVariable) *loopConflictVariable {
	hasPrior := false
	for _, modes := range []map[int]loopAccess{uses.direct, uses.launches} {
		for _, mode := range modes {
			hasPrior = hasPrior || mode&loopPriorRef != 0
		}
	}
	if !hasPrior {
		return uses // ordinary fresh-storage queries need no extra maps
	}
	out := &loopConflictVariable{
		direct: map[int]loopAccess{}, launches: map[int]loopAccess{}, stmts: uses.stmts,
	}
	for p, mode := range uses.direct {
		if current := loopEpochCurrentMode(mode); current != 0 {
			out.direct[p] = current
		}
	}
	for p, mode := range uses.launches {
		if current := loopEpochCurrentMode(mode); current != 0 {
			out.launches[p] = current
		}
	}
	return out
}

// loopEpochAcross is a two-state reachability query: an event is visible only
// on paths crossing at least one allocation boundary. Ordinary current direct
// accesses must NEVER be supplied here. The second state intentionally merges
// all older lifetimes: a conditional retention may keep an address indefinitely.
// Both solves are finite bit-vector worklists, not launch/event pair searches.
func (g *loopConflictFrame) loopEpochAcross(local []uint16, barrier []bool, forward bool) []uint16 {
	open := make([]bool, len(g.points))
	reachable := g.propagate(local, open, forward)
	seeds := make([]uint16, len(g.points))
	for p, boundary := range barrier {
		if boundary {
			seeds[p] = reachable[p]
		}
	}
	return g.propagate(seeds, open, forward)
}

// priorConflicts augments, but never disables, the ordinary fresh-storage
// barriers. Wire at the beginning of loopConflictFrame.conflicts:
//
//	g.priorConflicts(v, uses, out)
//	uses = loopEpochCurrentUses(uses)
//
// A current launch can meet an old-reference effect only AFTER a birth/copy.
// A prior launch can meet prior effects, or a current sibling BEFORE a birth.
// In particular an old-reference writer does NOT expose unrelated direct writes
// to the new v, and cannot race with a new-v worker which immediately exits.
// Allocation copies still read only the current slot in the ordinary solver.
//
// Limits: all older epochs of ONE indexed local share a may-alias bucket. This
// can conservatively join distinct retained generations, but never joins other
// local identities. There is no inference of heap/backing-store aliases and no
// snapshot of old aggregate contents. Prior-only references must not be expanded
// through w.values[v], or used to weak-store into v's new contents.
func (g *loopConflictFrame) priorConflicts(v *loopVariable, uses *loopConflictVariable, out *loopConflictHazards) {
	prior := false
	writer := false
	for _, modes := range []map[int]loopAccess{uses.direct, uses.launches} {
		for _, mode := range modes {
			prior = prior || mode&loopPriorRef != 0
			writer = writer || mode&(loopWrite|loopIndirectWrite) != 0
		}
	}
	if !prior || !writer {
		return
	}
	barrier := make([]bool, len(g.points))
	for _, points := range [][]int{g.births[v], g.copies[v]} {
		for _, p := range points {
			barrier[p] = true
		}
	}
	priorLocal := make([]uint16, len(g.points))
	priorLaunch := make([]uint16, len(g.points))
	currentLaunch := make([]uint16, len(g.points))
	for p, mode := range uses.direct {
		priorLocal[p] |= loopConflictMode(loopEpochPriorMode(mode), loopConflictDirectRead, loopConflictDirectWrite, loopConflictDirectDataRead, loopConflictDirectDataWrite)
	}
	for p, mode := range uses.launches {
		priorLaunch[p] = loopConflictMode(loopEpochPriorMode(mode), loopConflictLaunchRead, loopConflictLaunchWrite, loopConflictLaunchDataRead, loopConflictLaunchDataWrite)
		priorLocal[p] |= priorLaunch[p]
		currentLaunch[p] = loopConflictMode(loopEpochCurrentMode(mode), loopConflictLaunchRead, loopConflictLaunchWrite, loopConflictLaunchDataRead, loopConflictLaunchDataWrite)
	}
	open := make([]bool, len(g.points))
	futurePrior := g.propagate(priorLocal, open, false)
	pastPrior := g.propagate(priorLaunch, open, true)
	futureAcross := g.loopEpochAcross(priorLocal, barrier, false)
	pastAcross := g.loopEpochAcross(currentLaunch, barrier, true)
	if out.external == nil {
		out.external = map[*ast.GoStmt]map[*loopVariable]uint8{}
	}
	for p, mode := range uses.launches {
		var after, before uint16
		current, old := loopEpochCurrentMode(mode), loopEpochPriorMode(mode)
		if current != 0 {
			for _, next := range g.points[p].next {
				after |= futureAcross[next]
			}
		}
		if old != 0 {
			for _, next := range g.points[p].next {
				after |= futurePrior[next]
			}
			for _, prev := range g.points[p].prev {
				before |= pastPrior[prev] | pastAcross[prev]
			}
		}
		stmt := uses.stmts[p]
		direct := loopExternalConflicts(mode, uint8(after&loopConflictDirectMask))
		siblings := loopExternalConflicts(mode, uint8(((after|before)&loopConflictLaunchMask)>>4))
		external := direct | siblings
		if direct != 0 {
			out.direct[stmt] = true
		}
		if siblings != 0 {
			out.siblings[stmt] = true
		}
		if external != 0 {
			if out.external[stmt] == nil {
				out.external[stmt] = map[*loopVariable]uint8{}
			}
			out.external[stmt][v] |= external
		}
	}
}
