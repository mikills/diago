package diago

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

var loopLaunchContextCases = []struct {
	name, body string
	want       int
}{
	{"iterator repeated source event", `for range xs{v:=0;iter:=func(yield func()bool){go func(){use(v)}();yield()};for range iter{};v++;for range iter{}}`, 1},
	{"iterator first invocation captures", `for range xs{v:=0;f:=func(){use(v)};iter:=func(yield func()bool){go f();yield()};for range iter{};v++;f=func(){};for range iter{}}`, 1},
	{"iterator second invocation captures after write", `for range xs{v:=0;f:=func(){};iter:=func(yield func()bool){go f();yield()};for range iter{};v++;f=func(){use(v)};for range iter{}}`, 0},
	{"iterator second invocation captures before write", `for range xs{v:=0;f:=func(){};iter:=func(yield func()bool){go f();yield()};for range iter{};f=func(){use(v)};for range iter{};v++}`, 1},
	{"wrapper invocation current capture", `v:=0;f:=func(){};wrapper:=func(){go f()};for range xs{f=func(){use(v)};wrapper();v++}`, 1},
	{"wrapper invocation kills constructor capture", `v:=0;f:=func(){use(v)};wrapper:=func(){go f()};for range xs{f=func(){};wrapper();v++}`, 0},
	{"wrapper first invocation captures", `for range xs{v:=0;f:=func(){use(v)};wrapper:=func(){go f()};wrapper();v++;f=func(){};wrapper()}`, 1},
	{"wrapper second invocation captures after write", `for range xs{v:=0;f:=func(){};wrapper:=func(){go f()};wrapper();v++;f=func(){use(v)};wrapper()}`, 0},
	{"wrapper local strong overwrite", `v:=0;f:=func(){};wrapper:=func(){h:=f;h=func(){};go h()};for range xs{f=func(){use(v)};wrapper();v++}`, 0},
	{"wrapper local copy current binding", `v:=0;f:=func(){};wrapper:=func(){h:=f;go h()};for range xs{f=func(){use(v)};wrapper();v++}`, 1},
	{"wrapper alias current binding", `v:=0;f:=func(){};wrapper:=func(){go f()};alias:=wrapper;for range xs{f=func(){use(v)};alias();v++}`, 1},
	{"wrapper nested current binding", `v:=0;f:=func(){};inner:=func(){go f()};wrapper:=func(){inner()};for range xs{f=func(){use(v)};wrapper();v++}`, 1},
	{"wrapper nested kills constructor capture", `v:=0;f:=func(){use(v)};inner:=func(){go f()};wrapper:=func(){inner()};for range xs{f=func(){};wrapper();v++}`, 0},
	{"wrapper branch strong overwrite", `v:=0;f:=func(){};wrapper:=func(){h:=f;if b{h=func(){}}else{h=func(){}};go h()};for range xs{f=func(){use(v)};wrapper();v++}`, 0},
	{"wrapper branch retains capture", `v:=0;f:=func(){};wrapper:=func(){h:=f;if b{h=func(){}};go h()};for range xs{f=func(){use(v)};wrapper();v++}`, 1},
	{"wrapper internal write current capture", `for range xs{v:=0;f:=func(){};wrapper:=func(){go f();v++};f=func(){use(v)};wrapper()}`, 1},
	{"wrapper internal write kills constructor capture", `for range xs{v:=0;f:=func(){use(v)};wrapper:=func(){go f();v++};f=func(){};wrapper()}`, 0},
	{"iterator wrapper current capture", `v:=0;f:=func(){};wrapper:=func(){go f()};for range xs{f=func(){use(v)};iter:=func(yield func()bool){wrapper();yield()};for range iter{};v++}`, 1},
	{"iterator wrapper first invocation captures", `for range xs{v:=0;f:=func(){use(v)};wrapper:=func(){go f()};iter:=func(yield func()bool){wrapper();yield()};for range iter{};v++;f=func(){};for range iter{}}`, 1},
	{"iterator wrapper second invocation after write", `for range xs{v:=0;f:=func(){};wrapper:=func(){go f()};iter:=func(yield func()bool){wrapper();yield()};for range iter{};v++;f=func(){use(v)};for range iter{}}`, 0},
	{"wrapper iterator current capture", `v:=0;f:=func(){};wrapper:=func(){iter:=func(yield func()bool){go f();yield()};for range iter{}};for range xs{f=func(){use(v)};wrapper();v++}`, 1},
	{"wrapper invocation loop internal local conflict", `wrapper:=func(){v:=0;go func(){use(v)}();v++};for range xs{wrapper()}`, 1},
	{"wrapper invocation loop local allocation safe", `wrapper:=func(){v:=0;go func(){v++}()};for range xs{wrapper()}`, 0},
	{"wrapper no loop no diagnostic", `v:=0;f:=func(){};wrapper:=func(){go f()};f=func(){use(v)};wrapper();v++`, 0},
	{"nonloop event conflicts with loop event", `v:=0;f:=func(){};wrapper:=func(){go f()};f=func(){v++};wrapper();for range xs{go func(){use(v)}()}`, 1},
}

func TestLoopLaunchExecutionContexts(t *testing.T) {
	for _, tc := range loopLaunchContextCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					// The helper type-checks valid Go 1.23 before dropping type
					// information, even in the syntax-only modes.
					s := loopSyncTestScanner(t, tc.body, "1.23", mode)
					s.report()
					got := 0
					for _, finding := range *s.env.findings {
						if finding.Rule == "goroutine-in-loop" {
							got++
						}
					}
					if got != tc.want {
						t.Fatalf("findings=%d, want %d: %+v", got, tc.want, *s.env.findings)
					}
				})
			}
		})
	}
}

func TestLoopLaunchExecutionContextsPublic(t *testing.T) {
	var source strings.Builder
	source.WriteString("package contexts\nfunc use(...any){}\n")
	for i, tc := range loopLaunchContextCases {
		fmt.Fprintf(&source, "func case%d(xs []int,b bool){%s}\n", i, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":      "module example.com/contexts\n\ngo 1.23\n",
		"contexts.go": source.String(),
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("fixture lost type info: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			counts[finding.Symbol]++
		}
	}
	for i, tc := range loopLaunchContextCases {
		if got := counts[fmt.Sprintf("case%d", i)]; got != tc.want {
			t.Errorf("%s: findings=%d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestLoopLaunchExecutionContextsRepeatedWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("large repeated-call context regression")
	}
	body := `for range xs{v:=0;f:=func(){use(v)};wrapper:=func(){go f()};` + strings.Repeat(`wrapper();`, 10000) + `v++}`
	s := loopSyncTestScanner(t, body, "1.23", "lexical")
	s.report()
	if got := len(*s.env.findings); got != 1 {
		t.Fatalf("findings=%d, want 1", got)
	}
	c := loopLaunchTestContexts(s)
	actual, calls := 0, 0
	for _, frame := range c.activeContextFrames() {
		calls += len(frame.calls)
		if frame.actual && frame.owner == s.launches[0].owner {
			actual++
		}
	}
	if actual != 1 || calls != 10000 {
		t.Fatalf("worker execution contexts=%d calls=%d, want 1/10000", actual, calls)
	}
}

func TestLoopLaunchExecutionContextsWrapperDAG(t *testing.T) {
	// The dynamic invocation tree has 2^48 leaves; the source DAG has 49
	// workers. Context interning and memoized summaries must follow the DAG.
	const depth = 48
	var body strings.Builder
	body.WriteString(`v:=0;worker0:=func(){go func(){use(v)}()};`)
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&body, "worker%d:=func(){worker%d();worker%d()};", i, i-1, i-1)
	}
	fmt.Fprintf(&body, "for range xs{worker%d();v++}", depth)
	s := loopSyncTestScanner(t, body.String(), "1.23", "lexical")
	s.report()
	if got := len(*s.env.findings); got != 1 {
		t.Fatalf("findings=%d, want 1", got)
	}
	c := loopLaunchTestContexts(s)
	actual := 0
	for _, frame := range c.activeContextFrames() {
		if frame.actual {
			actual++
		}
	}
	if actual != depth+1 {
		t.Fatalf("actual contexts=%d, want %d", actual, depth+1)
	}
}

func loopLaunchTestContexts(s *loopScanner) *loopFlowConflicts {
	c := s.beginFlowConflicts()
	f := &loopFlow{s: s, aliases: map[*loopVariable]*loopVariable{}, effects: s.beginFlowEffects(), conflicts: c}
	f.effects.aliases = f.aliases
	f.refine(s.rootNode.(*ast.BlockStmt), nil)
	return c
}

func TestLoopLaunchOccurrenceSnapshots(t *testing.T) {
	s := loopSyncTestScanner(t, loopLaunchContextCases[1].body, "1.23", "lexical")
	c := loopLaunchTestContexts(s)
	var events []loopSyncLaunch
	for _, frame := range c.activeContextFrames() {
		for _, event := range frame.launches {
			if event.stmt == s.launches[0].stmt {
				events = append(events, event)
			}
		}
	}
	if len(events) != 2 || events[0].point == events[1].point {
		t.Fatalf("got %d occurrences, want 2 distinct CFG points", len(events))
	}
	// Block order is not source order. Exactly one occurrence owns v's
	// capture; neither the last snapshot nor a source-level union suffices.
	capturing := 0
	for _, event := range events {
		for v := range event.values {
			if v.ident != nil && v.ident.Name == "v" {
				capturing++
			}
		}
	}
	if capturing != 1 {
		t.Fatalf("v captured by %d occurrences, want 1", capturing)
	}
}
