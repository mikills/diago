package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// loopFlowLiveness returns binding roots needed at each block's entry. Pass the
// adapter used to build graph: older adapters keep range snapshots in private
// slots rather than identifiers in index.ids. Ordinary CFG nodes keep their
// original identifiers, so identity (not spelling or source order) is sufficient.
//
// Binding liveness differs from ordinary variable liveness. If a live closure f
// can contain a reference to p, p's current binding is needed even when the only
// syntax at the launch is "go f()". Dependencies are closed statically across ALL
// paths, not just through a predecessor's current state: one branch can supply
// f's capture of p while another supplies p's value at the join.
func (s *loopScanner) loopFlowLiveness(graph *cfg.CFG, adapters ...*loopFlowAST) []map[*loopVariable]bool {
	return s.loopFlowLivenessSeeded(graph, nil, nil, adapters...)
}

// loopFlowLiveness additionally includes inherited binding/alias dependencies.
// Use this form in refine, including for nested frames: a seed may contain an
// outer closure or address whose construction is absent from this frame's CFG.
func (f *loopFlow) loopFlowLiveness(graph *cfg.CFG, adapter *loopFlowAST, seed map[*loopVariable]loopValue) []map[*loopVariable]bool {
	if adapter != nil && adapter.owner != nil && f.deferred != nil {
		// CFG templates are shared between contexts. Address parameters and
		// captured pointer bindings are invocation-specific: do not mutate
		// the template's lexical exports or contaminate constructor contexts.
		var exports map[*loopVariable]bool
		for v, mode := range f.deferredAddressExports(adapter.owner, seed) {
			if adapter.exports[v] || loopEpochCurrentMode(mode) == 0 {
				continue
			}
			if exports == nil {
				exports = make(map[*loopVariable]bool, len(adapter.exports)+3)
				for root := range adapter.exports {
					exports[root] = true
				}
			}
			exports[v], exports[f.aliasSlot(v)], exports[f.deferred.companion(v)] = true, true, true
		}
		if exports != nil {
			copy := *adapter
			copy.exports = exports
			adapter = &copy
		}
	}
	return f.s.loopFlowLivenessSeeded(graph, seed, f.aliases, adapter)
}

func (s *loopScanner) loopFlowLivenessSeeded(graph *cfg.CFG, seed map[*loopVariable]loopValue, aliases map[*loopVariable]*loopVariable, adapters ...*loopFlowAST) []map[*loopVariable]bool {
	lp := &loopFlowLivePass{
		s:          s,
		adapters:   adapters,
		w:          &loopWalk{values: map[*loopVariable]loopValue{}},
		staticFlow: &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}},
		sources:    map[*loopVariable]*loopVariable{},
		uses:       make([]map[*loopVariable]bool, len(graph.Blocks)),
		defs:       make([]map[*loopVariable]bool, len(graph.Blocks)),
		deps:       map[*loopVariable]map[*loopVariable]bool{},
		copies:     map[*loopVariable][]*loopVariable{},
	}
	for v, value := range seed {
		if len(value) != 0 {
			lp.deps[v] = map[*loopVariable]bool{}
			for dep := range value {
				lp.deps[v][dep] = true
			}
		}
	}
	inheritedAddresses := map[*loopVariable]loopValue{}
	for v, slot := range aliases {
		if len(seed[slot]) != 0 {
			inheritedAddresses[v] = seed[slot]
		}
	}
	// A store through p can change a's summary when p may alias &a. Include
	// these dependency owners statically too, even on a different join edge.
	lp.addresses = s.loopFlowLiveAddresses(graph, inheritedAddresses)

	// Evaluate retained values once with symbolic CONTENTS for each binding.
	// A copied identifier produces a symbol; &p and a closure capturing p
	// produce the real p. Only the latter retain p's mutable slot. Propagate
	// contents along copy edges before closing binding liveness: connecting
	// copies directly to their source slots retains an entire alias history.
	lp.seedSymbols(graph)
	lp.seedAddressSlots()
	for _, block := range graph.Blocks {
		lp.recordBlock(block)
	}

	loopFlowLivePropagate(lp.deps, lp.copies)
	// Callable origins, like address proofs, belong to a binding, not to its
	// copied contents. Install them before liveness closure, after propagating
	// semantic contents; otherwise an alias chain retains every historical slot.
	for _, a := range adapters {
		if a != nil && a.deferredPlan != nil {
			a.deferredPlan.flow.companionDependencies(lp.deps, lp.uses, lp.defs)
		}
	}
	// Alias metadata belongs to a live slot, not to its copied value. Add it
	// AFTER contents propagation so copying p does not retain p's private slot.
	for v, slot := range aliases {
		if len(seed[slot]) != 0 {
			if lp.deps[v] == nil {
				lp.deps[v] = map[*loopVariable]bool{}
			}
			lp.deps[v][slot] = true
		}
	}
	return lp.solve(graph)
}

// loopFlowLivePass holds the static equations loopFlowLivenessSeeded builds
// for one CFG: per-block uses and strong definitions, plus the dependency and
// copy edges between bindings, evaluated over symbolic binding contents.
type loopFlowLivePass struct {
	s          *loopScanner
	adapters   []*loopFlowAST
	addresses  map[*loopVariable]map[*loopVariable]bool
	w          *loopWalk
	staticFlow *loopFlow
	sources    map[*loopVariable]*loopVariable // symbol marker -> binding
	uses, defs []map[*loopVariable]bool
	deps       map[*loopVariable]map[*loopVariable]bool
	copies     map[*loopVariable][]*loopVariable
}

func (lp *loopFlowLivePass) symbol(v *loopVariable) {
	if v != nil && lp.w.values[v] == nil {
		marker := &loopVariable{}
		lp.w.values[v] = loopValue{marker: loopRead}
		lp.sources[marker] = v
	}
}

// Symbols belong to this CFG, not every nested function in the source.
// Seeding all indexed identifiers for every deferred context allocates a
// quadratic number of unrelated symbols (notably with named parameters).
func (lp *loopFlowLivePass) seedSymbols(graph *cfg.CFG) {
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			ast.Inspect(node, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					return false
				case *ast.Ident:
					lp.symbol(lp.s.index.ids[n])
				}
				return true
			})
		}
	}
	for _, a := range lp.adapters {
		if a != nil {
			for _, rng := range a.entries {
				lp.symbol(rng.key)
				lp.symbol(rng.value)
			}
		}
	}
}

// Range pointer-to-array snapshots copy pointee contents, not the
// address itself. Give rangeValues the static may-address metadata.
func (lp *loopFlowLivePass) seedAddressSlots() {
	lp.w.aliases = lp.staticFlow.aliases
	for v, targets := range lp.addresses {
		value := loopValue{}
		for target := range targets {
			lp.symbol(target)
			value[target] = loopRead
		}
		lp.w.values[lp.staticFlow.aliasSlot(v)] = value
	}
}

// loopFlowLiveNode is one CFG node's bindings: uses before simultaneous strong
// definitions, the dependency owners it stores to, and their retained contents.
type loopFlowLiveNode struct {
	use, def, targets map[*loopVariable]bool
	contents          loopValue
}

func (lp *loopFlowLivePass) recordBlock(block *cfg.Block) {
	use, def := map[*loopVariable]bool{}, map[*loopVariable]bool{}
	lp.uses[block.Index], lp.defs[block.Index] = use, def
	for _, node := range block.Nodes {
		n := lp.nodeBindings(node)
		for v := range n.use {
			if !def[v] {
				use[v] = true
			}
		}
		for v := range n.def {
			def[v] = true
		}
		for target := range n.targets {
			lp.depend(target, n.contents)
		}
	}
}

func (lp *loopFlowLivePass) nodeBindings(node ast.Node) *loopFlowLiveNode {
	use, def, targets, weak := lp.s.loopFlowNodeBindings(node)
	n := &loopFlowLiveNode{use: use, def: def, targets: targets, contents: lp.s.loopFlowLiveStoredValue(node, lp.w)}
	lp.addPointerTargets(node, n, weak)
	for _, a := range lp.adapters {
		if a != nil {
			lp.addRangeBindings(a, node, n)
		}
	}
	for _, a := range lp.adapters {
		if a != nil && a.deferredPlan != nil {
			lp.addDeferredBindings(a, node, n)
		}
	}
	return n
}

// addPointerTargets adds the cells a store may update through a pointer:
// the may-address targets of a weakly updated base and nested destinations.
func (lp *loopFlowLivePass) addPointerTargets(node ast.Node, n *loopFlowLiveNode, weak map[*loopVariable]bool) {
	for target := range weak {
		for pointee := range lp.addresses[target] {
			n.targets[pointee] = true
		}
	}
	// Nested pointer destinations can update a cell several loads
	// away from the syntactic base. Its future retained contents and
	// companion metadata must survive joins too.
	assignment, ok := node.(*ast.AssignStmt)
	if !ok {
		return
	}
	for _, lhs := range assignment.Lhs {
		if _, direct := loopUnparen(lhs).(*ast.Ident); !direct {
			for target := range lp.staticFlow.storeRoots(lhs, lp.w) {
				n.targets[target] = true
			}
		}
	}
}

func (lp *loopFlowLivePass) addRangeBindings(a *loopFlowAST, node ast.Node, n *loopFlowLiveNode) {
	if rng := a.entries[node]; rng != nil {
		n.def[rng.key], n.def[rng.value] = true, true
		n.targets[rng.key], n.targets[rng.value] = true, true
		key, value := lp.staticFlow.rangeValues(rng.stmt.X, lp.w)
		loopMerge(n.contents, key)
		loopMerge(n.contents, value)
	}
	if rng := a.steps[node]; rng != nil {
		n.use[rng.key], n.use[rng.value] = true, true
		loopMerge(n.contents, lp.w.values[rng.key])
		loopMerge(n.contents, lp.w.values[rng.value])
	}
}

func (lp *loopFlowLivePass) addDeferredBindings(a *loopFlowAST, node ast.Node, n *loopFlowLiveNode) {
	plan := a.deferredPlan
	// Whole-variable writes also replace their callable provenance.
	// Without this kill, future metadata roots retain every dead copy.
	for v := range n.def {
		if slot := plan.flow.companions[v]; slot != nil {
			n.def[slot] = true
		}
	}
	u, d, dependencies := plan.live(node, lp.w, lp.staticFlow.addressAliases)
	for v := range u {
		n.use[v] = true
	}
	for v := range d {
		n.def[v] = true
	}
	if _, exit := plan.exits[node]; exit {
		for v := range a.exports {
			n.use[v] = true
		}
	}
	for target, value := range dependencies {
		lp.depend(target, value)
	}
}

// depend makes target an owner of value's references. A symbol stands for a
// copied binding's contents, so it becomes a copy edge from that binding.
func (lp *loopFlowLivePass) depend(target *loopVariable, value loopValue) {
	if lp.deps[target] == nil {
		lp.deps[target] = map[*loopVariable]bool{}
	}
	for v := range value {
		if source := lp.sources[v]; source != nil {
			lp.copies[source] = append(lp.copies[source], target)
		} else {
			lp.deps[target][v] = true
		}
	}
}

// solve closes the block equations backward over actual CFG edges.
func (lp *loopFlowLivePass) solve(graph *cfg.CFG) []map[*loopVariable]bool {
	preds := make([][]int32, len(graph.Blocks))
	for _, block := range graph.Blocks {
		for _, next := range block.Succs {
			preds[next.Index] = append(preds[next.Index], block.Index)
		}
	}
	live := make([]map[*loopVariable]bool, len(graph.Blocks))
	queued := make([]bool, len(graph.Blocks))
	queue := make([]int32, 0, len(graph.Blocks))
	// Reverse order usually settles straight-line regions in one pass. Actual
	// successors/predecessors, including goto and exit edges, drive convergence.
	for i := len(graph.Blocks) - 1; i >= 0; i-- {
		live[i] = map[*loopVariable]bool{}
		queue = append(queue, int32(i))
		queued[i] = true
	}
	for head := 0; head < len(queue); head++ {
		i := queue[head]
		queued[i] = false
		needed := lp.needed(graph, i, live)
		if len(needed) == len(live[i]) { // equations grow monotonically
			continue
		}
		live[i] = needed
		for _, pred := range preds[i] {
			if !queued[pred] {
				queued[pred] = true
				queue = append(queue, pred)
			}
		}
	}
	return live
}

// needed is block i's entry liveness from its successors' current entries,
// closed over static binding dependencies.
func (lp *loopFlowLivePass) needed(graph *cfg.CFG, i int32, live []map[*loopVariable]bool) map[*loopVariable]bool {
	needed := map[*loopVariable]bool{}
	for v := range lp.uses[i] {
		needed[v] = true
	}
	for _, next := range graph.Blocks[i].Succs {
		for v := range live[next.Index] {
			if !lp.defs[i][v] {
				needed[v] = true
			}
		}
	}
	pending := make([]*loopVariable, 0, len(needed))
	for v := range needed {
		pending = append(pending, v)
	}
	for j := 0; j < len(pending); j++ {
		for dep := range lp.deps[pending[j]] {
			if !needed[dep] {
				needed[dep] = true
				pending = append(pending, dep)
			}
		}
	}
	return needed
}

// loopFlowLiveStoredValue uses the same value semantics as transfer. Reads used
// only to compute a value (call arguments, indexes, destination addresses) do
// not become references retained by that value. Map keys are retained stores;
// keeping other destination indexes too is a conservative syntax-only fallback.
func (s *loopScanner) loopFlowLiveStoredValue(node ast.Node, w *loopWalk) loopValue {
	out := loopValue{}
	merge := func(e ast.Expr) { loopMerge(out, s.value(e, w)) }
	keys := func(e ast.Expr) {
		for e != nil {
			switch n := loopUnparen(e).(type) {
			case *ast.IndexExpr:
				merge(n.Index)
				e = n.X
			case *ast.SelectorExpr:
				e = n.X
			case *ast.StarExpr:
				e = n.X
			default:
				e = nil
			}
		}
	}
	switch n := node.(type) {
	case *ast.AssignStmt:
		for _, rhs := range n.Rhs {
			merge(rhs)
		}
		for _, lhs := range n.Lhs {
			keys(lhs)
		}
	case *ast.ValueSpec:
		for _, rhs := range n.Values {
			merge(rhs)
		}
	case *ast.SendStmt:
		merge(n.Value)
	}
	return out
}

// loopFlowNodeBindings collects uses before simultaneous strong definitions.
// Complex destinations READ their base/index/address; they are weak stores, not
// kills of the base binding. targets records dependency owners, NOT retained
// contents: syntactic reads cannot distinguish a value copy from a slot capture.
func (s *loopScanner) loopFlowNodeBindings(node ast.Node) (use, def, targets, weak map[*loopVariable]bool) {
	use, def, targets, weak = map[*loopVariable]bool{}, map[*loopVariable]bool{}, map[*loopVariable]bool{}, map[*loopVariable]bool{}
	read := func(n ast.Node, into map[*loopVariable]bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				for v := range s.index.captures[n] {
					into[v] = true
				}
				return false // locals and definitions belong to its own frame
			case *ast.Ident:
				if v := s.index.ids[n]; v != nil {
					into[v] = true
				}
			}
			return true
		})
	}
	lhs := func(e ast.Expr, compound bool) {
		if id, ok := loopUnparen(e).(*ast.Ident); ok {
			if v := s.index.ids[id]; v != nil {
				def[v], targets[v] = true, true
				if compound {
					use[v] = true
				}
			}
		} else {
			read(e, use)
			// Only the storage base owns the weak update, not its index.
			base := e
			for base != nil {
				switch b := loopUnparen(base).(type) {
				case *ast.Ident:
					if v := s.index.ids[b]; v != nil {
						targets[v], weak[v] = true, true
					}
					base = nil
				case *ast.SelectorExpr:
					base = b.X
				case *ast.IndexExpr:
					base = b.X
				case *ast.StarExpr:
					base = b.X
				case *ast.UnaryExpr:
					base = b.X
				default:
					read(base, targets)
					read(base, weak)
					base = nil
				}
			}
		}
	}
	switch n := node.(type) {
	case *ast.AssignStmt:
		for _, e := range n.Rhs {
			read(e, use)
		}
		for _, e := range n.Lhs {
			lhs(e, n.Tok != token.ASSIGN && n.Tok != token.DEFINE)
		}
	case *ast.ValueSpec:
		for _, e := range n.Values {
			read(e, use)
		}
		for _, id := range n.Names {
			lhs(id, false)
		}
	case *ast.SendStmt:
		read(n, use)
		read(n.Chan, targets)
		read(n.Chan, weak)
	default:
		read(node, use)
	}
	return use, def, targets, weak
}

// loopFlowLiveAddresses is a flow-insensitive may-alias graph for dependency
// owners, NOT a transfer analysis. Strong-update precision still belongs to
// loopFlow.transfer. Captures and ordinary call results are not address aliases.
func (s *loopScanner) loopFlowLiveAddresses(graph *cfg.CFG, inherited map[*loopVariable]loopValue) map[*loopVariable]map[*loopVariable]bool {
	g := &loopFlowLiveAddressGraph{
		s:        s,
		out:      map[*loopVariable]map[*loopVariable]bool{},
		copies:   map[*loopVariable][]*loopVariable{},
		watchers: map[*loopVariable]map[int]bool{},
		queued:   map[*loopVariable]bool{},
	}
	for v, targets := range inherited {
		g.out[v] = map[*loopVariable]bool{}
		for target := range targets {
			g.out[v][target] = true
		}
	}
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			g.collect(node)
		}
	}
	g.solve()
	return g.out
}

// loopFlowLiveAddressGraph holds loopFlowLiveAddresses' constraints: direct
// address and copy facts, and indirect assignments evaluated against the
// address facts they read.
type loopFlowLiveAddressGraph struct {
	s        *loopScanner
	out      map[*loopVariable]map[*loopVariable]bool
	copies   map[*loopVariable][]*loopVariable
	indirect []loopFlowLiveIndirect
	watchers map[*loopVariable]map[int]bool // binding -> indirect assignments reading it
	queue    []*loopVariable
	queued   map[*loopVariable]bool
}

type loopFlowLiveIndirect struct{ lhs, rhs ast.Expr }

func (g *loopFlowLiveAddressGraph) collect(node ast.Node) {
	switch n := node.(type) {
	case *ast.AssignStmt:
		for i, rhs := range n.Rhs {
			if i < len(n.Lhs) {
				g.assign(n.Lhs[i], rhs)
			}
		}
	case *ast.ValueSpec:
		for i, rhs := range n.Values {
			if i < len(n.Names) {
				g.assign(n.Names[i], rhs)
			}
		}
	}
}

func (g *loopFlowLiveAddressGraph) source(e ast.Expr) (address, copy *loopVariable) {
	switch e := loopUnparen(e).(type) {
	case *ast.Ident:
		return nil, g.s.index.ids[e]
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			if _, star := loopUnparen(e.X).(*ast.StarExpr); !star {
				return g.s.root(e.X), nil
			}
		}
	case *ast.CallExpr:
		if len(e.Args) == 1 && g.s.conversion(e.Fun) {
			return g.source(e.Args[0])
		}
	}
	return nil, nil
}

func (g *loopFlowLiveAddressGraph) assign(lhs, rhs ast.Expr) {
	id, ok := loopUnparen(lhs).(*ast.Ident)
	if !ok {
		if _, star := loopUnparen(lhs).(*ast.StarExpr); star {
			g.indirect = append(g.indirect, loopFlowLiveIndirect{lhs, rhs})
		}
		return
	}
	dst := g.s.index.ids[id]
	if dst == nil {
		return
	}
	address, copy := g.source(rhs)
	if address == nil && copy == nil {
		g.indirect = append(g.indirect, loopFlowLiveIndirect{lhs, rhs})
	}
	if address != nil {
		if g.out[dst] == nil {
			g.out[dst] = map[*loopVariable]bool{}
		}
		g.out[dst][address] = true
	}
	if copy != nil {
		g.copies[copy] = append(g.copies[copy], dst)
	}
}

// Pointer-cell stores and pointer loads introduce dynamic may-copy edges.
// Watch only the address facts each constraint actually reads; rescanning
// every assignment to a fixed point makes long alias chains quadratic.
func (g *loopFlowLiveAddressGraph) solve() {
	g.queue = make([]*loopVariable, 0, len(g.out))
	for v := range g.out {
		g.enqueue(v)
	}
	for i := range g.indirect {
		g.evaluate(i)
	}
	for head := 0; head < len(g.queue); head++ {
		v := g.queue[head]
		g.queued[v] = false
		for _, dst := range g.copies[v] {
			g.merge(dst, g.out[v])
		}
		for i := range g.watchers[v] {
			g.evaluate(i)
		}
	}
}

func (g *loopFlowLiveAddressGraph) enqueue(v *loopVariable) {
	if !g.queued[v] {
		g.queue = append(g.queue, v)
		g.queued[v] = true
	}
}

func (g *loopFlowLiveAddressGraph) merge(dst *loopVariable, value map[*loopVariable]bool) {
	for target := range value {
		if g.out[dst] == nil {
			g.out[dst] = map[*loopVariable]bool{}
		}
		if !g.out[dst][target] {
			g.out[dst][target] = true
			g.enqueue(dst)
		}
	}
}

func (g *loopFlowLiveAddressGraph) evaluate(i int) {
	assignment := g.indirect[i]
	var destinations map[*loopVariable]bool
	switch lhs := loopUnparen(assignment.lhs).(type) {
	case *ast.Ident:
		destinations = map[*loopVariable]bool{g.s.index.ids[lhs]: true}
	case *ast.StarExpr:
		destinations = g.value(lhs.X, i)
	}
	rhs := g.value(assignment.rhs, i)
	for dst := range destinations {
		g.merge(dst, rhs)
	}
}

// read returns v's address facts and has them re-evaluate indirect
// assignment i when they grow.
func (g *loopFlowLiveAddressGraph) read(v *loopVariable, i int) map[*loopVariable]bool {
	if v == nil {
		return nil
	}
	if g.watchers[v] == nil {
		g.watchers[v] = map[int]bool{}
	}
	g.watchers[v][i] = true
	return g.out[v]
}

// value is the address set expr evaluates to for indirect assignment i.
func (g *loopFlowLiveAddressGraph) value(expr ast.Expr, i int) map[*loopVariable]bool {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		return g.read(g.s.index.ids[expr], i)
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			if star, ok := loopUnparen(expr.X).(*ast.StarExpr); ok {
				return g.value(star.X, i)
			}
			if v := g.s.root(expr.X); v != nil {
				return map[*loopVariable]bool{v: true}
			}
		}
	case *ast.StarExpr:
		loaded := map[*loopVariable]bool{}
		for cell := range g.value(expr.X, i) {
			for target := range g.read(cell, i) {
				loaded[target] = true
			}
		}
		return loaded
	case *ast.CallExpr:
		if len(expr.Args) == 1 && g.s.conversion(expr.Fun) {
			return g.value(expr.Args[0], i)
		}
	}
	return nil
}

// loopFlowLivePropagate solves a flow-insensitive may-contents graph. Copy
// cycles and joins union reference CONTENTS, never the copied source slot.
func loopFlowLivePropagate(out map[*loopVariable]map[*loopVariable]bool, copies map[*loopVariable][]*loopVariable) {
	queue := make([]*loopVariable, 0, len(out))
	queued := map[*loopVariable]bool{}
	for v := range out {
		queue = append(queue, v)
		queued[v] = true
	}
	for head := 0; head < len(queue); head++ {
		v := queue[head]
		queued[v] = false
		for _, dst := range copies[v] {
			if out[dst] == nil {
				out[dst] = map[*loopVariable]bool{}
			}
			changed := false
			for target := range out[v] {
				if !out[dst][target] {
					out[dst][target], changed = true, true
				}
			}
			if changed && !queued[dst] {
				queued[dst] = true
				queue = append(queue, dst)
			}
		}
	}
}

// loopPruneBindings makes a writable, shallow state snapshot containing only
// needed roots and their transitive state dependencies. Values stay immutable.
// It visits roots, NOT every entry of state, so dead aliases do not make each
// successor join or block clone increasingly expensive. Empty bindings carry no
// information in loopJoinBindings and need not be copied.
//
// With loopFlow's private address slots, pass f.aliases. Keeping a live ordinary
// binding must also keep its alias metadata (and the referenced local contents).
// Static liveness roots are still required: state closure alone is not sound at
// joins where different predecessors contribute a closure and its pointee.
func loopPruneBindings(state map[*loopVariable]loopValue, needed map[*loopVariable]bool, aliases ...map[*loopVariable]*loopVariable) map[*loopVariable]loopValue {
	out := make(map[*loopVariable]loopValue)
	seen := map[*loopVariable]bool{}
	pending := make([]*loopVariable, 0, len(needed))
	for v, yes := range needed {
		if yes {
			pending = append(pending, v)
		}
	}
	for len(pending) != 0 {
		v := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if v == nil || seen[v] {
			continue
		}
		seen[v] = true
		for _, slots := range aliases {
			if slot := slots[v]; slot != nil {
				pending = append(pending, slot)
			}
		}
		if value := state[v]; len(value) != 0 {
			out[v] = value
			for dep := range value {
				// All modes are conservative roots, including write-only captures.
				pending = append(pending, dep)
			}
		}
	}
	return out
}
