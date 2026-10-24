package diago

import "go/ast"

func (s *loopScanner) recordFlowReachability(node ast.Node) {
	if node == nil {
		return
	}
	if s.index.reachable == nil {
		s.index.reachable = map[ast.Node]bool{}
	}
	s.inspectExecuted(node, func(node ast.Node) bool {
		s.index.reachable[node] = true
		return true // literal bodies have their own reachable CFG replay
	})
}

func (s *loopScanner) filterFlowReachability() {
	direct := s.index.direct[:0]
	for _, access := range s.index.direct {
		if access.effect || s.index.reachable[access.node] {
			direct = append(direct, access)
		}
	}
	s.index.direct = direct
}
