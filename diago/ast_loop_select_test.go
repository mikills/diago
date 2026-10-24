package diago

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/cfg"
)

type loopSelectReceiveCase struct {
	name, body, hoisted string
	want                int
}

var loopSelectReceiveCases = []loopSelectReceiveCase{
	{
		"producer once",
		`for range xs{v:=0;select{case x:= <-func()chan int{v++;go func(){use(v)}();c:=make(chan int,1);c<-1;return c}():use(x)}}`,
		`for range xs{v:=0;ch:=func()chan int{v++;go func(){use(v)}();c:=make(chan int,1);c<-1;return c}();select{case x:= <-ch:use(x)}}`, 0,
	},
	{
		"comma ok producer once",
		`for range xs{v:=0;select{case x,ok:= <-func()chan int{v++;go func(){use(v)}();c:=make(chan int,1);c<-1;return c}():use(x,ok)}}`,
		`for range xs{v:=0;ch:=func()chan int{v++;go func(){use(v)}();c:=make(chan int,1);c<-1;return c}();select{case x,ok:= <-ch:use(x,ok)}}`, 0,
	},
	{
		"assignment producer once",
		`for range xs{v,x:=0,0;select{case x= <-func()chan int{v++;go func(){use(v)}();return make(chan int)}():use(x);default:}}`,
		`for range xs{v,x:=0,0;ch:=func()chan int{v++;go func(){use(v)}();return make(chan int)}();select{case x= <-ch:use(x);default:}}`, 0,
	},
	{
		"expression producer once",
		`for range xs{v:=0;select{case <-func()chan int{v++;go func(){use(v)}();return make(chan int)}():default:}}`,
		`for range xs{v:=0;ch:=func()chan int{v++;go func(){use(v)}();return make(chan int)}();select{case <-ch:default:}}`, 0,
	},
	{
		"selected lhs follows producer",
		`for range xs{v:=0;a:=[1]int{};select{case a[func()int{v++;return 0}()]= <-func()chan int{go func(){use(v)}();return make(chan int)}():};use(a)}`,
		`for range xs{v:=0;a:=[1]int{};ch:=func()chan int{go func(){use(v)}();return make(chan int)}();select{case a[func()int{v++;return 0}()]= <-ch:};use(a)}`, 1,
	},
	{
		"nonchosen lhs stays inactive",
		`for range xs{v:=0;a:=[1]int{};select{case a[func()int{v++;return 0}()]= <-make(chan int):default:go func(){use(v)}()};use(a)}`,
		`for range xs{v:=0;a:=[1]int{};ch:=make(chan int);select{case a[func()int{v++;return 0}()]= <-ch:default:go func(){use(v)}()};use(a)}`, 0,
	},
	{
		"nonchosen channel still runs",
		`for range xs{v:=0;select{case x:= <-func()chan int{go func(){use(v)}();return make(chan int)}():use(x);default:v++}}`,
		`for range xs{v:=0;ch:=func()chan int{go func(){use(v)}();return make(chan int)}();select{case x:= <-ch:use(x);default:v++}}`, 1,
	},
	{
		"later channel runs before first choice",
		`for range xs{v:=0;select{case x:= <-func()chan int{go func(){use(v)}();return make(chan int)}():use(x);case <-func()chan int{v++;return make(chan int)}():default:}}`,
		`for range xs{v:=0;ch:=func()chan int{go func(){use(v)}();return make(chan int)}();ch2:=func()chan int{v++;return make(chan int)}();select{case x:= <-ch:use(x);case <-ch2:default:}}`, 1,
	},
	{
		"send rhs after receive channel",
		`for range xs{v:=0;select{case x:= <-func()chan int{go func(){use(v)}();return make(chan int)}():use(x);case make(chan int)<-func()int{v++;return 1}():default:}}`,
		`for range xs{v:=0;ch:=func()chan int{go func(){use(v)}();return make(chan int)}();select{case x:= <-ch:use(x);case make(chan int)<-func()int{v++;return 1}():default:}}`, 1,
	},
	{
		"send rhs before receive channel",
		`for range xs{v:=0;select{case make(chan int)<-func()int{v++;return 1}():case x:= <-func()chan int{go func(){use(v)}();return make(chan int)}():use(x);default:}}`,
		`for range xs{v:=0;send:=make(chan int);value:=func()int{v++;return 1}();ch:=func()chan int{go func(){use(v)}();return make(chan int)}();select{case send<-value:case x:= <-ch:use(x);default:}}`, 0,
	},
	{
		"receive does not infer channel contents",
		`v:=0;for range xs{ch:=make(chan func(),1);ch<-func(){v++};select{case f:= <-ch:go f();default:}}`,
		`v:=0;for range xs{ch:=make(chan func(),1);ch<-func(){v++};saved:=ch;select{case f:= <-saved:go f();default:}}`, 0,
	},
	{
		"labels preserve select exit",
		`for range xs{v:=0;again:select{case x:= <-func()chan int{v++;go func(){use(v)}();return make(chan int)}():use(x);break again;default:break again}}`,
		`for range xs{v:=0;ch:=func()chan int{v++;go func(){use(v)}();return make(chan int)}();again:select{case x:= <-ch:use(x);break again;default:break again}}`, 0,
	},
	{
		"returning producer prunes postreturn write",
		`for range xs{v:=0;select{case x:= <-func()chan int{v++;go func(){use(v)}();return make(chan int);v++;return nil}():use(x)}}`,
		`for range xs{v:=0;ch:=func()chan int{v++;go func(){use(v)}();return make(chan int);v++;return nil}();select{case x:= <-ch:use(x)}}`, 0,
	},
}

func loopSelectReceiveAllCases() []loopSelectReceiveCase {
	cases := append(loopSelectReceiveCases[:0:0], loopSelectReceiveCases...)
	for _, tc := range loopSelectReceiveCases {
		tc.name = "deferred " + tc.name
		tc.body = "func(){defer func(){" + tc.body + "}()}()"
		tc.hoisted = "func(){defer func(){" + tc.hoisted + "}()}()"
		cases = append(cases, tc)
	}
	return append(cases, []loopSelectReceiveCase{
		{
			"deferred channel mutates callee before default",
			`v:=0;for range xs{f:=func(){v++};ch:=func()chan int{return nil};func(){defer func(){select{case <-func()chan int{ch=func()chan int{f=func(){};return nil};return nil}():case x:= <-ch():use(x);default:};go f()}()}()}`,
			`v:=0;for range xs{f:=func(){v++};ch:=func()chan int{return nil};func(){defer func(){a:=func()chan int{ch=func()chan int{f=func(){};return nil};return nil}();b:=ch();select{case <-a:case x:= <-b:use(x);default:};go f()}()}()}`, 0,
		},
		{
			"deferred channel activates callee before default",
			`v:=0;for range xs{f:=func(){};ch:=func()chan int{return nil};func(){defer func(){select{case <-func()chan int{ch=func()chan int{f=func(){v++};return nil};return nil}():case x:= <-ch():use(x);default:};go f()}()}()}`,
			`v:=0;for range xs{f:=func(){};ch:=func()chan int{return nil};func(){defer func(){a:=func()chan int{ch=func()chan int{f=func(){v++};return nil};return nil}();b:=ch();select{case <-a:case x:= <-b:use(x);default:};go f()}()}()}`, 1,
		},
		{
			"deferred selected lhs changes launch binding",
			`for range xs{v:=0;f:=func(){v++};a:=[1]int{};func(){defer func(){select{case a[func()int{f=func(){};return 0}()]= <-make(chan int):go f();default:};use(a)}()}()}`,
			`for range xs{v:=0;f:=func(){v++};a:=[1]int{};func(){defer func(){ch:=make(chan int);select{case a[func()int{f=func(){};return 0}()]= <-ch:go f();default:};use(a)}()}()}`, 0,
		},
		{
			"iterator stopped copy with labeled select",
			`for range xs{v:=0;iter:=func(yield func(int)bool){yield(1);selected:select{case x:= <-func()chan int{v++;go func(){use(v)}();return make(chan int)}():use(x);break selected;default:break selected}};outer:for range iter{break outer}}`,
			`for range xs{v:=0;iter:=func(yield func(int)bool){yield(1);ch:=func()chan int{v++;go func(){use(v)}();return make(chan int)}();selected:select{case x:= <-ch:use(x);break selected;default:break selected}};outer:for range iter{break outer}}`, 0,
		},
		{
			"nested iterator copies",
			`for range xs{v:=0;outer:=func(yield func(int)bool){inner:=func(yield func(int)bool){yield(1);select{case x:= <-func()chan int{v++;go func(){use(v)}();return make(chan int)}():use(x);default:}};for range inner{if !yield(1){return}}};again:for range outer{break again}}`,
			`for range xs{v:=0;outer:=func(yield func(int)bool){inner:=func(yield func(int)bool){yield(1);ch:=func()chan int{v++;go func(){use(v)}();return make(chan int)}();select{case x:= <-ch:use(x);default:}};for range inner{if !yield(1){return}}};again:for range outer{break again}}`, 0,
		},
	}...)
}

func TestLoopSelectReceiveEvaluation(t *testing.T) {
	for _, tc := range loopSelectReceiveAllCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					for _, body := range []string{tc.body, tc.hoisted} {
						findings := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+body+"}", "go1.23", mode)
						if got := countRules(findings)["goroutine-in-loop"]; got != tc.want {
							t.Fatalf("got %d, want %d: %+v\n%s", got, tc.want, findings, body)
						}
					}
				})
			}
		})
	}
}

func TestLoopSelectReceiveEvaluationPublic(t *testing.T) {
	var source strings.Builder
	source.WriteString("package selects\nfunc use(...any){}\n")
	for i, tc := range loopSelectReceiveAllCases() {
		fmt.Fprintf(&source, "func case%d(xs []int){%s}\nfunc hoisted%d(xs []int){%s}\n", i, tc.body, i, tc.hoisted)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":     "module example.com/selects\n\ngo 1.23\n",
		"selects.go": source.String(),
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("fixture requires complete types: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for i, tc := range loopSelectReceiveAllCases() {
		for _, symbol := range []string{fmt.Sprintf("case%d", i), fmt.Sprintf("hoisted%d", i)} {
			if got := counts[symbol]; got != tc.want {
				t.Errorf("%s (%s): got %d, want %d", tc.name, symbol, got, tc.want)
			}
		}
	}
}

func TestLoopSelectAdapterPreservesAST(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "select.go", `package p
func work(ch chan int,a []int,index func()int,producer func()chan int){
select{case a[index()]= <-producer():case x,ok:= <-ch:use(x,ok);case <-producer():default:}
}
func use(...any){}
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	body := file.Decls[0].(*ast.FuncDecl).Body
	var before, after bytes.Buffer
	if err := format.Node(&before, fset, file); err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{}
	if _, err := conf.Check("example.com/selects", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	metadata := make(map[ast.Expr]types.TypeAndValue, len(info.Types))
	for expr, tv := range info.Types {
		metadata[expr] = tv
	}
	a := &loopFlowAST{labels: map[string]*ast.Object{}}
	graph := cfg.New(a.block(body), func(*ast.CallExpr) bool { return true })
	var selected *ast.AssignStmt
	var prelude ast.Node
	var calls []string
	for _, node := range graph.Blocks[0].Nodes {
		ast.Inspect(node, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				calls = append(calls, call.Fun.(*ast.Ident).Name)
			}
			if unary, ok := n.(*ast.UnaryExpr); ok && unary.Op == token.ARROW {
				t.Fatal("receive performed before selection")
			}
			return true
		})
		if a.selectSources[node] != nil {
			prelude = node
		}
	}
	if !reflect.DeepEqual(calls, []string{"producer", "producer"}) {
		t.Fatalf("preselection calls = %v; LHS must stay selected-only", calls)
	}
	for _, block := range graph.Blocks[1:] {
		for _, node := range block.Nodes {
			if assign, ok := node.(*ast.AssignStmt); ok {
				selected = assign
				ast.Inspect(assign.Rhs[0], func(n ast.Node) bool {
					if _, call := n.(*ast.CallExpr); call {
						t.Fatal("selected receive reexecutes producer")
					}
					return true
				})
			}
		}
	}
	if selected == nil || prelude == nil {
		t.Fatal("missing synthetic evaluation or assignment")
	}
	// Model the repeated-node sharing used by expanded iterator templates.
	graph.Blocks = append(graph.Blocks, &cfg.Block{Nodes: []ast.Node{prelude, selected, prelude, selected}})
	birth := &loopVariable{}
	births := map[ast.Node][]*loopVariable{a.selectSources[selected]: {birth}}
	a.finishSelectCFG(graph, births)
	seen := map[ast.Node]bool{}
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			if source := a.selectSources[node]; source != nil {
				if seen[node] {
					t.Fatal("synthetic select marker shared by physical occurrences")
				}
				seen[node] = true
				if _, assign := node.(*ast.AssignStmt); assign && !reflect.DeepEqual(births[node], births[source]) {
					t.Fatal("selected assignment lost declaration births")
				}
			}
		}
	}
	if err := format.Node(&after, fset, file); err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("adapter mutated original AST")
	}
	if !reflect.DeepEqual(info.Types, metadata) {
		t.Fatal("adapter changed original type metadata")
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj != nil {
			t.Fatal("adapter mutated source identifier metadata")
		}
		return true
	})
}
