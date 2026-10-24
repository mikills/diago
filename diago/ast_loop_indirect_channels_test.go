package diago

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

var loopIndirectChannelCases = []struct {
	name, body string
	want       int
}{
	{"pointerHeaderCopy", `for range xs {v:=0; p:=&v; go func(){*p=1}(); _=p}`, 0},
	{"pointerHeaderLocalCopy", `for range xs {v:=0; p:=&v; go func(){*p=1}(); q:=p; _=q}`, 0},
	{"boundMethodHeaderCopy", `for range xs {p:=new(item); go func(){p.N=1}(); _=p.Use}`, 0},
	{"sliceHeaderCopy", `for range xs {s:=[]int{0}; go func(){s[0]=1}(); q:=s; _=q}`, 0},
	{"mapHeaderCopy", `for range xs {s:=map[int]int{0:0}; go func(){s[0]=1}(); q:=s; _=q}`, 0},
	{"pointerHeaderWrite", `for range xs {v:=0; p:=&v; go func(){*p=1}(); p=new(int); _=p}`, 1},
	{"sliceHeaderWrite", `for range xs {s:=[]int{0}; go func(){s[0]=1}(); s=nil; _=s}`, 1},
	{"mapHeaderWrite", `for range xs {s:=map[int]int{0:0}; go func(){s[0]=1}(); s=nil; _=s}`, 1},
	{"knownStorageWrite", `for range xs {v:=0; p:=&v; go func(){*p=1}(); _=v}`, 1},
	{"knownStorageRead", `for range xs {v:=0; p:=&v; go func(){_=*p}(); v=1}`, 1},
	{"pointerDataRead", `for range xs {p:=new(int); go func(){*p=1}(); _=*p}`, 1},
	{"pointerDataWrite", `for range xs {p:=new(int); go func(){_=*p}(); *p=1}`, 1},
	{"nestedPointerDataRead", `for range xs {pp:=new(*int); go func(){*pp=new(int)}(); _=**pp}`, 1},
	{"nestedIndexDataRead", `for range xs {s:=make([]*int,1); go func(){s[0]=new(int)}(); _=*s[0]}`, 1},
	{"nestedIndexOperandRead", `for range xs {s:=make([]*int,1); i:=0; go func(){_=*s[i]}(); i++}`, 1},
	{"pointerDataSiblings", `for range xs {p:=new(int); go func(){*p=1}(); go func(){_=*p}()}`, 2},
	{"pointerHeaderSibling", `for range xs {p:=new(int); go func(){*p=1}(); go func(){_=p}()}`, 0},
	{"sliceDataRead", `for range xs {s:=make([]int,1); go func(){s[0]=1}(); _=s[0]}`, 1},
	{"sliceDataWrite", `for range xs {s:=make([]int,1); go func(){_=s[0]}(); s[0]=1}`, 1},
	{"sliceDataSiblings", `for range xs {s:=make([]int,1); go func(){s[0]=1}(); go func(){_=s[0]}()}`, 2},
	{"mapDataRead", `for range xs {s:=make(map[int]int); go func(){s[0]=1}(); _=s[0]}`, 1},
	{"mapDataWrite", `for range xs {s:=make(map[int]int); go func(){_=s[0]}(); s[0]=1}`, 1},
	{"mapDataSiblings", `for range xs {s:=make(map[int]int); go func(){s[0]=1}(); go func(){_=s[0]}()}`, 2},
	{"pointerFieldDataRead", `for range xs {p:=new(item); go func(){p.N=1}(); _=p.N}`, 1},
	{"pointerFieldDataWrite", `for range xs {p:=new(item); go func(){_=p.N}(); p.N=1}`, 1},
	{"pointerFieldSiblings", `for range xs {p:=new(item); go func(){p.N=1}(); go func(){_=p.N}()}`, 2},
	{"pointerArrayDataRead", `for range xs {p:=new([1]int); go func(){p[0]=1}(); _=p[0]}`, 1},
	{"indexOperandRead", `for range xs {s:=make([]int,1); i:=0; go func(){_=s[i]}(); i++}`, 1},
	{"synchronousLiteralDataRead", `for range xs {s:=make([]int,1); f:=func(){_=s[0]}; go func(){s[0]=1}(); f()}`, 1},
	{"latentLiteralDataRead", `for range xs {s:=make([]int,1); f:=func(){_=s[0]}; go func(){s[0]=1}(); _=f}`, 0},
	{"synchronousWrapperDataRead", `for range xs {s:=make([]int,1); f:=func(){go func(){_=s[0]}()}; f(); s[0]=1}`, 1},
	{"delegatedDataWriterHeaderChild", `for range xs {s:=make([]int,1); go func(){s[0]=1; go func(){_=s}()}(); _=s[0]}`, 1},
	{"delegatedDataWriterDataChild", `for range xs {s:=make([]int,1); go func(){s[0]=1; go func(){s[0]=2}()}(); _=s[0]}`, 2},
	{"epochHeaderReader", `var f func(); for range xs {p:=new(int); if f!=nil {f()}; f=func(){_=p}; go func(){*p=1}()}`, 0},
	{"epochDataReader", `var f func(); for range xs {p:=new(int); if f!=nil {f()}; f=func(){_=*p}; go func(){*p=1}()}`, 1},
	{"epochDataWriterHeaderReader", `var f func(); for range xs {s:=make([]int,1); if f!=nil {f()}; f=func(){s[0]=1}; go func(){_=s}()}`, 0},
	{"epochDataWriterDataReader", `var f func(); for range xs {s:=make([]int,1); if f!=nil {f()}; f=func(){s[0]=1}; go func(){_=s[0]}()}`, 1},
	{"freshHeaderCopyOnly", `for p,i:=new(int),0; i<3; i++ {go func(){*p=1}()}`, 0},
}

func TestLoopIndirectChannels(t *testing.T) {
	for _, tc := range loopIndirectChannelCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, version := range []string{"1.21", "1.23"} {
				// A legacy for-header pointer is shared by successive workers;
				// this case specifically tests the fresh per-iteration copy.
				if tc.name == "freshHeaderCopyOnly" && version == "1.21" {
					continue
				}
				for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
					t.Run(version+"/"+mode, func(t *testing.T) {
						got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", version, mode)
						if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
							t.Fatalf("got %d findings, want %d: %+v", n, tc.want, got)
						}
					})
				}
			}
		})
	}
}

func TestLoopIndirectChannelsPublicCompleteInfo(t *testing.T) {
	for _, version := range []string{"1.21", "1.23"} {
		t.Run(version, func(t *testing.T) {
			var source strings.Builder
			source.WriteString(loopTestPrelude)
			for _, tc := range loopIndirectChannelCases {
				if tc.name == "freshHeaderCopyOnly" && version == "1.21" {
					continue
				}
				fmt.Fprintf(&source, "func %s(xs []int){%s}\n", tc.name, tc.body)
			}
			dir := writeTypeModule(t, map[string]string{
				"go.mod":      "module example.com/channels\n\ngo " + version + "\n",
				"channels.go": source.String(),
			})
			findings, err := AnalyzeAST(dir, "./...")
			if err != nil {
				t.Fatal(err)
			}
			byFunction := map[string]int{}
			for _, finding := range findings {
				if finding.Rule == "type-info-unavailable" {
					t.Fatalf("fixture requires complete type information: %+v", finding)
				}
				if finding.Rule == "goroutine-in-loop" {
					byFunction[finding.Symbol]++
				}
			}
			for _, tc := range loopIndirectChannelCases {
				if tc.name == "freshHeaderCopyOnly" && version == "1.21" {
					continue
				}
				if byFunction[tc.name] != tc.want {
					t.Errorf("%s: got %d findings, want %d", tc.name, byFunction[tc.name], tc.want)
				}
			}
		})
	}
}

func TestLoopIndirectChannelSolverMatrix(t *testing.T) {
	modes := []loopAccess{loopRetain, loopRead, loopHeaderRead, loopWrite, loopRead | loopIndirectRead, loopRead | loopIndirectWrite}
	for _, mode := range modes {
		for _, other := range modes {
			for _, sibling := range []bool{false, true} {
				t.Run(fmt.Sprintf("%x/%x/sibling=%v", mode, other, sibling), func(t *testing.T) {
					g := &loopConflictFrame{points: make([]loopConflictPoint, 2)}
					g.edge(0, 1)
					stmt := &ast.GoStmt{}
					uses := &loopConflictVariable{direct: map[int]loopAccess{}, launches: map[int]loopAccess{0: mode}, stmts: map[int]*ast.GoStmt{0: stmt}}
					if sibling {
						uses.launches[1] = other
						uses.stmts[1] = &ast.GoStmt{}
					} else {
						uses.direct[1] = other
					}
					out := &loopConflictHazards{direct: map[*ast.GoStmt]bool{}, siblings: map[*ast.GoStmt]bool{}, copies: map[*ast.GoStmt]bool{}}
					g.conflicts(&loopVariable{}, uses, out)
					slotAccess := loopReadAccessMask | loopWrite
					dataAccess := loopIndirectRead | loopIndirectWrite
					want := mode&loopWrite != 0 && other&slotAccess != 0 || other&loopWrite != 0 && mode&slotAccess != 0 ||
						mode&loopIndirectWrite != 0 && other&dataAccess != 0 || other&loopIndirectWrite != 0 && mode&dataAccess != 0
					got := out.direct[stmt]
					if sibling {
						got = out.siblings[stmt]
						if out.siblings[uses.stmts[1]] != want {
							t.Fatal("sibling conflict must be symmetric")
						}
					}
					if got != want {
						t.Fatalf("conflict=%v, want %v", got, want)
					}
				})
			}
		}
	}
}

func TestLoopIndirectChannelInheritance(t *testing.T) {
	v := &loopVariable{}
	lit := &ast.FuncLit{}
	parent := &ast.GoStmt{Call: &ast.CallExpr{Fun: lit}}
	header, data, slotWriter, dataWriter := &ast.GoStmt{}, &ast.GoStmt{}, &ast.GoStmt{}, &ast.GoStmt{}
	e := &loopSyncEscapes{
		callee: func(expr ast.Expr) *ast.FuncLit {
			if expr == lit {
				return lit
			}
			return nil
		},
		children: map[*ast.FuncLit]map[*ast.GoStmt]loopValue{lit: {
			header: {v: loopHeaderRead}, data: {v: loopRead | loopIndirectRead},
			slotWriter: {v: loopWrite}, dataWriter: {v: loopRead | loopIndirectWrite},
		}},
	}
	for _, stmt := range []*ast.GoStmt{header, data, slotWriter, dataWriter} {
		stmt.Call = &ast.CallExpr{}
	}
	h := &loopConflictHazards{direct: map[*ast.GoStmt]bool{}, external: map[*ast.GoStmt]map[*loopVariable]uint8{
		parent: {v: loopExternalRead | loopExternalWrite | loopExternalDataRead | loopExternalDataWrite},
	}}
	e.inheritConflicts(h)
	for child, want := range map[*ast.GoStmt]uint8{
		header:     loopExternalWrite,
		data:       loopExternalWrite | loopExternalDataWrite,
		slotWriter: loopExternalRead | loopExternalWrite,
		dataWriter: loopExternalWrite | loopExternalDataRead | loopExternalDataWrite,
	} {
		if !h.direct[child] || h.external[child][v] != want {
			t.Fatalf("inherited irrelevant channel: got %#x, want %#x", h.external[child][v], want)
		}
	}
}

func TestLoopIndirectChannelModeLatency(t *testing.T) {
	v := &loopVariable{}
	lit := &ast.FuncLit{}
	mode := loopRead | loopIndirectRead
	s := &loopScanner{index: &loopIndex{captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{
		lit: {v: loopRetain},
	}, direct: []loopDirectAccess{{owner: lit, variable: v, mode: mode}}}}
	if !s.updateCaptureEffects() || s.updateCaptureEffects() {
		t.Fatal("data-read capture must converge after exactly one growth step")
	}
	stored := s.value(lit, &loopWalk{})[v]
	if stored&loopCallableExecMask != 0 || stored != loopRetain|mode<<loopCallableShift {
		t.Fatalf("stored data read must be latent: %#x", stored)
	}
	for _, epoch := range []loopAccess{0, loopPriorRef, loopPriorRef | loopEpochMixedRef} {
		if got := loopActivateCallableMode(stored | epoch); got != loopRetain|mode|epoch {
			t.Fatalf("activation lost a channel or epoch: %#x", got)
		}
	}
}

func TestLoopIndirectChannelEpochQueries(t *testing.T) {
	for _, oldLaunch := range []bool{false, true} {
		for _, sibling := range []bool{false, true} {
			for _, read := range []loopAccess{loopRead, loopHeaderRead, loopRead | loopIndirectRead} {
				t.Run(fmt.Sprintf("old=%v/sibling=%v/read=%x", oldLaunch, sibling, read), func(t *testing.T) {
					v := &loopVariable{}
					g := &loopConflictFrame{points: make([]loopConflictPoint, 4), births: map[*loopVariable][]int{v: {2}}}
					g.edge(0, 1)
					g.edge(1, 2)
					g.edge(2, 3)
					launch, event := 1, 3
					launchMode := loopRead | loopIndirectWrite
					if oldLaunch {
						launch, event = 3, 1
						launchMode |= loopPriorRef
					} else {
						read |= loopPriorRef
					}
					stmt := &ast.GoStmt{}
					uses := &loopConflictVariable{direct: map[int]loopAccess{}, launches: map[int]loopAccess{launch: launchMode}, stmts: map[int]*ast.GoStmt{launch: stmt}}
					if sibling {
						uses.launches[event] = read
						uses.stmts[event] = &ast.GoStmt{}
					} else if !oldLaunch {
						uses.direct[event] = read
					} else {
						// Old-reference launches may meet later old effects, never
						// unrelated direct reads of current storage before birth.
						uses.direct[3] = read | loopPriorRef
						g.edge(3, 3)
					}
					out := &loopConflictHazards{direct: map[*ast.GoStmt]bool{}, siblings: map[*ast.GoStmt]bool{}, copies: map[*ast.GoStmt]bool{}}
					g.conflicts(v, uses, out)
					want := read&loopIndirectRead != 0
					got := out.direct[stmt]
					if sibling {
						got = out.siblings[stmt]
					}
					if got != want {
						t.Fatalf("conflict=%v, want %v; external=%v", got, want, out.external)
					}
				})
			}
		}
	}
}
