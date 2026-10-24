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
	owners := map[*ast.FuncLit]bool{p.owner: true}
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			if marker, ok := p.flow.markers[node]; ok {
				owners[marker.owner] = true
			}
		}
	}
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
			stable := true
			for v := range p.flow.s.index.captures[lit] {
				// A captured cell born in this CFG can age before exit. Cells
				// born outside it cannot: their saved reference mode is the
				// owner's execution descriptor, applied at materialization.
				if v.birth != nil && owners[v.birth.owner] {
					stable = false
					break
				}
			}
			if stable {
				p.static[s] = loopDeferredStatic{literal: lit}
			}
		}
	}

	// nil is unreachable/bottom; unknown is the join of different tops. Tops
	// describe this owner only: inlined iterator owners have independent stacks.
	unknown := &loopVariable{}
	inputs := make([]*loopVariable, len(graph.Blocks))
	inputs[0] = p.flow.owner(p.owner).bottom
	queue := []*cfg.Block{graph.Blocks[0]}
	queued := make([]bool, len(graph.Blocks))
	queued[0] = true
	transfer := func(block *cfg.Block, top *loopVariable, visit func(*loopDeferredSlot, *loopVariable)) *loopVariable {
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
	for head := 0; head < len(queue); head++ {
		block := queue[head]
		queued[block.Index] = false
		top := transfer(block, inputs[block.Index], nil)
		for _, next := range block.Succs {
			before := inputs[next.Index]
			after := top
			if before != nil && before != top {
				after = unknown
			}
			if before != after {
				inputs[next.Index] = after
				if !queued[next.Index] {
					queue = append(queue, next)
					queued[next.Index] = true
				}
			}
		}
	}
	// One source registration can have several adapted occurrences. A fact
	// must hold at ALL reachable occurrences, not just the last visited clone.
	previous := map[*loopDeferredSlot]*loopVariable{}
	for _, block := range graph.Blocks {
		if inputs[block.Index] == nil {
			continue
		}
		transfer(block, inputs[block.Index], func(s *loopDeferredSlot, top *loopVariable) {
			if before := previous[s]; before != nil && before != top {
				previous[s] = unknown
			} else {
				previous[s] = top
			}
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
