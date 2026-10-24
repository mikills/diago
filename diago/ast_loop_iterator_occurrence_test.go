package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
)

var loopIteratorOccurrenceCases = []struct {
	name, body string
	want       int
}{
	{"repeated nested post yield", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}()}`, 1},
	{"repeated nested pre yield only", `v:=0;leaf:=func(y func()bool){v++;y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}()}`, 0},
	{"second nested break cleanup", `v:=0;leaf:=func(y func()bool){if !y(){v++;return}};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}();break}`, 1},
	{"second nested break prunes true", `v:=0;leaf:=func(y func()bool){if y(){v++}};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}();break}`, 0},
	{"second nested continue prunes false", `v:=0;leaf:=func(y func()bool){if !y(){v++;return}};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}();continue}`, 0},
	{"second nested return cleanup", `v:=0;leaf:=func(y func()bool){if !y(){v++;return}};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}();return}`, 1},
	{"second nested return prunes true", `v:=0;leaf:=func(y func()bool){if !y(){return};v++};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};for range mid{go func(){use(v)}();return}`, 0},
	{"nested range in false continuation", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){if !y(){for range leaf{};return}};for range mid{};for range mid{go func(){use(v)}();break}`, 1},
	{"nested range in true continuation", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){if y(){for range leaf{}}};for range mid{};for range mid{go func(){use(v)}();break}`, 0},
	{"nested range in both continuation copies", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){y();for range leaf{}};for range mid{};for range mid{go func(){use(v)}();break}`, 1},
	{"nested first go current binding", `for range xs{v:=0;f:=func(){use(v)};leaf:=func(y func()bool){go f();y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};v++;f=func(){};for range mid{}}`, 1},
	{"nested second go after write", `for range xs{v:=0;f:=func(){};leaf:=func(y func()bool){go f();y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};v++;f=func(){use(v)};for range mid{}}`, 0},
	{"nested second go before write", `for range xs{v:=0;f:=func(){};leaf:=func(y func()bool){go f();y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};f=func(){use(v)};for range mid{};v++}`, 1},
	{"nested local strong overwrite", `for range xs{v:=0;f:=func(){use(v)};leaf:=func(y func()bool){h:=f;h=func(){};go h();y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};v++;for range mid{}}`, 0},
	{"nested captured strong overwrite", `v:=0;f:=func(){use(v)};leaf:=func(y func()bool){f=func(){};y();v++};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};f=func(){use(v)};for range mid{go f()}`, 0},
	{"second forwarded snapshot overwrites capture", `v:=0;f:=func(){use(v)};leaf:=func(y func(func())bool){y(f)};mid:=func(y func(func())bool){for h:=range leaf{if !y(h){return}}};for range mid{};f=func(){};for h:=range mid{go h()};v++`, 0},
	{"second forwarded snapshot captures", `v:=0;f:=func(){};leaf:=func(y func(func())bool){y(f)};mid:=func(y func(func())bool){for h:=range leaf{if !y(h){return}}};for range mid{};f=func(){use(v)};for h:=range mid{go h()};v++`, 1},
	{"forwarded key does not inherit value or prior call", `v:=0;f:=func(){use(v)};g:=func(){};leaf:=func(y func(func(),func())bool){y(f,g)};mid:=func(y func(func(),func())bool){for k,h:=range leaf{if !y(k,h){return}}};for range mid{};f=func(){};g=func(){use(v)};for h,_:=range mid{go h()};v++`, 0},
	{"forwarded value uses second call binding", `v:=0;f:=func(){use(v)};g:=func(){};leaf:=func(y func(func(),func())bool){y(f,g)};mid:=func(y func(func(),func())bool){for k,h:=range leaf{if !y(k,h){return}}};for range mid{};f=func(){};g=func(){use(v)};for _,h:=range mid{go h()};v++`, 1},
	{"repeated callback overwrite cannot poison snapshots", `v:=0;f:=func(){};leaf:=func(y func(func())bool){if !y(f){return};y(f)};mid:=func(y func(func())bool){for h:=range leaf{if !y(h){return}}};for range mid{};for h:=range mid{go h();h=func(){use(v)};_=h};v++`, 0},
	{"nested false-only header remains unreachable", `v:=0;leaf:=func(y func()bool){y();v++};mid:=func(y func()bool){if y(){return};for range leaf{}};for range mid{};for range mid{go func(){use(v)}()}`, 0},
	{"second pointer effect overwrites capture", `for range xs{v,other:=0,0;p:=&v;leaf:=func(y func()bool){*p=1;y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};p=&other;go func(){use(v)}();for range mid{}}`, 0},
	{"second pointer effect current binding", `for range xs{v,other:=0,0;p:=&other;leaf:=func(y func()bool){*p=1;y()};mid:=func(y func()bool){for range leaf{if !y(){return}}};for range mid{};p=&v;go func(){use(v)}();for range mid{}}`, 1},
}

func TestLoopIteratorOccurrences(t *testing.T) {
	for _, tc := range loopIteratorOccurrenceCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					s := loopSyncTestScanner(t, tc.body, "1.23", mode)
					s.report()
					counts := countRules(*s.env.findings)
					if counts["goroutine-in-loop"] != tc.want || counts["iterator-analysis-incomplete"] != 0 {
						t.Fatalf("got %+v, want %d exact goroutine findings", *s.env.findings, tc.want)
					}
				})
			}
		})
	}
}

func TestLoopIteratorOccurrencesPublicGo123(t *testing.T) {
	var source strings.Builder
	source.WriteString("package occurrences\nfunc use(...any){}\n")
	for i, tc := range loopIteratorOccurrenceCases {
		fmt.Fprintf(&source, "func case%d(xs []int){%s}\n", i, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":         "module example.com/occurrences\n\ngo 1.23\n",
		"occurrences.go": source.String(),
	})
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Go 1.23 compile: %v\n%s", err, out)
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	bySymbol := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" || finding.Rule == "iterator-analysis-incomplete" {
			t.Fatalf("expected complete typed analysis: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			bySymbol[finding.Symbol]++
		}
	}
	for i, tc := range loopIteratorOccurrenceCases {
		if got := bySymbol[fmt.Sprintf("case%d", i)]; got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func loopIteratorDAGBody(depth int) string {
	var body strings.Builder
	body.WriteString("v:=0;iter0:=func(y func()bool){y()};")
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&body, "iter%d:=func(y func()bool){for range iter%d{if !y(){return}};for range iter%d{if !y(){return}}};", i, i-1, i-1)
	}
	fmt.Fprintf(&body, "for range iter%d{go func(){v++}()}", depth)
	return body.String()
}

// Build the private analysis CFG without asking the compiler optimizer to
// inline the runtime DAG. Even syntax-only benchmarks type-check the source.
func loopIteratorOccurrenceGraph(t testing.TB, body string) (*loopFlowAST, *cfg.CFG) {
	t.Helper()
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "occurrences.go", "package p\nfunc work(){"+body+"}", parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{GoVersion: "go1.23"}
	if _, err := conf.Check("example.com/p", fs, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	x := &loopIndex{
		ctx: loopContext{fset: fs, types: info, file: file, decl: fn, goVersion: "1.23"}, fresh: true,
		ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{}, parsed: map[*ast.Object]*loopVariable{},
		external: map[string]*loopVariable{}, regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	x.node(fn.Body)
	a := &loopFlowAST{index: x, labels: map[string]*ast.Object{}, entries: map[ast.Node]*loopFlowRange{}, steps: map[ast.Node]*loopFlowRange{}, copySteps: map[ast.Node]*loopRegion{}}
	return a, cfg.New(a.block(fn.Body), func(*ast.CallExpr) bool { return true })
}

func TestLoopIteratorOccurrenceMetadata(t *testing.T) {
	a, graph := loopIteratorOccurrenceGraph(t, `leaf:=func(y func(int,int)bool){y(1,2)};mid:=func(y func()bool){y();for k,v:=range leaf{_,_=k,v}};for range mid{};for range mid{break}`)
	ids := map[*ast.Ident]*loopVariable{}
	for id, v := range a.index.ids {
		ids[id] = v
	}
	a.expandIterators(graph)
	if a.iteratorIncomplete != nil {
		t.Fatal("normal repeated invocation exceeded budget")
	}
	for id, v := range ids {
		if a.index.ids[id] != v {
			t.Fatal("expansion rebound an original AST identifier")
		}
	}
	entries := map[ast.Node]bool{}
	slots := map[*loopVariable]bool{}
	counts := map[*ast.RangeStmt]int{}
	for _, it := range a.iterators {
		if entries[it.rng.entry] || slots[it.rng.key] || slots[it.rng.value] {
			t.Fatal("iterator occurrences share an entry or snapshot slot")
		}
		entries[it.rng.entry], slots[it.rng.key], slots[it.rng.value] = true, true, true
		counts[it.rng.stmt]++
		if !a.iteratorEntries[it.rng.entry] || a.entries[it.rng.entry] != it.rng || len(it.entry.Succs) != 1 || len(it.entry.Succs[0].Succs) != 1 {
			t.Fatal("physical iterator entry was not expanded exactly once")
		}
		wrapper, ok := it.rng.entry.(*ast.ParenExpr)
		if !ok || wrapper.X != it.rng.stmt.X || a.index.ctx.types.TypeOf(wrapper.X) == nil {
			t.Fatal("occurrence changed original expression/type-info identity")
		}
	}
	max := 0
	for _, n := range counts {
		if n > max {
			max = n
		}
	}
	if max < 3 {
		t.Fatalf("did not exercise repeated true/false copies: %v", counts)
	}
}

func TestLoopIteratorDAGExpansionBudget(t *testing.T) {
	for _, depth := range []int{2, 4, 6, 8, 10} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			a, graph := loopIteratorOccurrenceGraph(t, loopIteratorDAGBody(depth))
			initial := len(graph.Blocks)
			a.expandIterators(graph)
			if added := len(graph.Blocks) - initial; added > loopIteratorBlockBudget {
				t.Fatalf("unbounded graph: added %d blocks", added)
			}
			if depth == 2 && a.iteratorIncomplete != nil {
				t.Fatal("small DAG must be exact")
			}
			if depth == 10 && a.iteratorIncomplete == nil {
				t.Fatal("large compact DAG must explicitly report incomplete expansion")
			}
			t.Logf("depth=%d blocks=%d plans=%d incomplete=%v", depth, len(graph.Blocks), len(a.iterators), a.iteratorIncomplete != nil)
		})
	}
}

func TestLoopIteratorDAGIncompleteDiagnostic(t *testing.T) {
	s := loopSyncTestScanner(t, loopIteratorDAGBody(10), "1.23", "lexical")
	s.report()
	if countRules(*s.env.findings)["iterator-analysis-incomplete"] == 0 {
		t.Fatalf("budget exhaustion silently looked clean: %+v", *s.env.findings)
	}
}

func BenchmarkLoopIteratorOccurrenceDAG(b *testing.B) {
	for _, depth := range []int{2, 4, 6, 8, 10} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			body := loopIteratorDAGBody(depth)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				a, graph := loopIteratorOccurrenceGraph(b, body)
				a.expandIterators(graph)
			}
		})
	}
}
