package diago

import (
	"go/ast"
	"go/token"
)

// expressionRightEvaluated reports a may-execution edge, not a value-flow
// fact. Mutable booleans keep both edges; even a known result never suppresses
// evaluation of the left operand. Share the builtin/constant evidence with the
// deferred expression evaluator, without running its captured-binding interpreter.
func (s *loopScanner) expressionRightEvaluated(expr *ast.BinaryExpr) bool {
	if expr.Op != token.LAND && expr.Op != token.LOR {
		return true
	}
	left, known := s.deferredTruth(expr.X)
	return !known || !(expr.Op == token.LAND && !left || expr.Op == token.LOR && left)
}

// executionChildren is the expression-semantic counterpart of loopChildren.
// It does not enter literal bodies: construction and invocation are separate
// frames. Identity/type indexing must still walk the original syntax; these
// edges are only for execution, effects, and construction-seed discovery.
func (s *loopScanner) executionChildren(node ast.Node, visit func(ast.Node)) {
	switch n := node.(type) {
	case *ast.FuncLit:
		return
	case *ast.BinaryExpr:
		visit(n.X)
		if s.expressionRightEvaluated(n) {
			visit(n.Y)
		}
	case *ast.CallExpr:
		s.executionOperands(n, visit)
	default:
		loopChildren(node, visit)
	}
}

func (s *loopScanner) executionOperands(call *ast.CallExpr, visit func(ast.Node)) {
	visit(call.Fun)
	if !s.deferredUnevaluated(call) {
		for _, arg := range call.Args {
			visit(arg)
		}
	}
}

// inspectExecuted has Inspect's pruning convention (without nil callbacks),
// but visits only possible expression evaluation edges in the current frame.
// Statements still require CFG reachability from the caller.
func (s *loopScanner) inspectExecuted(node ast.Node, inspect func(ast.Node) bool) {
	if node == nil || !inspect(node) {
		return
	}
	s.executionChildren(node, func(child ast.Node) { s.inspectExecuted(child, inspect) })
}
