package diago

import (
	"go/ast"
	"go/token"
)

// loopSiblingPath records a lexical alternative, not a general path condition.
// In particular, conditions in two sequential if statements are independent.
// Arms joined by switch fallthrough share one path entry.
type loopSiblingPath struct {
	parent   *loopSiblingPath
	decision ast.Node
}

type loopSiblingUse struct {
	count, writers int
}

func (u loopSiblingUse) add(mode loopAccess) loopSiblingUse {
	u.count++
	if mode&loopWrite != 0 {
		u.writers++
	}
	return u
}

type loopSiblingUses struct {
	total     loopSiblingUse
	decisions map[ast.Node]loopSiblingUse
	arms      map[*loopSiblingPath]loopSiblingUse
}

// loopSiblingHazards identifies launches with a potentially overlapping,
// different launch site accessing the same variable in the same function frame,
// where at least one access writes. This deliberately does not decide hazards
// from repeated loop iterations, parent-frame accesses, or three-clause copies;
// report must retain those checks even for mutually exclusive sibling sites.
//
// Indexing costs O(AST size); aggregation and queries cost O(sum of captured
// references * their branch depths), without comparing pairs of launch sites.
func loopSiblingHazards(root ast.Node, launches []loopLaunch) map[*ast.GoStmt]bool {
	hazards := make(map[*ast.GoStmt]bool)
	if len(launches) < 2 {
		return hazards
	}
	paths := loopAlternativePaths(root)
	type key struct {
		variable *loopVariable
		owner    *ast.FuncLit
	}
	uses := make(map[key]*loopSiblingUses)
	for _, launch := range launches {
		for v, mode := range launch.values {
			if mode&(loopReadAccessMask|loopWrite|loopIndirectWrite) == 0 {
				continue
			}
			k := key{v, launch.owner}
			u := uses[k]
			if u == nil {
				u = &loopSiblingUses{
					decisions: make(map[ast.Node]loopSiblingUse),
					arms:      make(map[*loopSiblingPath]loopSiblingUse),
				}
				uses[k] = u
			}
			u.total = u.total.add(mode)
			for p := paths[launch.stmt]; p != nil; p = p.parent {
				u.decisions[p.decision] = u.decisions[p.decision].add(mode)
				u.arms[p] = u.arms[p].add(mode)
			}
		}
	}

	for _, launch := range launches {
		for v, mode := range launch.values {
			if mode&(loopReadAccessMask|loopWrite|loopIndirectWrite) == 0 {
				continue
			}
			u := uses[key{v, launch.owner}]
			overlap := u.total
			for p := paths[launch.stmt]; p != nil; p = p.parent {
				decision, arm := u.decisions[p.decision], u.arms[p]
				// The opposing arm subtrees of successive ancestors are
				// disjoint, so each excluded reference is subtracted once.
				overlap.count -= decision.count - arm.count
				overlap.writers -= decision.writers - arm.writers
			}
			// The launch's own reference is still included in overlap.
			if overlap.count > 1 && overlap.writers > 0 {
				hazards[launch.stmt] = true
				break
			}
		}
	}
	return hazards
}

func loopAlternativePaths(root ast.Node) map[ast.Node]*loopSiblingPath {
	paths := make(map[ast.Node]*loopSiblingPath)
	var walk func(ast.Node, *loopSiblingPath)
	var cases func(ast.Node, *ast.BlockStmt, *loopSiblingPath)
	cases = func(decision ast.Node, body *ast.BlockStmt, parent *loopSiblingPath) {
		if body == nil {
			return
		}
		var arm *loopSiblingPath
		fallthroughFromPrevious := false
		for _, stmt := range body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				walk(stmt, parent)
				continue
			}
			// Case expressions may be evaluated even when another clause
			// is selected. Only the bodies are alternatives.
			for _, expr := range clause.List {
				walk(expr, parent)
			}
			if !fallthroughFromPrevious {
				arm = &loopSiblingPath{parent: parent, decision: decision}
			}
			for _, child := range clause.Body {
				walk(child, arm)
			}
			fallthroughFromPrevious = false
			for i := len(clause.Body) - 1; i >= 0; i-- {
				// Legal fallthrough is the final non-empty, top-level
				// statement of an expression-switch clause.
				last := clause.Body[i]
				for {
					label, ok := last.(*ast.LabeledStmt)
					if !ok {
						break
					}
					last = label.Stmt
				}
				if _, empty := last.(*ast.EmptyStmt); empty {
					continue
				}
				branch, ok := last.(*ast.BranchStmt)
				fallthroughFromPrevious = ok && branch.Tok == token.FALLTHROUGH
				break
			}
		}
	}
	walk = func(node ast.Node, path *loopSiblingPath) {
		if node == nil {
			return
		}
		paths[node] = path
		switch n := node.(type) {
		case *ast.GoStmt:
			paths[n] = path
			walk(n.Call, path)
		case *ast.IfStmt:
			walk(n.Init, path)
			walk(n.Cond, path)
			walk(n.Body, &loopSiblingPath{parent: path, decision: n})
			walk(n.Else, &loopSiblingPath{parent: path, decision: n})
		case *ast.SwitchStmt:
			walk(n.Init, path)
			walk(n.Tag, path)
			cases(n, n.Body, path)
		case *ast.TypeSwitchStmt:
			walk(n.Init, path)
			walk(n.Assign, path)
			cases(n, n.Body, path)
		case *ast.SelectStmt:
			for _, stmt := range n.Body.List {
				clause := stmt.(*ast.CommClause)
				// Channel operands and send values are evaluated before
				// selection, not just in the selected alternative.
				arm := &loopSiblingPath{parent: path, decision: n}
				if assign, ok := clause.Comm.(*ast.AssignStmt); ok {
					paths[assign] = path
					for _, rhs := range assign.Rhs {
						walk(rhs, path)
					}
					// Receive-assignment destinations are evaluated and stored
					// only after this communication is selected.
					for _, lhs := range assign.Lhs {
						walk(lhs, arm)
					}
				} else {
					walk(clause.Comm, path)
				}
				for _, child := range clause.Body {
					walk(child, arm)
				}
			}
		default:
			loopChildren(node, func(child ast.Node) { walk(child, path) })
		}
	}
	walk(root, nil)
	return paths
}
