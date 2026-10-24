package diago

import (
	"go/ast"
	"go/token"
	"sort"
)

type loopDirectAccess struct {
	node        ast.Node
	variable    *loopVariable
	owner       *ast.FuncLit
	mode        loopAccess
	pos         token.Pos
	effect      bool // synthesized call/pointee effect rather than a lexical identifier
	summaryOnly bool // execution summary, not an event at this physical point
}

type loopAccessPositions struct {
	reads, writes []token.Pos
}

func (p *loopAccessPositions) add(access loopDirectAccess) {
	if access.mode&loopReadAccessMask != 0 {
		p.reads = append(p.reads, access.pos)
	}
	if access.mode&loopWrite != 0 {
		p.writes = append(p.writes, access.pos)
	}
}

func (p *loopAccessPositions) sort() {
	sort.Slice(p.reads, func(i, j int) bool { return p.reads[i] < p.reads[j] })
	sort.Slice(p.writes, func(i, j int) bool { return p.writes[i] < p.writes[j] })
}

func loopPositionsAfter(positions []token.Pos, after token.Pos) int {
	return len(positions) - sort.Search(len(positions), func(i int) bool { return positions[i] >= after })
}

func (p *loopAccessPositions) after(pos token.Pos, writer bool) int {
	n := loopPositionsAfter(p.writes, pos)
	if writer {
		n += loopPositionsAfter(p.reads, pos)
	}
	return n
}

type loopDirectUses struct {
	total     loopAccessPositions
	decisions map[ast.Node]*loopAccessPositions
	arms      map[*loopSiblingPath]*loopAccessPositions
}

// Direct accesses in an opposing conditional arm cannot conflict with this
// launch. Prefix indexes avoid rescanning every access for each launch, while
// preserving the conservative lexical ordering used for per-iteration storage.
func loopBodyPreludes(root ast.Node) map[ast.Node]token.Pos {
	positions := map[ast.Node]token.Pos{}
	if root == nil {
		return positions
	}
	mark := func(node ast.Node, pos token.Pos) {
		ast.Inspect(node, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			positions[n] = pos
			_, literal := n.(*ast.FuncLit)
			return !literal // constructing a literal does not execute its body
		})
	}
	ast.Inspect(root, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SwitchStmt:
			for _, stmt := range n.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					mark(expr, n.Pos())
				}
			}
		case *ast.SelectStmt:
			for _, stmt := range n.Body.List {
				switch comm := stmt.(*ast.CommClause).Comm.(type) {
				case *ast.AssignStmt:
					for _, rhs := range comm.Rhs {
						mark(rhs, n.Pos())
					}
				default:
					mark(comm, n.Pos())
				}
			}
		}
		return true
	})
	return positions
}

func loopDirectHazards(root ast.Node, launches []loopLaunch, accesses []loopDirectAccess) map[*ast.GoStmt]bool {
	hazards := map[*ast.GoStmt]bool{}
	if len(launches) == 0 {
		return hazards
	}
	paths := loopAlternativePaths(root)
	preludes := loopBodyPreludes(root)
	type key struct {
		variable *loopVariable
		owner    *ast.FuncLit
	}
	uses := map[key]*loopDirectUses{}
	for _, access := range accesses {
		if pos, ok := preludes[access.node]; ok {
			access.pos = pos
		}
		k := key{access.variable, access.owner}
		u := uses[k]
		if u == nil {
			u = &loopDirectUses{decisions: map[ast.Node]*loopAccessPositions{}, arms: map[*loopSiblingPath]*loopAccessPositions{}}
			uses[k] = u
		}
		u.total.add(access)
		for path := paths[access.node]; path != nil; path = path.parent {
			if u.decisions[path.decision] == nil {
				u.decisions[path.decision] = &loopAccessPositions{}
			}
			if u.arms[path] == nil {
				u.arms[path] = &loopAccessPositions{}
			}
			u.decisions[path.decision].add(access)
			u.arms[path].add(access)
		}
	}
	for _, u := range uses {
		u.total.sort()
		for _, positions := range u.decisions {
			positions.sort()
		}
		for _, positions := range u.arms {
			positions.sort()
		}
	}
	for _, launch := range launches {
		for variable, mode := range launch.values {
			if mode&(loopReadAccessMask|loopWrite|loopIndirectWrite) == 0 {
				continue
			}
			u := uses[key{variable, launch.owner}]
			if u == nil {
				continue
			}
			writer := mode&loopWrite != 0
			n := u.total.after(launch.stmt.End(), writer)
			for path := paths[launch.stmt]; path != nil; path = path.parent {
				if all := u.decisions[path.decision]; all != nil {
					n -= all.after(launch.stmt.End(), writer)
				}
				if own := u.arms[path]; own != nil {
					n += own.after(launch.stmt.End(), writer)
				}
			}
			if n > 0 {
				hazards[launch.stmt] = true
				break
			}
		}
	}
	return hazards
}
