package diago

import (
	"go/ast"
	"testing"

	"golang.org/x/tools/go/cfg"
)

func TestLoopSelectIteratorOccurrenceMarkers(t *testing.T) {
	for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
		t.Run(mode, func(t *testing.T) {
			s := loopSyncTestScanner(t, `for range xs{outer:=func(yield func(int)bool){
				inner:=func(yield func(int)bool){yield(1);select{case x,ok:= <-make(chan int):use(x,ok);default:}}
				for range inner{if !yield(1){return}}
			};again:for range outer{break again}}`, "1.23", mode)
			a := &loopFlowAST{
				labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{},
				index: s.index, copySteps: map[ast.Node]*loopRegion{},
			}
			graph := cfg.New(a.block(s.rootNode.(*ast.BlockStmt)), func(*ast.CallExpr) bool { return true })
			a.expandIterators(graph)
			if a.iteratorIncomplete != nil {
				t.Fatal("nested iterator expansion incomplete")
			}
			births := s.beginFlowConflicts().births
			a.finishSelectCFG(graph, births)
			seen := map[ast.Node]bool{}
			occurrences := map[ast.Node]int{}
			for _, block := range graph.Blocks {
				for _, node := range block.Nodes {
					source := a.selectSources[node]
					if source == nil {
						continue
					}
					if seen[node] {
						t.Fatal("iterator copies share a synthetic select marker")
					}
					seen[node] = true
					occurrences[source]++
					if assignment, ok := node.(*ast.AssignStmt); ok {
						if len(births[node]) != 2 {
							t.Fatalf("receive assignment lost comma-ok declarations: %v", births[node])
						}
						original := source.(*ast.AssignStmt)
						for i, lhs := range assignment.Lhs {
							if lhs != original.Lhs[i] || s.index.ids[lhs.(*ast.Ident)] == nil {
								t.Fatal("selected receive lost original LHS identity")
							}
						}
					}
				}
			}
			duplicated := false
			for _, count := range occurrences {
				duplicated = duplicated || count > 1
			}
			if !duplicated {
				t.Fatal("fixture did not exercise repeated select occurrences")
			}
		})
	}
}
