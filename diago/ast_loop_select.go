package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// selectReceive separates Go's two evaluation phases without changing source
// syntax or indexing synthetic identifiers. All channel operands (and the
// untouched send operands) execute in source order in cfg's common block. A
// selected receive then evaluates its destination addresses and assigns an
// opaque received value. In particular, channel contents are not alias proof,
// and the second destination of a comma-ok receive remains an empty bool value.
func (a *loopFlowAST) selectReceive(comm ast.Stmt) (ast.Expr, ast.Stmt) {
	var rhs ast.Expr
	switch n := comm.(type) {
	case *ast.AssignStmt:
		if len(n.Rhs) != 1 {
			return nil, nil
		}
		rhs = n.Rhs[0]
	case *ast.ExprStmt:
		rhs = n.X
	default:
		return nil, nil
	}
	receive, ok := loopUnparen(rhs).(*ast.UnaryExpr)
	if !ok || receive.Op != token.ARROW {
		return nil, nil
	}
	if a.selectSources == nil {
		a.selectSources = map[ast.Node]ast.Node{}
	}
	// A private expression identity is essential: the same source operand can
	// occur in multiple expanded iterator copies. Its children stay original
	// so calls, identifiers, type queries, and evaluation-time environments
	// retain their metadata.
	channel := &ast.ParenExpr{Lparen: receive.X.Pos(), X: receive.X, Rparen: receive.X.End() - 1}
	a.selectSources[channel] = receive.X
	value := loopSelectOpaqueReceive(receive)
	a.selectSources[value] = receive
	var selected ast.Stmt
	switch n := comm.(type) {
	case *ast.AssignStmt:
		copy := *n
		copy.Rhs = []ast.Expr{value}
		selected = &copy // original LHS identities, evaluated only on this edge
	case *ast.ExprStmt:
		copy := *n
		copy.X = value
		selected = &copy
	}
	a.selectSources[selected] = comm
	return channel, selected
}

func loopSelectOpaqueReceive(receive *ast.UnaryExpr) *ast.UnaryExpr {
	copy := *receive
	// No channel/result inference is attempted. This operand is deliberately
	// opaque and effect-free, not a copy of the producer AST. Preserve End for
	// physical ordering of the selected assignment's writes.
	copy.X = &ast.BasicLit{ValuePos: receive.X.End() - 1, Kind: token.INT, Value: "0"}
	return &copy
}

// finishSelectCFG must run after iterator expansion and before deferred
// planning/recording. Expanded copies share original syntax, but private select
// markers must identify one physical occurrence. Reattach declaration births
// to the synthetic selected assignments without changing the caller's AST.
func (a *loopFlowAST) finishSelectCFG(graph *cfg.CFG, births map[ast.Node][]*loopVariable) {
	if graph == nil || len(a.selectSources) == 0 {
		return
	}
	seen := map[ast.Node]bool{}
	for _, block := range graph.Blocks {
		for i, node := range block.Nodes {
			source := a.selectSources[node]
			if source == nil {
				continue
			}
			if seen[node] {
				switch n := node.(type) {
				case *ast.ParenExpr:
					copy := *n
					node = &copy
				case *ast.UnaryExpr:
					node = loopSelectOpaqueReceive(n)
				case *ast.ExprStmt:
					copy := *n
					switch expr := n.X.(type) {
					case *ast.ParenExpr:
						operand := *expr
						copy.X = &operand
					case *ast.UnaryExpr:
						copy.X = loopSelectOpaqueReceive(expr)
					}
					node = &copy
				case *ast.AssignStmt:
					copy := *n
					copy.Rhs = []ast.Expr{loopSelectOpaqueReceive(n.Rhs[0].(*ast.UnaryExpr))}
					node = &copy
				}
				a.selectSources[node] = source
				block.Nodes[i] = node
			}
			seen[node] = true
			if births != nil && len(births[source]) != 0 {
				births[node] = births[source]
			}
		}
	}
}
