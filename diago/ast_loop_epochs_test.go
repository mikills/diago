package diago

import (
	"go/ast"
	"go/token"
	"testing"
)

func TestLoopEpochReferenceModes(t *testing.T) {
	latent := loopRetain | loopRead<<loopCallableShift | loopWrite<<loopCallableShift
	prior := latent | loopPriorRef
	mixed := loopEpochMergeMode(prior, latent)
	if mixed != prior|loopEpochMixedRef {
		t.Fatalf("current/prior join lost a lifetime: %#x", mixed)
	}
	for _, tc := range []struct {
		mode, current, old loopAccess
	}{
		{latent, latent, 0}, {prior, 0, latent}, {mixed, latent, latent}, {0, 0, 0},
	} {
		if got := loopEpochCurrentMode(tc.mode); got != tc.current {
			t.Errorf("current(%#x) = %#x, want %#x", tc.mode, got, tc.current)
		}
		if got := loopEpochPriorMode(tc.mode); got != tc.old {
			t.Errorf("prior(%#x) = %#x, want %#x", tc.mode, got, tc.old)
		}
	}
	if loopEpochMergeMode(prior, prior) != prior || loopEpochMergeMode(prior, 0) != prior || loopEpochMergeMode(0, prior) != prior {
		t.Fatal("joining only prior references must not invent a current reference")
	}
	if mixed&loopCallableExecMask != 0 {
		t.Fatal("reference metadata activated a latent callable")
	}
}

func TestLoopEpochJoinMetadata(t *testing.T) {
	v, p := &loopVariable{}, &loopVariable{}
	for _, reverse := range []bool{false, true} {
		current, prior := loopValue{v: loopRead}, loopValue{v: loopRead | loopPriorRef}
		first, next := prior, current
		if reverse {
			first, next = current, prior
		}
		state := map[*loopVariable]loopValue{p: first}
		if !loopJoinBindings(state, map[*loopVariable]loopValue{p: next}) {
			t.Fatal("current/prior join was incorrectly treated as a mode subset")
		}
		if state[p][v] != loopRead|loopPriorRef|loopEpochMixedRef {
			t.Fatalf("join lost a lifetime: %#x", state[p][v])
		}
		if first[v]&loopEpochMixedRef != 0 || next[v]&loopEpochMixedRef != 0 {
			t.Fatal("join mutated an incoming snapshot")
		}
		if loopJoinBindings(state, map[*loopVariable]loopValue{p: next}) {
			t.Fatal("metadata join is not idempotent")
		}
	}
}

func TestLoopEpochBirthsPreserveSnapshots(t *testing.T) {
	v, p, slot, other := &loopVariable{}, &loopVariable{}, &loopVariable{}, &loopVariable{}
	latent := loopRetain | loopWrite<<loopCallableShift
	old := loopValue{v: latent, other: loopRead}
	address := loopValue{v: loopRead}
	node := &ast.AssignStmt{}
	f := &loopFlow{
		aliases:   map[*loopVariable]*loopVariable{p: slot},
		conflicts: &loopFlowConflicts{births: map[ast.Node][]*loopVariable{node: {v}}},
	}
	w := &loopWalk{values: map[*loopVariable]loopValue{p: old, slot: address}}
	f.advanceLoopEpoch(node, w, nil)
	if old[v] != latent || address[v] != loopRead {
		t.Fatal("aging mutated an earlier closure/alias snapshot")
	}
	if w.values[p][v] != latent|loopPriorRef || w.values[slot][v] != loopRead|loopPriorRef {
		t.Fatalf("lost retained reference provenance: %v", w.values)
	}
	if w.values[p][other] != loopRead || w.values[p][v]&loopCallableExecMask != 0 {
		t.Fatal("aging changed unrelated references or activated a callable")
	}
	pruned := loopPruneBindings(w.values, map[*loopVariable]bool{p: true}, f.aliases)
	if pruned[slot][v]&loopPriorRef == 0 || pruned[p][v]&loopPriorRef == 0 {
		t.Fatal("liveness pruning discarded reference provenance")
	}
	joined := map[*loopVariable]loopValue{}
	loopJoinBindings(joined, pruned)
	if joined[p][v] != latent|loopPriorRef {
		t.Fatal("joining into an empty state discarded metadata")
	}
	mixed := loopValue{v: loopRead | loopPriorRef | loopEpochMixedRef}
	aged := loopEpochAgeValue(mixed, map[*loopVariable]bool{v: true})
	if aged[v] != loopRead|loopPriorRef || mixed[v]&loopEpochMixedRef == 0 {
		t.Fatal("birth must age both alternatives without mutating the join")
	}
}

func TestLoopEpochRangeAndHeaderMarkers(t *testing.T) {
	for _, header := range []bool{false, true} {
		v, p, slot := &loopVariable{}, &loopVariable{}, &loopVariable{}
		id := ast.NewIdent("v")
		rng := &ast.RangeStmt{Tok: token.DEFINE, Value: id}
		region := &loopRegion{node: rng, copies: map[*loopVariable]bool{v: true}}
		v.birth = region
		marker := &ast.EmptyStmt{}
		a := &loopFlowAST{steps: map[ast.Node]*loopFlowRange{}, copySteps: map[ast.Node]*loopRegion{}}
		if header {
			a.copySteps[marker] = region
		} else {
			a.steps[marker] = &loopFlowRange{stmt: rng}
		}
		f := &loopFlow{
			s:       &loopScanner{index: &loopIndex{ids: map[*ast.Ident]*loopVariable{id: v}, regions: map[ast.Node]*loopRegion{rng: region}}},
			aliases: map[*loopVariable]*loopVariable{p: slot},
		}
		w := &loopWalk{values: map[*loopVariable]loopValue{p: {v: loopRead}, slot: {v: loopRead}}}
		f.advanceLoopEpoch(marker, w, a)
		if w.values[slot][v] != loopRead|loopPriorRef {
			t.Fatalf("header=%v: marker did not age the address", header)
		}
		fresh := f.addressAliases(&ast.UnaryExpr{Op: token.AND, X: id}, w)
		if fresh[v] != loopRead {
			t.Fatalf("header=%v: a new &v inherited the old lifetime", header)
		}
	}
}

func TestLoopEpochConflictProofs(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		births, copies          []int
		launches, direct        map[int]loopAccess
		wantDirect, wantSibling map[int]bool
	}{
		{"old write crosses birth", []int{2}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopWrite | loopPriorRef}, map[int]bool{1: true}, nil},
		{"old write crosses copy", nil, []int{2}, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopWrite | loopPriorRef}, map[int]bool{1: true}, nil},
		{"new write stays new", []int{2}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopWrite}, nil, nil},
		{"old read does not expose new write", []int{2}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopRead | loopPriorRef, 4: loopWrite}, nil, nil},
		{"old write before birth stays old", []int{4}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopWrite | loopPriorRef}, nil, nil},
		{"old worker new mutation stays clean", []int{0}, nil, map[int]loopAccess{1: loopRead | loopPriorRef}, map[int]loopAccess{3: loopWrite}, nil, nil},
		{"new worker old mutation and exit", []int{0}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{3: loopWrite | loopPriorRef}, nil, nil},
		{"prior workers may share old storage", []int{2}, nil, map[int]loopAccess{1: loopRead | loopPriorRef, 3: loopWrite | loopPriorRef}, nil, nil, map[int]bool{1: true, 3: true}},
		{"current and prior siblings cross birth", []int{2}, nil, map[int]loopAccess{1: loopRead, 3: loopWrite | loopPriorRef}, nil, nil, map[int]bool{1: true, 3: true}},
		{"current and prior siblings without birth", []int{0}, nil, map[int]loopAccess{1: loopRead, 3: loopWrite | loopPriorRef}, nil, nil, nil},
		{"prior writer and prior direct reader", nil, nil, map[int]loopAccess{1: loopWrite | loopPriorRef}, map[int]loopAccess{3: loopRead | loopPriorRef}, map[int]bool{1: true}, nil},
		{"mixed worker retains current possibility", []int{0}, nil, map[int]loopAccess{1: loopRead | loopPriorRef | loopEpochMixedRef}, map[int]loopAccess{3: loopWrite}, map[int]bool{1: true}, nil},
		{"old effect on birth point", []int{2}, nil, map[int]loopAccess{1: loopRead}, map[int]loopAccess{2: loopWrite | loopPriorRef}, map[int]bool{1: true}, nil},
		{"header read crosses birth", []int{2}, nil, map[int]loopAccess{1: loopHeaderRead}, map[int]loopAccess{3: loopWrite | loopPriorRef}, map[int]bool{1: true}, nil},
		{"prior header read new write stays clean", []int{0}, nil, map[int]loopAccess{1: loopHeaderRead | loopPriorRef}, map[int]loopAccess{3: loopWrite}, nil, nil},
		{"prior writer and prior header reader", nil, nil, map[int]loopAccess{1: loopWrite | loopPriorRef}, map[int]loopAccess{3: loopHeaderRead | loopPriorRef}, map[int]bool{1: true}, nil},
		{"mixed header read retains current possibility", []int{0}, nil, map[int]loopAccess{1: loopHeaderRead | loopPriorRef | loopEpochMixedRef}, map[int]loopAccess{3: loopWrite}, map[int]bool{1: true}, nil},
		{"header sibling crosses birth", []int{2}, nil, map[int]loopAccess{1: loopHeaderRead, 3: loopWrite | loopPriorRef}, nil, nil, map[int]bool{1: true, 3: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := &loopVariable{}
			g := &loopConflictFrame{births: map[*loopVariable][]int{v: tc.births}, copies: map[*loopVariable][]int{v: tc.copies}}
			for i := 0; i < 6; i++ {
				g.point()
				if i != 0 {
					g.edge(i-1, i)
				}
			}
			uses := &loopConflictVariable{direct: tc.direct, launches: tc.launches, stmts: map[int]*ast.GoStmt{}}
			for p := range tc.launches {
				uses.stmts[p] = &ast.GoStmt{}
			}
			out := &loopConflictHazards{direct: map[*ast.GoStmt]bool{}, siblings: map[*ast.GoStmt]bool{}, copies: map[*ast.GoStmt]bool{}}
			// Explicit composition also exercises the helper before parent wiring.
			g.priorConflicts(v, uses, out)
			g.conflicts(v, loopEpochCurrentUses(uses), out)
			for p, stmt := range uses.stmts {
				if out.direct[stmt] != tc.wantDirect[p] || out.siblings[stmt] != tc.wantSibling[p] {
					t.Errorf("launch %d: direct/siblings = %v/%v, want %v/%v", p, out.direct[stmt], out.siblings[stmt], tc.wantDirect[p], tc.wantSibling[p])
				}
			}
		})
	}
}

func TestLoopEpochRetainedLocalRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"range previous", `var previous *int; for _,v:=range xs {if previous!=nil {*previous=1}; previous=&v; go func(){use(v)}()}`, 1},
		{"initialized previous", `outer:=0; previous:=&outer; for _,v:=range xs {*previous=1; previous=&v; go func(){use(v)}()}`, 1},
		{"conditional retained address", `var previous *int; for _,v:=range xs {if previous!=nil {*previous=1}; if b {previous=&v}; go func(){use(v)}()}`, 1},
		{"copied retained address", `var previous *int; for _,v:=range xs {q:=previous; if q!=nil {*q=1}; previous=&v; go func(){use(v)}()}`, 1},
		{"retained across loops", `var previous *int; for _,v:=range xs {previous=&v; go func(){use(v)}()}; for range xs {if previous!=nil {*previous=1}}`, 1},
		{"old callable invoked", `var previous func(); for _,v:=range xs {if previous!=nil {previous()}; previous=func(){v++}; go func(){use(v)}()}`, 1},
		{"body declaration", `var previous *int; for range xs {v:=0; if previous!=nil {*previous=1}; previous=&v; go func(){use(v)}()}`, 1},
		{"body var declaration", `var previous *int; for range xs {var v int; if previous!=nil {*previous=1}; previous=&v; go func(){use(v)}()}`, 1},
		{"header copy", `var previous *int; for v:=0;v<len(xs);v++ {if previous!=nil {*previous=1}; previous=&v; go func(){use(v)}()}`, 1},
		{"local array address", `var previous *int; for range xs {var a [1]int; if previous!=nil {*previous=1}; previous=&a[0]; go func(){use(a[0])}()}`, 1},
		{"local struct address", `var previous *int; for range xs {var a struct{N int}; if previous!=nil {*previous=1}; previous=&a.N; go func(){use(a.N)}()}`, 1},
		{"old array pointer", `var previous *[1]int; for range xs {var a [1]int; if previous!=nil {previous[0]=1}; previous=&a; go func(){use(a[0])}()}`, 1},
		{"old struct pointer", `var previous *struct{N int}; for range xs {var a struct{N int}; if previous!=nil {previous.N=1}; previous=&a; go func(){use(a.N)}()}`, 1},
		{"old read current worker writes", `var previous *int; for _,v:=range xs {if previous!=nil {use(*previous)}; previous=&v; go func(){v++}()}`, 1},
		{"direct old pointer argument", `var previous *int; for _,v:=range xs {if previous!=nil {go use(previous)}; previous=&v; go func(){v++}()}`, 2},
		{"fresh range writers", `for _,v:=range xs {go func(){v++}()}`, 0},
		{"old callable remains latent", `var previous func(); for _,v:=range xs {if previous!=nil {_=previous}; previous=func(){v++}; go func(){use(v)}()}`, 0},
		{"launch path exits", `var previous *int; for _,v:=range xs {if b {go func(){use(v)}(); break}; if previous!=nil {*previous=1}; previous=&v}`, 0},
		{"fresh body writers", `for range xs {v:=0; go func(){v++}()}`, 0},
		{"fresh header reader", `for v:=0;v<len(xs);v++ {go func(){use(v)}()}`, 0},
		{"unrelated old storage", `outer:=0; previous:=&outer; for _,v:=range xs {*previous=1; go func(){use(v)}()}`, 0},
		{"new mutation not old reference", `var previous *int; for _,v:=range xs {v++; previous=&v; go func(){use(v)}()}; _=previous`, 0},
		{"old reference only retained", `var previous *int; for _,v:=range xs {if previous!=nil {_=previous}; previous=&v; go func(){v++}()}`, 0},
		{"old reference read new variable written", `var previous *int; for _,v:=range xs {if previous!=nil {use(*previous)}; v++; previous=&v; go func(){use(v)}()}`, 0},
		{"overwrite before old access", `var previous *int; for _,v:=range xs {previous=&v; *previous=1; go func(){use(v)}()}`, 0},
		{"unrelated fresh body write", `var previous *int; for _,v:=range xs {fresh:=0; fresh++; if previous!=nil {use(*previous)}; previous=&v; go func(){use(v)}(); use(fresh)}`, 0},
		{"exit before next birth", `outer:=0; previous:=&outer; for _,v:=range xs {go func(){use(v)}(); *previous=1; previous=&v; break}; _=previous`, 0},
		{"join keeps current alias", `var previous *int; for _,v:=range xs {p:=previous; if b {p=&v}; if p!=nil {go use(p)}; v++; previous=&v}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+`func work(xs []int,b bool){`+tc.body+`}`, "1.22", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopEpochPublicAnalyzeAST(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/epochs\n\ngo 1.22\n",
		"epochs.go": `package epochs
func use(...any) {}
func retained(xs []int) {
 var previous *int
 for _,v:=range xs {
  if previous!=nil {*previous=1}
  previous=&v
  go func(){use(v)}()
 }
}
func fresh(xs []int) {for _,v:=range xs {go func(){v++}()}}
func unrelated(xs []int) {
 var previous *int
 for _,v:=range xs {
  fresh:=0; fresh++
  if previous!=nil {use(*previous)}
  previous=&v
  go func(){use(v)}()
  use(fresh)
 }
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if counts := countRules(findings); counts["type-info-unavailable"] != 0 || counts["goroutine-in-loop"] != 1 {
		t.Fatalf("expected exactly the retained-storage finding: %+v", findings)
	}
	for _, finding := range findings {
		if finding.Rule == "goroutine-in-loop" && finding.Symbol != "retained" {
			t.Fatalf("fresh/unrelated storage was reported: %+v", finding)
		}
	}
}
