package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
)

func loopSyncTestScanner(t testing.TB, body, version, mode string) *loopScanner {
	t.Helper()
	fset := token.NewFileSet()
	var flags parser.Mode
	if strings.Contains(mode, "lexical") {
		flags = parser.SkipObjectResolution
	}
	file, err := parser.ParseFile(fset, "sync.go", loopTestPrelude+"func work(xs []int,b bool){"+body+"}", flags)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{GoVersion: "go" + version}
	if _, err := conf.Check("example.com/sync", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "partial") {
		info.Defs, info.Selections = nil, nil
		for id := range info.Uses {
			if id.Name != "use" {
				delete(info.Uses, id)
			}
		}
	} else if mode != "typed" {
		info = nil
	}
	fn := file.Decls[len(file.Decls)-1].(*ast.FuncDecl)
	x := &loopIndex{ctx: loopContext{fset: fset, types: info, file: file, decl: fn, goVersion: version}, fresh: freshLoopVariables(version), ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{}, parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{}, regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{}}
	x.pushScope()
	x.fields(fn.Type.Params)
	for _, stmt := range fn.Body.List {
		x.node(stmt)
	}
	var findings []ASTFinding
	s := &loopScanner{index: x, rootNode: fn.Body, env: loopEnv{ctx: x.ctx, findings: &findings}}
	s.discover(fn.Body, &loopWalk{})
	s.refineLaunchBindings(fn.Body)
	return s
}

// Exercise the hooks independently of the parent's integration. The fixture
// type-checks EVERY mode before dropping/partially removing type info.
func loopSyncTestProof(t testing.TB, body, version, mode string) (*loopScanner, *loopConflictHazards) {
	t.Helper()
	s := loopSyncTestScanner(t, body, version, mode)
	c := s.beginFlowConflicts()
	record := func(body *ast.BlockStmt) {
		a := &loopFlowAST{labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}, index: s.index, copySteps: map[ast.Node]*loopRegion{}}
		graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
		g := &loopConflictFrame{nodes: map[ast.Node][]int{}, gos: map[*ast.GoStmt]int{}, copies: map[*loopVariable][]int{}, births: map[*loopVariable][]int{}, iterations: map[*loopVariable][]int{}}
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
		for _, block := range queue {
			last := g.point()
			entry[block] = last
			for _, node := range block.Nodes {
				p := g.point()
				g.edge(last, p)
				last = c.recordSyncNode(g, node, p)
				if region := a.copySteps[node]; region != nil {
					for v := range region.copies {
						g.copies[v] = append(g.copies[v], p)
					}
				}
				if rng := a.steps[node]; rng != nil && rng.stmt.Tok == token.DEFINE {
					for _, target := range []ast.Expr{rng.stmt.Key, rng.stmt.Value} {
						if id, ok := target.(*ast.Ident); ok {
							if v := s.index.ids[id]; v != nil {
								if v.birth == s.index.regions[rng.stmt] {
									g.births[v] = append(g.births[v], p)
								} else {
									g.iterations[v] = append(g.iterations[v], p)
								}
							}
						}
					}
				}
				if stmt, ok := node.(*ast.GoStmt); ok {
					launch := g.point()
					g.edge(last, launch)
					g.gos[stmt], last = launch, launch
				}
			}
			exit[block] = last
		}
		for _, block := range queue {
			for _, next := range block.Succs {
				g.edge(exit[block], entry[next])
			}
		}
		c.frames = append(c.frames, g)
	}
	record(s.rootNode.(*ast.BlockStmt))
	ast.Inspect(s.rootNode, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok && s.index.reachable[lit] {
			record(lit.Body)
		}
		return true
	})
	accesses := map[ast.Node][]loopDirectAccess{}
	for _, access := range s.index.direct {
		accesses[access.node] = append(accesses[access.node], access)
	}
	pointCounts := make([]int, len(c.frames))
	for i, g := range c.frames {
		pointCounts[i] = len(g.points)
	}
	var out *loopConflictHazards
	// Finishing twice also proves projection doesn't mutate recorder graphs.
	for pass := 0; pass < 2; pass++ {
		out = &loopConflictHazards{direct: map[*ast.GoStmt]bool{}, siblings: map[*ast.GoStmt]bool{}, copies: map[*ast.GoStmt]bool{}, external: map[*ast.GoStmt]map[*loopVariable]uint8{}}
		escapes := c.projectSyncEscapes()
		for _, frame := range escapes.frames {
			g := frame.graph
			variables := map[*loopVariable]*loopConflictVariable{}
			for _, launch := range frame.launches {
				for v, mode := range launch.values {
					if mode&(loopRead|loopWrite|loopIndirectWrite) == 0 {
						continue
					}
					u := variables[v]
					if u == nil {
						u = &loopConflictVariable{direct: map[int]loopAccess{}, launches: map[int]loopAccess{}, stmts: map[int]*ast.GoStmt{}}
						variables[v] = u
					}
					u.launches[launch.point] |= mode
					u.stmts[launch.point] = launch.stmt
				}
			}
			for node, points := range g.nodes {
				for _, access := range accesses[node] {
					if u := variables[access.variable]; u != nil {
						for _, p := range points {
							u.direct[p] |= access.mode
						}
					}
				}
			}
			for v, uses := range variables {
				for _, p := range g.iterations[v] {
					uses.direct[p] |= loopWrite
				}
				g.conflicts(v, uses, out)
			}
		}
		escapes.inheritConflicts(out)
		for i, g := range c.frames {
			if len(g.points) != pointCounts[i] {
				t.Fatal("projection mutated original graph")
			}
		}
	}
	return s, out
}

func TestLoopSyncEscapesProof(t *testing.T) {
	for _, tc := range []struct {
		name, body, direct, siblings, copies string
	}{
		{"reused range slot", `var v int;for _,v=range xs{func(){go func(){use(v)}()}()}`, "1", "0", "0"},
		{"owner reads after call", `for range xs{v:=0;func(){go func(){v++}()}();use(v)}`, "1", "0", "0"},
		{"owner writes after call", `for range xs{v:=0;func(){go func(){use(v)}()}();v++}`, "1", "0", "0"},
		{"parent access before call", `for range xs{v:=0;use(v);func(){go func(){v++}()}()}`, "0", "0", "0"},
		{"assignment store after call", `for range xs{v:=0;v=func()int{go func(){use(v)}();return 1}()}`, "1", "0", "0"},
		{"later operand reads after call", `for range xs{v:=0;use(func()int{go func(){v++}();return 0}(),v)}`, "1", "0", "0"},
		{"earlier operand reads before call", `for range xs{v:=0;use(v,func()int{go func(){v++}();return 0}())}`, "0", "0", "0"},
		{"copied parameter allocation", `var v int;for _,v=range xs{func(v int){go func(){use(v)}()}(v)}`, "0", "0", "0"},
		{"fresh range slot", `for _,v:=range xs{func(){go func(){use(v)}()}()}`, "0", "0", "0"},
		{"fresh exclusive writer", `for _,v:=range xs{func(){go func(){v++}()}()}`, "0", "0", "0"},
		{"callee local allocation", `for range xs{func(){v:=0;go func(){v++}()}()}`, "0", "0", "0"},
		{"callee internal conflict", `for range xs{func(){v:=0;go func(){v++}();use(v)}()}`, "1", "0", "0"},
		{"two synchronous frames", `var v int;for _,v=range xs{func(){func(){go func(){use(v)}()}()}()}`, "1", "0", "0"},
		{"unused definition", `var v int;for _,v=range xs{f:=func(){go func(){use(v)}()};_=f}`, "0", "0", "0"},
		{"deferred invocation is not sync", `var v int;for _,v=range xs{defer func(){go func(){use(v)}()}()}`, "0", "0", "0"},
		{"async outer invocation", `var v int;for _,v=range xs{go func(){go func(){use(v)}()}()}`, "11", "00", "00"},
		{"sync inside async", `var v int;for _,v=range xs{go func(){func(){go func(){use(v)}()}()}()}`, "11", "00", "00"},
		{"go argument is synchronous", `var v int;for _,v=range xs{go use(func()int{go func(){use(v)}();return 0}())}`, "01", "00", "00"},
		{"defer argument is synchronous", `var v int;for _,v=range xs{defer use(func()int{go func(){use(v)}();return 0}())}`, "1", "0", "0"},
		{"go function operand is synchronous", `var v int;for _,v=range xs{go func()func(){go func(){use(v)}();return func(){}}()()}`, "01", "00", "00"},
		{"conditional break prevents backedge", `var v int;for _,v=range xs{if b{func(){go func(){use(v)}()}();break}}`, "0", "0", "0"},
		{"different path mutation", `v:=0;for range xs{if b{func(){go func(){use(v)}()}();break};v++}`, "0", "0", "0"},
		{"goto skips call", `var v int;for _,v=range xs{goto done;func(){go func(){use(v)}()}();done:use()}`, "0", "0", "0"},
		{"unreachable child launch", `var v int;for _,v=range xs{func(){return;go func(){use(v)}()}()}`, "0", "0", "0"},
		{"unreachable nested invocation", `var v int;for _,v=range xs{func(){return;func(){go func(){use(v)}()}()}()}`, "0", "0", "0"},
		{"fresh header copied after call", `for i:=0;i<3;i++{func(){go func(){i++}()}()}`, "0", "0", "1"},
		{"break avoids fresh header copy", `for i:=0;i<3;i++{if b{func(){go func(){i++}()}();break}}`, "0", "0", "0"},
		{"continue reaches fresh header copy", `for i:=0;i<3;i++{if b{func(){go func(){i++}()}();continue}}`, "0", "0", "1"},
		{"indirect write does not write copied slot", `for p,n:=new(item),0;n<3;p,n=new(item),n+1{func(){go func(){p.N++}()}()}`, "0", "0", "0"},
		{"conditional children stay exclusive", `for _,v:=range xs{func(){if b{go func(){v++}()}else{go func(){use(v)}()}}()}`, "00", "00", "00"},
		{"child internal sibling conflicts", `for _,v:=range xs{func(){go func(){v++}();go func(){use(v)}()}()}`, "00", "11", "00"},
		{"caller sibling after call", `for _,v:=range xs{func(){go func(){v++}()}();go func(){use(v)}()}`, "00", "11", "00"},
		{"caller sibling before call", `for _,v:=range xs{go func(){use(v)}();func(){go func(){v++}()}()}`, "10", "11", "00"},
		{"same allocation on goto backedge", `for range xs{v:=0;again:func(){go func(){v++}()}();if b{goto again}}`, "1", "1", "0"},
		{"new allocation on goto backedge", `for range xs{again:v:=0;func(){go func(){v++}()}();if b{goto again}}`, "0", "0", "0"},
		{"immutable alias called twice", `for range xs{v:=0;f:=func(){go func(){v++}()};f();f()}`, "1", "1", "0"},
		{"immutable alias to alias", `var v int;for _,v=range xs{f:=func(){go func(){use(v)}()};g:=f;g()}`, "1", "0", "0"},
		{"immutable alias local lifetime", `f:=func(){v:=0;go func(){v++}()};for range xs{func(){f()}()}`, "0", "0", "0"},
		{"reassigned callee not inferred", `var v int;for _,v=range xs{f:=func(){go func(){use(v)}()};f=func(){};f()}`, "0", "0", "0"},
		{"address taken callee not inferred", `var v int;for _,v=range xs{f:=func(){go func(){use(v)}()};p:=&f;*p=func(){};f()}`, "0", "0", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s, got := loopSyncTestProof(t, tc.body, "1.22", mode)
					for _, check := range []struct {
						name, want string
						got        map[*ast.GoStmt]bool
					}{{"direct", tc.direct, got.direct}, {"siblings", tc.siblings, got.siblings}, {"copies", tc.copies, got.copies}} {
						var bits strings.Builder
						for _, launch := range s.launches {
							if check.got[launch.stmt] {
								bits.WriteByte('1')
							} else {
								bits.WriteByte('0')
							}
						}
						if bits.String() != check.want {
							t.Errorf("%s: got %s, want %s", check.name, bits.String(), check.want)
						}
					}
				})
			}
		})
	}
}

func TestLoopSyncEscapesPublicAnalyzeAST(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/syncescapes\n\ngo 1.22\n",
		"escapes.go": `package syncescapes
func use(...any) {}
func reused(xs []int) {var v int;for _,v=range xs{func(){go func(){use(v)}()}()}}
func after(xs []int) {for range xs{v:=0;func(){go func(){v++}()}();use(v)}}
func nested(xs []int) {var v int;for _,v=range xs{func(){func(){go func(){use(v)}()}()}()}}
func argument(xs []int) {var v int;for _,v=range xs{go use(func()int{go func(){use(v)}();return 0}())}}
func alias(xs []int) {var v int;for _,v=range xs{f:=func(){go func(){use(v)}()};f()}}
func safe(xs []int,b bool) {
 for _,v:=range xs{func(){go func(){use(v)}()}()}
 for range xs{func(){v:=0;go func(){v++}()}()}
 var v int;for _,v=range xs{if b{func(){go func(){use(v)}()}();break}}
 for _,v:=range xs{f:=func(){go func(){v++}()};_=f}
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	byFunction := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("typed fixture lost type info: %+v", findings)
		}
		if finding.Rule == "goroutine-in-loop" {
			byFunction[finding.Symbol]++
		}
	}
	for _, name := range []string{"reused", "after", "nested", "argument", "alias"} {
		if byFunction[name] != 1 {
			t.Errorf("%s findings=%d, want 1: %+v", name, byFunction[name], findings)
		}
	}
	if byFunction["safe"] != 0 {
		t.Errorf("safe findings=%d, want 0", byFunction["safe"])
	}
}

func TestLoopSyncEscapesManySites(t *testing.T) {
	if testing.Short() {
		t.Skip("large launch-site scale regression")
	}
	body := `for _,v:=range xs{` + strings.Repeat(`func(){go func(){v++}()}();`, 10000) + `}`
	s, hazards := loopSyncTestProof(t, body, "1.22", "lexical")
	if len(s.launches) != 10000 || len(hazards.siblings) != 10000 {
		t.Fatalf("launches=%d siblings=%d, want 10000 each", len(s.launches), len(hazards.siblings))
	}
}
