package diago

import "go/ast"

// Discovery owns syntax/function/loop boundaries only. Binding states and
// synchronous effects belong to the CFG pass; duplicating them in a lexical
// branch walker is both incorrect around jumps and needlessly quadratic.
func (s *loopScanner) discover(n ast.Node, w *loopWalk) {
	if n == nil {
		return
	}
	switch n := n.(type) {
	case *ast.FuncLit:
		next := *w
		next.owner, next.deferDepth = n, 0
		s.discover(n.Body, &next)
	case *ast.ForStmt:
		s.discover(n.Init, w)
		next := *w
		next.loops = append(append([]*loopRegion{}, w.loops...), s.index.regions[n])
		next.deferDepth++
		s.discover(n.Cond, &next)
		s.discover(n.Body, &next)
		s.discover(n.Post, &next)
	case *ast.RangeStmt:
		if s.index.rangeExpressionEvaluated(n) {
			s.discover(n.X, w)
		}
		next := *w
		next.loops = append(append([]*loopRegion{}, w.loops...), s.index.regions[n])
		next.deferDepth++
		s.discover(n.Body, &next)
	case *ast.GoStmt:
		// All launches are conflict/effect events, even outside a loop. Only
		// diagnostics are restricted to launches with an enclosing loop.
		s.launches = append(s.launches, loopLaunch{stmt: n, owner: w.owner, loops: w.loops})
		s.executionOperands(n.Call, func(child ast.Node) { s.discover(child, w) })
	case *ast.DeferStmt:
		if w.deferDepth != 0 {
			s.finding(n, "defer-in-loop", "defer inside loop can delay resource release")
		}
		s.executionOperands(n.Call, func(child ast.Node) { s.discover(child, w) })
	default:
		s.executionChildren(n, func(child ast.Node) { s.discover(child, w) })
	}
}
