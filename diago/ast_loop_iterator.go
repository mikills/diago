package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// A known iterator and its yield callback form a synchronous coroutine, not a
// call at X followed by an independent loop. Splice the iterator CFG into the
// caller: a yield enters the existing range body, fallthrough/continue resumes
// with true, and break/return resumes with false before the pending caller exit.
// In the false state a further yield terminates (the runtime panics).
//
// Each physical range invocation has its own entry and snapshot slots, even
// when multiple invocations reuse the same source AST. Within that occurrence
// the callback body is shared between yield sites: their continuations join,
// but no edge ever resumes the iterator's initial entry. Effects reachable
// only before the first yield cannot be mistaken for post-launch effects.
// Original AST identities and function owners are preserved, including defers;
// only CFG edges change. The ordinary range adapter still creates fresh target
// storage on body entry, with private slots snapshotting each yield's arguments.
type loopFlowIterator struct {
	rng   *loopFlowRange
	lit   *ast.FuncLit
	entry *cfg.Block // the physical occurrence, never an AST-expression search
}

// Exact coroutine expansion can be exponential in a compact iterator DAG.
// Bound both storage and expansion work, and report incompleteness rather than
// treating an unexpanded known iterator as a proven-clean invocation.
const loopIteratorBlockBudget = 4096
const loopIteratorOccurrenceBudget = 512

// The wrapper is opaque only as an identity: its child retains every original
// identifier, type-info key and function owner used by effects and bindings.
func loopIteratorEntry(x ast.Expr) ast.Expr {
	return &ast.ParenExpr{Lparen: x.Pos(), X: x, Rparen: x.End()}
}

func (a *loopFlowAST) planIterator(rng *loopFlowRange) {
	if a.index == nil {
		return
	}
	x := loopUnparen(rng.stmt.X)
	lit, _ := x.(*ast.FuncLit)
	if id, ok := x.(*ast.Ident); ok {
		v := a.index.ids[id]
		// A single local literal initializer, never reassigned or addressed.
		// No call-result, field, alias, or package-function inference.
		if v == nil || !v.valueInitializerKnown || len(v.writes) != 0 {
			return
		}
		lit, _ = loopUnparen(v.valueInitializer).(*ast.FuncLit)
		for node := range a.index.locations {
			if unary, ok := node.(*ast.UnaryExpr); ok && unary.Op == token.AND {
				if name, ok := loopUnparen(unary.X).(*ast.Ident); ok && a.index.ids[name] == v {
					return
				}
			}
		}
	}
	if lit == nil || lit.Type.Params == nil || len(lit.Type.Params.List) != 1 || len(lit.Type.Params.List[0].Names) != 1 {
		return
	}
	yield := a.index.ids[lit.Type.Params.List[0].Names[0]]
	if yield == nil || !loopIteratorSupported(lit.Body, yield, a.index) || !loopIteratorBodySupported(rng.stmt, a.rangeLabels[rng.stmt]) {
		return
	}
	if a.iteratorEntries == nil {
		a.iteratorEntries = map[ast.Node]bool{}
		a.inlinedIterators = map[*ast.FuncLit]bool{}
	}
	rng.iterator = lit
	a.iterators = append(a.iterators, &loopFlowIterator{rng: rng, lit: lit})
	a.inlinedIterators[lit] = true
}

// Direct yield expression statements and boolean conditions (including !yield)
// have explicit true/false CFG successors. An escaped/stored yield callback or
// a yield buried in an arbitrary expression needs a richer expression CFG and
// deliberately keeps the conservative ordinary invocation fallback.
func loopIteratorSupported(body *ast.BlockStmt, yield *loopVariable, index *loopIndex) bool {
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		var expr ast.Expr
		switch n := node.(type) {
		case *ast.ExprStmt:
			expr = n.X
		case *ast.IfStmt:
			expr = n.Cond
		case *ast.ForStmt:
			expr = n.Cond
		}
		if call, _ := loopIteratorYield(expr, yield, index); call != nil {
			allowed[loopUnparen(call.Fun).(*ast.Ident)] = true
		}
		_, literal := node.(*ast.FuncLit)
		return !literal
	})
	ok := true
	ast.Inspect(body, func(node ast.Node) bool {
		if id, yes := node.(*ast.Ident); yes && index.ids[id] == yield && !allowed[id] {
			ok = false
		}
		return true
	})
	return ok
}

func loopIteratorBodySupported(rng *ast.RangeStmt, ownLabel string) bool {
	// External labeled transfers need a pending-exit stack across nested
	// iterators. Break/continue of this range, return, and body-local gotos work.
	labels := map[string]bool{}
	ast.Inspect(rng.Body, func(node ast.Node) bool {
		if label, ok := node.(*ast.LabeledStmt); ok {
			labels[label.Label.Name] = true
		}
		_, literal := node.(*ast.FuncLit)
		return !literal
	})
	ok := true
	ast.Inspect(rng.Body, func(node ast.Node) bool {
		if branch, yes := node.(*ast.BranchStmt); yes && branch.Label != nil && !labels[branch.Label.Name] {
			if branch.Label.Name != ownLabel || branch.Tok != token.BREAK && branch.Tok != token.CONTINUE {
				ok = false
			}
		}
		_, literal := node.(*ast.FuncLit)
		return !literal
	})
	return ok
}

func loopIteratorYield(expr ast.Expr, yield *loopVariable, index *loopIndex) (*ast.CallExpr, bool) {
	negated := false
	if expr == nil {
		return nil, false
	}
	expr = loopUnparen(expr)
	for {
		unary, ok := expr.(*ast.UnaryExpr)
		if !ok || unary.Op != token.NOT {
			break
		}
		negated = !negated
		expr = loopUnparen(unary.X)
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if id, ok := loopUnparen(call.Fun).(*ast.Ident); ok && index.ids[id] == yield {
			return call, negated
		}
	}
	return nil, false
}

func loopIteratorBlock(graph *cfg.CFG, nodes []ast.Node, next ...*cfg.Block) *cfg.Block {
	block := &cfg.Block{Index: int32(len(graph.Blocks)), Nodes: nodes, Succs: next}
	graph.Blocks = append(graph.Blocks, block)
	return block
}

// Splitting at primitive nodes makes each yield a suspension point, even when
// it shares a basic block with earlier/later writes or the implicit return.
func loopIteratorSplit(graph *cfg.CFG) {
	blocks := append([]*cfg.Block(nil), graph.Blocks...)
	for _, block := range blocks {
		if len(block.Nodes) < 2 {
			continue
		}
		nodes, next := block.Nodes, block.Succs
		block.Nodes, block.Succs = nodes[:1], nil
		last := block
		for _, node := range nodes[1:] {
			current := loopIteratorBlock(graph, []ast.Node{node})
			last.Succs = []*cfg.Block{current}
			last = current
		}
		last.Succs = next
	}
}

func (a *loopFlowAST) expandIterators(graph *cfg.CFG) {
	// Initial plans belong to the caller CFG. Later plans are registered with
	// their exact entry block as each true/false iterator copy is created.
	plans := map[ast.Node]*loopFlowIterator{}
	for _, it := range a.iterators {
		plans[it.rng.entry] = it
	}
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			if it := plans[node]; it != nil {
				it.entry = block
			}
		}
	}
	initial := len(graph.Blocks)
	for i := 0; i < len(a.iterators); i++ {
		it := a.iterators[i]
		if i >= loopIteratorOccurrenceBudget || !a.expandIterator(graph, it, loopIteratorBlockBudget-(len(graph.Blocks)-initial)) {
			a.iteratorIncomplete = it.rng.stmt
			return
		}
	}
}

func (a *loopFlowAST) expandIterator(graph *cfg.CFG, it *loopFlowIterator, budget int) bool {
	if it.entry == nil || len(it.entry.Succs) != 1 {
		return false
	}
	header := it.entry.Succs[0]
	if len(header.Succs) != 2 {
		return false
	}
	body, done := header.Succs[0], header.Succs[1]
	// Capture callback exits BEFORE installing iterator edges.
	type exit struct {
		from, to *cfg.Block
		edge     int
	}
	var exits []exit
	seen := map[*cfg.Block]bool{}
	queue := []*cfg.Block{body}
	for len(queue) != 0 {
		block := queue[0]
		queue = queue[1:]
		if seen[block] {
			continue
		}
		seen[block] = true
		if block.Return() != nil {
			if len(block.Succs) == 1 {
				// An enclosing iterator has already installed its pending
				// return continuation. Finish this iterator before that one.
				exits = append(exits, exit{from: block, to: block.Succs[0], edge: 0})
			} else {
				exits = append(exits, exit{from: block, edge: -1})
			}
			continue
		}
		for edge, next := range block.Succs {
			if next == header || next == done {
				exits = append(exits, exit{from: block, to: next, edge: edge})
			} else {
				queue = append(queue, next)
			}
		}
	}
	// Use the same adapter maps for nested ranges/copy steps and ordinary label
	// repair. Iterator returns will be replaced by edges to the caller exit.
	// Adapting a template discovers nested ranges, not physical invocations.
	// Remove its plans: clone below creates one plan and fresh snapshot slots
	// per actual occurrence, including copies entered after a false yield.
	start, template := len(a.iterators), a.iteratorTemplate
	a.iteratorTemplate = true
	iterator := cfg.New(a.block(it.lit.Body), func(*ast.CallExpr) bool { return true })
	a.iteratorTemplate = template
	a.iterators = a.iterators[:start]
	loopIteratorSplit(iterator)
	yield := a.index.ids[it.lit.Type.Params.List[0].Names[0]]
	type suspension struct {
		block, yes, no *cfg.Block
		call           *ast.CallExpr
	}
	atYield := map[*cfg.Block]suspension{}
	for _, block := range iterator.Blocks {
		if len(block.Nodes) != 1 {
			continue
		}
		var expr ast.Expr
		switch node := block.Nodes[0].(type) {
		case ast.Expr:
			expr = node
		case *ast.ExprStmt:
			expr = node.X
		}
		if call, negated := loopIteratorYield(expr, yield, a.index); call != nil {
			yes, no := block.Succs[0], block.Succs[0]
			if len(block.Succs) == 2 {
				no = block.Succs[1]
				if negated {
					yes, no = no, yes
				}
			}
			point := suspension{block, yes, no, call}
			atYield[block] = point
		}
	}
	// Only yields reachable while earlier callbacks return true can invoke
	// the shared body. Do not resurrect dead code or a yield in a false-only
	// branch by adding its continuation to the shared resume point.
	var yields []suspension
	reachable := map[*cfg.Block]bool{}
	pending := []*cfg.Block{iterator.Blocks[0]}
	for len(pending) != 0 {
		block := pending[0]
		pending = pending[1:]
		if reachable[block] {
			continue
		}
		reachable[block] = true
		if point, ok := atYield[block]; ok {
			yields = append(yields, point)
			pending = append(pending, point.yes)
		} else {
			pending = append(pending, block.Succs...)
		}
	}
	// A stopped copy starts at false continuations, never at the iterator's
	// initial entry. Do not clone unreachable headers (or expand their DAGs).
	stoppedReachable := map[*cfg.Block]bool{}
	pending = nil
	for _, point := range yields {
		pending = append(pending, point.no)
	}
	for len(pending) != 0 {
		block := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if stoppedReachable[block] {
			continue
		}
		stoppedReachable[block] = true
		if _, stop := atYield[block]; !stop {
			pending = append(pending, block.Succs...)
		}
	}
	finishes := map[*cfg.Block]bool{}
	for _, out := range exits {
		if out.to != header {
			finishes[out.to] = true
		}
	}
	// Preflight before allocating or modifying any caller edges. The extra
	// blocks cover true/false resume points and a pending void-return exit.
	returnCost := func(selected map[*cfg.Block]bool) int {
		if a.deferred == nil {
			return 0
		}
		n := 0
		for block := range selected {
			if block.Return() != nil {
				n++
			}
		}
		return n
	}
	cost := len(reachable) + returnCost(reachable) + len(finishes)*(len(stoppedReachable)+returnCost(stoppedReachable)+2) + 1
	if cost > budget {
		return false
	}
	clone := func(finish *cfg.Block, stopped bool) map[*cfg.Block]*cfg.Block {
		selected := reachable
		if stopped {
			selected = stoppedReachable
		}
		mapping := map[*cfg.Block]*cfg.Block{}
		ranges := map[*loopFlowRange]*loopFlowRange{}
		occurrence := func(rng *loopFlowRange) *loopFlowRange {
			if ranges[rng] == nil {
				copy := *rng
				copy.entry = loopIteratorEntry(rng.stmt.X)
				copy.key, copy.value = &loopVariable{}, &loopVariable{}
				ranges[rng] = &copy
			}
			return ranges[rng]
		}
		for _, block := range iterator.Blocks {
			if !selected[block] {
				continue
			}
			copy := loopIteratorBlock(graph, nil)
			mapping[block] = copy
			if ret := block.Return(); ret != nil {
				if a.deferred != nil {
					// Do NOT hide this ReturnStmt yet: a nested range must
					// recognize its callback return and unwind before completion.
					copy.Nodes = []ast.Node{ret}
					a.deferred.markers[ret] = loopDeferredMarker{owner: it.lit, result: ret}
				}
				continue
			}
			if _, suspend := atYield[block]; suspend {
				continue // normal yields are installed below; stopped yields panic
			}
			for _, node := range block.Nodes {
				if rng := a.entries[node]; rng != nil {
					rng = occurrence(rng)
					node = rng.entry
					a.entries[node] = rng
					if rng.iterator != nil {
						a.iterators = append(a.iterators, &loopFlowIterator{rng: rng, lit: rng.iterator, entry: copy})
					}
				} else if rng := a.steps[node]; rng != nil {
					step := *node.(*ast.AssignStmt)
					node = &step
					a.steps[node] = occurrence(rng)
				}
				copy.Nodes = append(copy.Nodes, node)
			}
		}
		for _, block := range iterator.Blocks {
			copy := mapping[block]
			if copy == nil {
				continue
			}
			if ret := block.Return(); ret != nil {
				completion := finish
				if a.deferred != nil {
					completion = loopIteratorBlock(graph, []ast.Node{a.deferred.pendingExit(ret, it.lit)}, finish)
				}
				copy.Succs = []*cfg.Block{completion}
			} else if _, suspend := atYield[block]; !suspend {
				for _, next := range block.Succs {
					copy.Succs = append(copy.Succs, mapping[next])
				}
			}
		}
		return mapping
	}
	normal := clone(done, false)
	trueResume := loopIteratorBlock(graph, nil)
	for _, point := range yields {
		// Snapshot synchronous yield operands before entering the callback.
		var lhs []ast.Expr
		for i, slot := range []*loopVariable{it.rng.key, it.rng.value} {
			if i >= len(point.call.Args) {
				break
			}
			id := &ast.Ident{NamePos: point.call.Pos(), Name: "_yield"}
			a.index.ids[id] = slot
			lhs = append(lhs, id)
		}
		var nodes []ast.Node
		if len(lhs) != 0 {
			nodes = []ast.Node{&ast.AssignStmt{Lhs: lhs, Tok: token.ASSIGN, Rhs: point.call.Args}}
		}
		normal[point.block].Nodes, normal[point.block].Succs = nodes, []*cfg.Block{body}
		trueResume.Succs = append(trueResume.Succs, normal[point.yes])
	}
	falseResumes := map[*cfg.Block]*cfg.Block{}
	var pendingReturn *cfg.Block
	for _, out := range exits {
		var resume *cfg.Block
		if out.to == header {
			resume = trueResume
		} else {
			finish := out.to
			if finish == nil {
				if pendingReturn == nil {
					var nodes []ast.Node
					if a.deferred != nil {
						nodes = []ast.Node{a.deferred.pendingExit(out.from.Return(), a.owner)}
					}
					pendingReturn = loopIteratorBlock(graph, nodes)
				}
				finish = pendingReturn
			}
			resume = falseResumes[finish]
			if resume == nil {
				stopped := clone(finish, true)
				resume = loopIteratorBlock(graph, nil)
				for _, point := range yields {
					resume.Succs = append(resume.Succs, stopped[point.no])
				}
				falseResumes[finish] = resume
			}
		}
		if out.edge < 0 {
			out.from.Succs = []*cfg.Block{resume}
		} else {
			out.from.Succs[out.edge] = resume
		}
	}
	if a.deferred != nil {
		header.Nodes = append(header.Nodes, a.deferred.iteratorEntry(it.lit, it.lit.Pos()))
	}
	header.Succs = []*cfg.Block{normal[iterator.Blocks[0]]}
	a.iteratorEntries[it.rng.entry] = true
	return true
}
