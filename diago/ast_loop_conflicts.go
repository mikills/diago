package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// loopConflictHazards contains CFG proofs, not additional lexical guesses.
// When available, direct and siblings replace the lexical query results; copies
// replaces the region-global copyReachable test. Same-frame region-wide write
// or repeated-launch guesses must not override these results: a launch that
// exits cannot race with a different path's mutation. Cross-frame report checks
// remain separate. A copy reads the old SLOT, never writes its pointee.
type loopConflictHazards struct {
	direct, siblings, copies map[*ast.GoStmt]bool
	external                 map[*ast.GoStmt]map[*loopVariable]uint8
	loopCalls                map[*ast.GoStmt]bool
}

// One recorder is shared across recursively refined execution contexts.
// recordGraph retains original AST identities but allocates distinct CFG sites
// for repeated iterator expansions. Post-convergence replay supplies each site's
// launch payload and actual callee context. finishFlowConflicts uses these
// occurrence snapshots and reachable direct accesses, not source-level unions.
// Literal bodies are never indexed into their constructing frame.
type loopFlowConflicts struct {
	s      *loopScanner
	births map[ast.Node][]*loopVariable
	frames []*loopConflictFrame

	// Execution contexts are interned by their immutable entry bindings. A
	// construction frame is never substituted for an actual invocation.
	contexts map[*ast.BlockStmt]map[uint64][]*loopConflictFrame
	ids      map[*loopVariable]uint64
	callee   func(ast.Expr) *ast.FuncLit
	refined  bool
}

type loopConflictFrame struct {
	points     []loopConflictPoint
	nodes      map[ast.Node][]int
	gos        map[*ast.GoStmt]int
	copies     map[*loopVariable][]int
	births     map[*loopVariable][]int
	iterations map[*loopVariable][]int

	owner         *ast.FuncLit
	actual, root  bool
	seed          map[*loopVariable]loopValue
	sites         map[*cfg.Block][]*loopConflictSite
	recording     *loopConflictSite
	launches      []loopSyncLaunch
	calls         []loopContextCall
	constructors  []*loopConflictFrame
	execution     *loopDeferredInvocation
	delayed       map[*ast.DeferStmt][]loopDeferredPhysical
	registrations map[*ast.DeferStmt][]int
	resets        map[*ast.FuncLit][]int

	// Synthetic effects belong to one replayed execution occurrence, not to
	// every context sharing the same source AST node.
	effects map[int][]loopDirectAccess
}

// A source AST node may occur repeatedly in an expanded iterator graph. Sites,
// unlike nodes/gos, identify exactly one occurrence in one execution context.
type loopConflictSite struct {
	launch int
	calls  []loopContextCall
	nodes  map[ast.Node][]int
	after  int
}

type loopContextCall struct {
	call   *ast.CallExpr
	point  int
	target *loopConflictFrame
	guard  *ast.DeferStmt
}

type loopConflictPoint struct {
	next, prev []int
}

func (s *loopScanner) beginFlowConflicts() *loopFlowConflicts {
	c := &loopFlowConflicts{s: s, births: map[ast.Node][]*loopVariable{}}
	// The identity index intentionally omits a direct write for a new :=
	// variable, but records one for a same-scope redeclaration. Preserve that
	// distinction BEFORE reachable-use filtering removes unreachable accesses.
	redeclared := map[ast.Node]bool{}
	for _, access := range s.index.direct {
		if !access.effect && access.mode&loopWrite != 0 {
			redeclared[access.node] = true
		}
	}
	ast.Inspect(s.rootNode, func(node ast.Node) bool {
		var names []*ast.Ident
		switch n := node.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && !redeclared[id] {
						names = append(names, id)
					}
				}
			}
		case *ast.ValueSpec:
			names = n.Names
		}
		for _, id := range names {
			if v := s.index.ids[id]; v != nil {
				c.births[node] = append(c.births[node], v)
			}
		}
		return true
	})
	return c
}

func (g *loopConflictFrame) point() int {
	i := len(g.points)
	g.points = append(g.points, loopConflictPoint{})
	return i
}

// cutAfter is used only after a converged invocation proves it cannot return.
func (g *loopConflictFrame) cutAfter(point int) {
	for _, to := range g.points[point].next {
		prev := g.points[to].prev[:0]
		for _, from := range g.points[to].prev {
			if from != point {
				prev = append(prev, from)
			}
		}
		g.points[to].prev = prev
	}
	g.points[point].next = nil
}

func (g *loopConflictFrame) edge(from, to int) {
	g.points[from].next = append(g.points[from].next, to)
	g.points[to].prev = append(g.points[to].prev, from)
}

func (c *loopFlowConflicts) recordGraph(graph *cfg.CFG, adapter *loopFlowAST) *loopConflictFrame {
	if c == nil || graph == nil || len(graph.Blocks) == 0 {
		return nil
	}
	g := &loopConflictFrame{
		nodes: map[ast.Node][]int{}, gos: map[*ast.GoStmt]int{},
		sites: map[*cfg.Block][]*loopConflictSite{}, effects: map[int][]loopDirectAccess{},
		copies: map[*loopVariable][]int{}, births: map[*loopVariable][]int{}, iterations: map[*loopVariable][]int{},
	}
	// Derive reachability from edges, rather than trusting optional Live bits
	// in hand-built graphs or lexical positions of unreachable statements.
	reachable := map[*cfg.Block]bool{graph.Blocks[0]: true}
	queue := []*cfg.Block{graph.Blocks[0]}
	for head := 0; head < len(queue); head++ {
		for _, next := range queue[head].Succs {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}
	entry, exit := map[*cfg.Block]int{}, map[*cfg.Block]int{}
	for _, block := range graph.Blocks {
		if !reachable[block] {
			continue
		}
		last := g.point() // empty blocks still carry real CFG edges
		entry[block] = last
		for _, node := range block.Nodes {
			site := &loopConflictSite{launch: -1, nodes: map[ast.Node][]int{}}
			g.sites[block] = append(g.sites[block], site)
			g.recording = site
			p := g.point()
			g.edge(last, p)
			last = p
			last = c.recordSyncNode(g, node, p)
			site.after = last
			if stmt, ok := node.(*ast.DeferStmt); ok {
				if g.registrations == nil {
					g.registrations = map[*ast.DeferStmt][]int{}
				}
				g.registrations[stmt] = append(g.registrations[stmt], last)
			}
			if adapter != nil && adapter.deferred != nil {
				if marker, ok := adapter.deferred.markers[node]; ok && marker.entry {
					if g.resets == nil {
						g.resets = map[*ast.FuncLit][]int{}
					}
					g.resets[marker.owner] = append(g.resets[marker.owner], p)
				}
			}
			if adapter != nil {
				if region := adapter.copySteps[node]; region != nil {
					for v := range region.copies {
						g.copies[v] = append(g.copies[v], p)
					}
				}
				if rng := adapter.steps[node]; rng != nil && rng.stmt.Tok == token.DEFINE {
					for _, target := range []ast.Expr{rng.stmt.Key, rng.stmt.Value} {
						if id, ok := target.(*ast.Ident); ok {
							if v := c.s.index.ids[id]; v != nil {
								if v.birth == c.s.index.regions[rng.stmt] {
									g.births[v] = append(g.births[v], p)
								} else {
									g.iterations[v] = append(g.iterations[v], p)
								}
							}
						}
					}
				}
			}
			if stmt, ok := node.(*ast.GoStmt); ok {
				// Operands execute synchronously BEFORE launching. In particular,
				// their effects must not be mistaken for accesses after this go.
				launch := g.point()
				g.edge(last, launch)
				g.gos[stmt], last = launch, launch // compatibility index, not the event list
				site.launch = launch
			}
		}
		exit[block] = last
	}
	for _, block := range graph.Blocks {
		if reachable[block] {
			for _, next := range block.Succs {
				g.edge(exit[block], entry[next])
			}
		}
	}
	g.recording = nil
	c.frames = append(c.frames, g)
	return g
}

const (
	loopConflictDirectRead uint16 = 1 << iota // slot/header, never retained data
	loopConflictDirectWrite
	loopConflictDirectDataRead
	loopConflictDirectDataWrite
	loopConflictLaunchRead
	loopConflictLaunchWrite
	loopConflictLaunchDataRead
	loopConflictLaunchDataWrite
	loopConflictCopy

	loopConflictDirectMask = loopConflictDirectRead | loopConflictDirectWrite | loopConflictDirectDataRead | loopConflictDirectDataWrite
	loopConflictLaunchMask = loopConflictLaunchRead | loopConflictLaunchWrite | loopConflictLaunchDataRead | loopConflictLaunchDataWrite
)

func loopConflictMode(mode loopAccess, read, write, dataRead, dataWrite uint16) uint16 {
	var out uint16
	if mode&loopReadAccessMask != 0 {
		out |= read
	}
	if mode&loopWrite != 0 {
		out |= write
	}
	if mode&loopIndirectRead != 0 {
		out |= dataRead
	}
	if mode&loopIndirectWrite != 0 {
		out |= dataWrite
	}
	return out
}

// Keep only external operations that can conflict with this worker's actual
// access channel. In particular a data writer cannot race with a header copy.
// The four external bits have the same order as each graph access group.
func loopExternalConflicts(mode loopAccess, external uint8) uint8 {
	var out uint8
	if mode&(loopReadAccessMask|loopWrite) != 0 {
		out |= external & loopExternalWrite
	}
	if mode&loopWrite != 0 {
		out |= external & loopExternalRead
	}
	if mode&(loopIndirectRead|loopIndirectWrite) != 0 {
		out |= external & loopExternalDataWrite
	}
	if mode&loopIndirectWrite != 0 {
		out |= external & loopExternalDataRead
	}
	return out
}

type loopConflictVariable struct {
	direct   map[int]loopAccess
	launches map[int]loopAccess
	stmts    map[int]*ast.GoStmt
}

func (c *loopFlowConflicts) finishFlowConflicts() *loopConflictHazards {
	out := &loopConflictHazards{
		direct: map[*ast.GoStmt]bool{}, siblings: map[*ast.GoStmt]bool{}, copies: map[*ast.GoStmt]bool{},
		external: map[*ast.GoStmt]map[*loopVariable]uint8{},
	}
	if c == nil {
		return out
	}
	// Lexical identities are context-independent. Replayed synthetic effects
	// are not: their source-indexed union is only an abstract summary. Legacy
	// standalone graphs without replay still use that source index.
	accesses := map[ast.Node][]loopDirectAccess{}
	for _, access := range c.s.index.direct {
		if !access.summaryOnly && (!c.refined || !access.effect) {
			accesses[access.node] = append(accesses[access.node], access)
		}
	}
	escapes := c.projectSyncEscapes()
	out.loopCalls = escapes.looped
	escapes.forEachFrame(func(frame loopSyncEscapeFrame) {
		g := frame.graph
		variables := map[*loopVariable]*loopConflictVariable{}
		for _, launch := range frame.launches {
			stmt, point := launch.stmt, launch.point
			for v, mode := range launch.values {
				if mode&(loopReadAccessMask|loopWrite|loopIndirectWrite) == 0 {
					continue
				}
				u := variables[v]
				if u == nil {
					u = &loopConflictVariable{direct: map[int]loopAccess{}, launches: map[int]loopAccess{}, stmts: map[int]*ast.GoStmt{}}
					variables[v] = u
				}
				u.launches[point] = loopEpochMergeMode(u.launches[point], mode)
				u.stmts[point] = stmt
			}
		}
		for node, points := range g.nodes {
			for _, access := range accesses[node] {
				if u := variables[access.variable]; u != nil {
					mode := access.mode
					if g.execution != nil {
						mode = g.execution.referenceMode(access.variable, mode)
					}
					for _, point := range points {
						u.direct[point] = loopEpochMergeMode(u.direct[point], mode)
					}
				}
			}
		}
		for point, effects := range g.effects {
			for _, access := range effects {
				if !access.summaryOnly {
					if u := variables[access.variable]; u != nil {
						u.direct[point] = loopEpochMergeMode(u.direct[point], access.mode)
					}
				}
			}
		}
		for v, uses := range variables {
			for _, p := range g.iterations[v] {
				uses.direct[p] = loopEpochMergeMode(uses.direct[p], loopWrite)
			}
			g.conflicts(v, uses, out)
		}
	})
	escapes.inheritConflicts(out)
	return out
}

// conflicts solves finite bit-vector equations per captured variable. Each bit
// crosses an edge at most once per direction; there are no launch pairs, path
// enumerations, or source-order assumptions. A launch queries its successors
// (and predecessors for siblings), so its own event counts only on a CFG cycle.
// Birth/copy barriers limit both directions to the SAME allocation lifetime.
func (g *loopConflictFrame) conflicts(v *loopVariable, uses *loopConflictVariable, out *loopConflictHazards) {
	g.priorConflicts(v, uses, out)
	uses = loopEpochCurrentUses(uses)
	if out.external == nil {
		out.external = map[*ast.GoStmt]map[*loopVariable]uint8{}
	}
	writer := false
	for _, mode := range uses.launches {
		writer = writer || mode&(loopWrite|loopIndirectWrite) != 0
	}
	if !writer {
		for _, mode := range uses.direct {
			writer = writer || mode&(loopWrite|loopIndirectWrite) != 0
		}
	}
	// Ordinary read-only workers need no graph solve, however many sites.
	if !writer {
		return
	}
	local := make([]uint16, len(g.points))
	barrier := make([]bool, len(g.points))
	for _, p := range g.births[v] {
		barrier[p] = true
	}
	for _, p := range g.copies[v] {
		barrier[p] = true
		local[p] = loopConflictCopy
	}
	for p, mode := range uses.direct {
		if !barrier[p] { // a range := creates new storage, not a write to old storage
			local[p] |= loopConflictMode(mode, loopConflictDirectRead, loopConflictDirectWrite, loopConflictDirectDataRead, loopConflictDirectDataWrite)
		}
	}
	for p, mode := range uses.launches {
		local[p] |= loopConflictMode(mode, loopConflictLaunchRead, loopConflictLaunchWrite, loopConflictLaunchDataRead, loopConflictLaunchDataWrite)
	}
	future := g.propagate(local, barrier, false)
	pastLocal := make([]uint16, len(local))
	for p, mode := range uses.launches {
		pastLocal[p] = loopConflictMode(mode, loopConflictLaunchRead, loopConflictLaunchWrite, loopConflictLaunchDataRead, loopConflictLaunchDataWrite)
	}
	past := g.propagate(pastLocal, barrier, true)
	for p, mode := range uses.launches {
		var after, before uint16
		for _, next := range g.points[p].next {
			after |= future[next]
		}
		for _, prev := range g.points[p].prev {
			before |= past[prev]
		}
		stmt := uses.stmts[p]
		direct := loopExternalConflicts(mode, uint8(after&loopConflictDirectMask))
		siblings := loopExternalConflicts(mode, uint8(((after|before)&loopConflictLaunchMask)>>4))
		external := direct | siblings
		if direct != 0 {
			out.direct[stmt] = true
		}
		if siblings != 0 {
			out.siblings[stmt] = true
		}
		// Indirect writes read the pointer slot, but do not modify it. Copying
		// that slot cannot race with a write to independently allocated data.
		if mode&loopWrite != 0 && after&loopConflictCopy != 0 {
			out.copies[stmt] = true
			external |= loopExternalRead
		}
		if external != 0 {
			if out.external[stmt] == nil {
				out.external[stmt] = map[*loopVariable]uint8{}
			}
			out.external[stmt][v] |= external
		}
	}
}

// propagate computes local | union(neighbor) except at allocation barriers.
// Backward state is at point entry, forward state at point exit. Local copy
// markers remain visible backwards to launches before the copy, but cannot
// carry accesses from the new allocation into the old one's lifetime.
func (g *loopConflictFrame) propagate(local []uint16, barrier []bool, forward bool) []uint16 {
	state := append([]uint16(nil), local...)
	queue := make([]int, 0, len(local))
	queued := make([]bool, len(local))
	for p, bits := range state {
		if bits != 0 {
			queue = append(queue, p)
			queued[p] = true
		}
	}
	for head := 0; head < len(queue); head++ {
		p := queue[head]
		queued[p] = false
		edges := g.points[p].prev
		if forward {
			edges = g.points[p].next
		}
		for _, next := range edges {
			if barrier[next] {
				continue
			}
			bits := state[next] | state[p]
			if bits != state[next] {
				state[next] = bits
				if !queued[next] {
					queued[next] = true
					queue = append(queue, next)
				}
			}
		}
	}
	return state
}
