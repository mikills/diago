package diago

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

var loopExecutionSummaryCases = []struct {
	name, body string
	want       int
}{
	{"current reader kills constructor read", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;read()};read=func(){};for range xs{go worker();v++}`, 0},
	{"current reader adds actual read", `v:=0;read:=func(){};worker:=func(){_= &v;read()};read=func(){use(v)};for range xs{go worker();v++}`, 1},
	{"immutable alias current reader", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;read()};alias:=worker;read=func(){};for range xs{go alias();v++}`, 0},
	{"immutable alias reverse reader", `v:=0;read:=func(){};worker:=func(){_= &v;read()};alias:=worker;read=func(){use(v)};for range xs{go alias();v++}`, 1},
	{"local callback copy uses launch binding", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;copy:=read;copy()};read=func(){};for range xs{go worker();v++}`, 0},
	{"local callback copy reverse", `v:=0;read:=func(){};worker:=func(){_= &v;copy:=read;copy()};read=func(){use(v)};for range xs{go worker();v++}`, 1},
	{"copied callback survives source overwrite", `v:=0;read:=func(){use(v)};copy:=read;worker:=func(){_= &v;copy()};read=func(){};for range xs{go worker();v++}`, 1},
	{"copied noop ignores source replacement", `v:=0;read:=func(){};copy:=read;worker:=func(){_= &v;copy()};read=func(){use(v)};for range xs{go worker();v++}`, 0},
	{"local strong overwrite", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;copy:=read;copy=func(){};copy()};for range xs{go worker();v++}`, 0},
	{"nested immediate current reader", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;func(){read()}()};read=func(){};for range xs{go worker();v++}`, 0},
	{"nested immediate reverse reader", `v:=0;read:=func(){};worker:=func(){_= &v;func(){read()}()};read=func(){use(v)};for range xs{go worker();v++}`, 1},
	{"transitive direct literal writer", `v:=0;worker:=func(){_= &v;func(){func(){v++}()}()};for range xs{go worker()}`, 1},
	{"nested immutable writer", `v:=0;worker:=func(){_= &v;f:=func(){v++};alias:=f;alias()};for range xs{go worker()}`, 1},
	{"nested literal stays latent", `v:=0;worker:=func(){_= &v;_=func(){v++}};for range xs{go worker()}`, 0},
	{"nested callback argument stays latent", `v:=0;worker:=func(){_= &v;func(f func()){_=f}(func(){v++})};for range xs{go worker()}`, 0},
	{"root callback argument stays latent", `v:=0;worker:=func(f func()){_= &v;_=f};for range xs{go worker(func(){v++})}`, 0},
	{"root callback argument executes", `v:=0;worker:=func(f func()){_= &v;f()};for range xs{go worker(func(){v++})}`, 1},
	{"root literal parameter copy", `v:=0;read:=func(){use(v)};for range xs{go func(f func()){_= &v;f()}(read);read=func(){};v++}`, 1},
	{"root literal noop parameter", `v:=0;read:=func(){};for range xs{go func(f func()){_= &v;f()}(read);read=func(){use(v)};break}`, 0},
	{"sync wrapper launch current reader", `v:=0;read:=func(){use(v)};worker:=func(){_= &v;read()};wrapper:=func(){go worker()};read=func(){};for range xs{wrapper();v++}`, 0},
	{"sync wrapper launch reverse reader", `v:=0;read:=func(){};worker:=func(){_= &v;read()};wrapper:=func(){go worker()};read=func(){use(v)};for range xs{wrapper();v++}`, 1},
	{"sync current reader does not poison later launch", `for range xs{v:=0;read:=func(){use(v)};worker:=func(){_= &v;read()};worker();read=func(){};go worker();v++}`, 0},
	{"same async worker distinct occurrence payloads", `for range xs{v:=0;read:=func(){};worker:=func(f func()){_= &v;f()};go worker(read);v++;read=func(){use(v)};go worker(read)}`, 0},
	{"same async worker first occurrence captures", `for range xs{v:=0;read:=func(){use(v)};worker:=func(f func()){_= &v;f()};go worker(read);v++;read=func(){};go worker(read)}`, 1},
	{"iterator execution summary retained", `v:=0;worker:=func(){_= &v;for range func(yield func()bool){v++;yield()} {}};for range xs{go worker()}`, 1},
	{"iterator current reader", `v:=0;read:=func(){use(v)};it:=func(yield func()bool){_= &v;read();yield()};worker:=func(){_= &v;for range it {}};read=func(){};for range xs{go worker();v++}`, 0},
	{"iterator reverse reader", `v:=0;read:=func(){};it:=func(yield func()bool){_= &v;read();yield()};worker:=func(){_= &v;for range it {}};read=func(){use(v)};for range xs{go worker();v++}`, 1},
	{"uninvoked iterator stays latent", `v:=0;worker:=func(){_= &v;_=func(yield func()bool){v++;yield()}};for range xs{go worker()}`, 0},
	{"uninvoked method remains latent", `v:=item{};worker:=func(){_= &v;_=v.Use};for range xs{go worker();v.N++}`, 0},
	{"invoked method retains receiver read", `v:=item{};worker:=func(){_= &v;v.Use()};for range xs{go worker();v.N++}`, 1},
}

func TestLoopExecutionSummaries(t *testing.T) {
	for _, tc := range loopExecutionSummaryCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.23", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("findings=%d, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

func TestLoopExecutionSummaryExcludesInactiveConstructor(t *testing.T) {
	s := loopSyncTestScanner(t, loopExecutionSummaryCases[0].body, "1.23", "lexical")
	s.resetCaptureEffects()
	for round := 0; ; round++ {
		if round > 32 {
			t.Fatal("capture refinement did not converge")
		}
		s.refineLaunchBindings(s.rootNode)
		if !s.updateCaptureEffects() {
			break
		}
	}
	c, e := s.beginFlowConflicts(), s.beginFlowEffects()
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: e, conflicts: c}
	e.aliases = f.aliases
	f.refine(s.rootNode.(*ast.BlockStmt), nil)
	var target *loopConflictFrame
	for _, frame := range c.frames {
		if frame.root {
			target = frame.launches[0].target
		}
	}
	if target == nil {
		t.Fatal("known go has no binding-specific execution context")
	}
	var v *loopVariable
	for variable := range s.index.captures[target.owner] {
		if variable.ident != nil && variable.ident.Name == "v" {
			v = variable
		}
	}
	if v == nil || e.summaries[target][v]&loopCallableExecMask != 0 {
		t.Fatal("actual worker summary imported constructor execution")
	}
	stale := false
	for frame, summary := range e.summaries {
		if frame != target && frame.owner == target.owner && summary[v]&loopRead != 0 {
			stale = true
		}
	}
	if !stale {
		t.Fatal("fixture must retain a distinct, capturing constructor context")
	}
	e.finishFlowEffects()
	for _, access := range e.direct {
		if access.owner == target.owner && access.variable == v && access.mode&loopRead != 0 {
			t.Fatal("inactive constructor effect entered source-global capture refinement")
		}
	}
}

func TestLoopExecutionSummariesPublicCompleteTypes(t *testing.T) {
	var source strings.Builder
	source.WriteString(loopTestPrelude)
	for i, tc := range loopExecutionSummaryCases {
		fmt.Fprintf(&source, "func case%d(xs []int){%s}\n", i, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":       "module example.com/execution\n\ngo 1.23\n",
		"execution.go": source.String(),
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("fixture must have complete type information: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for i, tc := range loopExecutionSummaryCases {
		if got := counts[fmt.Sprintf("case%d", i)]; got != tc.want {
			t.Errorf("%s: findings=%d, want %d", tc.name, got, tc.want)
		}
	}
}
