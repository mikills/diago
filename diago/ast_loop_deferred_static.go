package diago

import (
	"go/ast"

	"golang.org/x/tools/go/cfg"
)

// Program-local facts, not an alternate deferred interpreter. An operand may
// leave the binding lattice only when its value cannot vary during this owner
// invocation. Dynamic function/argument values and allocation epochs still use
// the normal saved slots. Each registration keeps its original activation token.
type loopDeferredStatic struct {
	previous *loopVariable
	literal  *ast.FuncLit
}

// factorOperands removes invariant operands from CFG input maps. In particular,
// straight-line zero-argument literals need only the owner's top token, rather
// than a growing table copied at every block. The tiny constant-top analysis
// deliberately gives up on disagreeing predecessors; it never picks one path.
func (p *loopDeferredPlan) factorOperands(graph *cfg.CFG) {
	p.static = map[*loopDeferredSlot]loopDeferredStatic{}
	p.persistent = true
	p.factorLiterals(graph, p.graphOwners(graph))

	// nil is unreachable/bottom; unknown is the join of different tops. Tops
	// describe this owner only: inlined iterator owners have independent stacks.
	unknown := &loopVariable{}
	p.factorPrevious(graph, p.topInputs(graph, unknown), unknown)
}

// graphOwners is this plan's owner plus every marker owner inlined in graph.
func (p *loopDeferredPlan) graphOwners(graph *cfg.CFG) map[*ast.FuncLit]bool {
	owners := map[*ast.FuncLit]bool{p.owner: true}
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			if marker, ok := p.flow.markers[node]; ok {
				owners[marker.owner] = true
			}
		}
	}
	return owners
}

// factorLiterals records zero-argument literal callees whose captured cells
// cannot age before exit.
func (p *loopDeferredPlan) factorLiterals(graph *cfg.CFG, owners map[*ast.FuncLit]bool) {
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			stmt, ok := node.(*ast.DeferStmt)
			if !ok {
				continue
			}
			s := p.flow.slot(stmt)
			lit, ok := loopUnparen(stmt.Call.Fun).(*ast.FuncLit)
			if !ok || len(stmt.Call.Args) != 0 {
				continue
			}
			if p.stableCaptures(lit, owners) {
				p.static[s] = loopDeferredStatic{literal: lit}
			}
		}
	}
}

// stableCaptures reports whether lit captures no cell born in owners.
func (p *loopDeferredPlan) stableCaptures(lit *ast.FuncLit, owners map[*ast.FuncLit]bool) bool {
	for v := range p.flow.s.index.captures[lit] {
		// A captured cell born in this CFG can age before exit. Cells
		// born outside it cannot: their saved reference mode is the
		// owner's execution descriptor, applied at materialization.
		if v.birth != nil && owners[v.birth.owner] {
			return false
		}
	}
	return true
}

// topInputs solves the constant-top analysis, giving each block's input top.
func (p *loopDeferredPlan) topInputs(graph *cfg.CFG, unknown *loopVariable) []*loopVariable {
	inputs := make([]*loopVariable, len(graph.Blocks))
	inputs[0] = p.flow.owner(p.owner).bottom
	queue := []*cfg.Block{graph.Blocks[0]}
	queued := make([]bool, len(graph.Blocks))
	queued[0] = true
	for head := 0; head < len(queue); head++ {
		block := queue[head]
		queued[block.Index] = false
		top := p.transferTop(block, inputs[block.Index], nil)
		for _, next := range block.Succs {
			before := inputs[next.Index]
			after := loopDeferredJoinTop(before, top, unknown)
			if before != after {
				inputs[next.Index] = after
				if !queued[next.Index] {
					queue = append(queue, next)
					queued[next.Index] = true
				}
			}
		}
	}
	return inputs
}

// transferTop advances this owner's top across block, calling visit (if
// non-nil) with the top each of the owner's registrations pushes onto.
func (p *loopDeferredPlan) transferTop(block *cfg.Block, top *loopVariable, visit func(*loopDeferredSlot, *loopVariable)) *loopVariable {
	for _, node := range block.Nodes {
		if marker, ok := p.flow.markers[node]; ok && marker.entry && marker.owner == p.owner {
			top = p.flow.owner(p.owner).bottom
		}
		if stmt, ok := node.(*ast.DeferStmt); ok {
			s := p.flow.slot(stmt)
			if s.owner == p.owner {
				if visit != nil {
					visit(s, top)
				}
				top = s.token
			}
		}
		if exit, ok := p.exits[node]; ok && exit.owner == p.owner {
			top = p.flow.owner(p.owner).bottom
		}
	}
	return top
}

// loopDeferredJoinTop keeps a top only while every visited path agrees on it.
func loopDeferredJoinTop(before, top, unknown *loopVariable) *loopVariable {
	if before != nil && before != top {
		return unknown
	}
	return top
}

// factorPrevious records each registration's predecessor top when it is
// constant.
func (p *loopDeferredPlan) factorPrevious(graph *cfg.CFG, inputs []*loopVariable, unknown *loopVariable) {
	// One source registration can have several adapted occurrences. A fact
	// must hold at ALL reachable occurrences, not just the last visited clone.
	previous := map[*loopDeferredSlot]*loopVariable{}
	for _, block := range graph.Blocks {
		if inputs[block.Index] == nil {
			continue
		}
		p.transferTop(block, inputs[block.Index], func(s *loopDeferredSlot, top *loopVariable) {
			previous[s] = loopDeferredJoinTop(previous[s], top, unknown)
		})
	}
	for s, token := range previous {
		if token != unknown {
			fact := p.static[s]
			fact.previous = token
			p.static[s] = fact
		}
	}
}

func (p *loopDeferredPlan) storage(s *loopDeferredSlot) []*loopVariable {
	fact := p.static[s]
	if fact.previous == nil && fact.literal == nil {
		return s.storage()
	}
	var out []*loopVariable
	if fact.previous == nil {
		out = append(out, s.previous)
	}
	if fact.literal == nil {
		out = append(out, s.callee, s.origins)
	}
	out = append(out, s.args...)
	out = append(out, s.addresses...)
	out = append(out, s.argOrigins...)
	return out
}

func (p *loopDeferredPlan) previous(s *loopDeferredSlot, state map[*loopVariable]loopValue) loopValue {
	if token := p.static[s].previous; token != nil {
		return loopValue{token: loopRetain}
	}
	return state[s.previous]
}

// Materialize once per exit, never once per event or CFG input. The shared plan
// contains syntax facts only; prior/mixed references belong to this invocation.
func (p *loopDeferredPlan) savedOperands(w *loopWalk, owner *loopDeferredOwner) map[*loopVariable]loopValue {
	if len(p.static) == 0 && !w.persistentDeferred {
		return w.values
	}
	saved := loopCloneValues(w.values)
	// Do not materialize enclosing owners' pending stacks at iterator exits.
	// Their immutable tree remains shared until their own exit/entry.
	for _, s := range owner.slots {
		if w.persistentDeferred {
			for _, slot := range p.storage(s) {
				if value := p.flow.savedValue(w, slot); len(value) != 0 {
					saved[slot] = value
				}
			}
		}
		if fact := p.static[s]; fact.literal != nil {
			saved[s.callee] = p.flow.s.value(fact.literal, w)
			saved[s.origins] = loopValue{p.flow.literal(fact.literal): loopRetain}
		}
	}
	return saved
}
