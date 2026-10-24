package diago

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var loopIteratorCases = []struct {
	name, body string
	want       int
}{
	{"after yield", `v:=0;for range func(yield func()bool){yield();v++}{go func(){use(v)}()}`, 1},
	{"before first yield only", `v:=0;for range func(yield func()bool){v++;yield()}{go func(){use(v)}()}`, 0},
	{"outer launch before invocation", `for range xs{v:=0;go func(){use(v)}();for range func(yield func()bool){v++;yield()} {}}`, 1},
	{"named after yield", `v:=0;iter:=func(yield func()bool){yield();v++};for range iter{go func(){use(v)}()}`, 1},
	{"named before yield", `v:=0;iter:=func(yield func()bool){v++;yield()};for range iter{go func(){use(v)}()}`, 0},
	{"goto post yield precedes it lexically", `v:=0;for range func(yield func()bool){goto start;after:v++;return;start:yield();goto after}{go func(){use(v)}()}`, 1},
	{"goto pre yield follows it lexically", `v:=0;for range func(yield func()bool){goto before;resume:yield();return;before:v++;goto resume}{go func(){use(v)}()}`, 0},
	{"conditional later write", `v:=0;for range func(yield func()bool){yield();if len(xs)>0{v++}}{go func(){use(v)}()}`, 1},
	{"conditional pre only", `v:=0;for range func(yield func()bool){if len(xs)>0{v++;yield()}else{yield()}}{go func(){use(v)}()}`, 0},
	{"between yields", `v:=0;for range func(yield func()bool){if !yield(){return};v++;yield()}{go func(){use(v)}()}`, 1},
	{"break respects false guard", `v:=0;for range func(yield func()bool){if !yield(){return};v++;yield()}{go func(){use(v)}();break}`, 0},
	{"break still runs cleanup", `v:=0;for range func(yield func()bool){if !yield(){v++;return}}{go func(){use(v)}();break}`, 1},
	{"break ignores unreachable true branch", `v:=0;for range func(yield func()bool){if yield(){v++}}{go func(){use(v)}();break}`, 0},
	{"continue follows true guard", `v:=0;for range func(yield func()bool){if !yield(){return};v++}{go func(){use(v)}();continue}`, 1},
	{"return runs false cleanup", `v:=0;for range func(yield func()bool){if !yield(){v++;return}}{go func(){use(v)}();return}`, 1},
	{"return respects false guard", `v:=0;for range func(yield func()bool){if !yield(){return};v++}{go func(){use(v)}();return}`, 0},
	{"nested return inner cleanup", `v:=0;for range func(yield func()bool){yield()}{for range func(yield func()bool){yield();v++}{go func(){use(v)}();return}}`, 1},
	{"nested return outer cleanup", `v:=0;for range func(yield func()bool){yield();v++}{for range func(yield func()bool){yield()}{go func(){use(v)}();return}}`, 1},
	{"after last yield on break", `v:=0;for range func(yield func()bool){yield();v++}{go func(){use(v)}();break}`, 1},
	{"sync argument before body", `v:=0;for range func(yield func(int)bool){yield(func()int{v++;return 0}())}{go func(){use(v)}()}`, 0},
	{"sync second argument after earlier body", `v:=0;for range func(yield func(int)bool){if !yield(0){return};yield(func()int{v++;return 0}())}{go func(){use(v)}()}`, 1},
	{"fresh yield slots", `for v:=range func(yield func(int)bool){if !yield(0){return};yield(1)}{go func(){v++}()}`, 0},
	{"reused yield slots", `var v int;for v=range func(yield func(int)bool){if !yield(0){return};yield(1)}{go func(){use(v)}()}`, 1},
	{"yield callback snapshot", `v:=0;for f:=range func(yield func(func())bool){yield(func(){use(v)});v++}{go f()}`, 1},
	{"unreachable iterator write", `v:=0;for range func(yield func()bool){yield();return;v++}{go func(){use(v)}()}`, 0},
	{"unreachable yield cannot add continuation", `v:=0;for range func(yield func()bool){yield();return;yield();v++}{go func(){use(v)}()}`, 0},
	{"false-only yield cannot add continuation", `v:=0;for range func(yield func()bool){if yield(){return};yield();v++}{go func(){use(v)}()}`, 0},
	{"iterator loop before next yield", `v:=0;for range func(yield func()bool){for i:=0;i<2;i++{v++;if !yield(){return}}}{go func(){use(v)}()}`, 1},
	{"break stops iterator loop", `v:=0;for range func(yield func()bool){for i:=0;i<2;i++{v++;if !yield(){return}}}{go func(){use(v)}();break}`, 0},
	{"labeled break respects false guard", `v:=0;again:for range func(yield func()bool){if !yield(){return};v++}{go func(){use(v)}();break again}`, 0},
	{"labeled continue resumes iterator", `v:=0;again:for range func(yield func()bool){if !yield(){return};v++}{go func(){use(v)}();continue again}`, 1},
	{"finish after no yield", `v:=0;for range func(yield func()bool){v++}{go func(){use(v)}()}`, 0},
	{"after finish parent access", `v:=0;for range func(yield func()bool){yield()}{go func(){v++}()};use(v)`, 1},
	{"single yield has no fictitious sibling", `v:=0;for range func(yield func()bool){v++;yield()}{go func(){v++}()}`, 0},
	{"two yields launch siblings", `v:=0;for range func(yield func()bool){if !yield(){return};yield()}{go func(){v++}()}`, 1},
	{"scalar two value bindings", `for v,w:=range func(yield func(int,int)bool){if !yield(0,1){return};yield(2,3)}{go func(){v++;w++}()}`, 0},
	{"second slot callable snapshot", `v:=0;for _,f:=range func(yield func(int,func())bool){yield(0,func(){use(v)});v++}{go f()}`, 1},
	{"body overwrite does not poison next snapshot", `v:=0;for f:=range func(yield func(func())bool){if !yield(func() {}){return};yield(func(){})}{go f();f=func(){v++};_=f}`, 0},
	{"iterator local overwrite flows to callback", `v:=0;f:=func(){use(v)};for range func(yield func()bool){f=func(){};yield();v++}{go f()}`, 0},
}

func TestLoopIteratorOrdering(t *testing.T) {
	for _, tc := range loopIteratorCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.23", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got %+v want %d", mode, got, tc.want)
				}
			}
		})
	}
}

func TestLoopIteratorPublicGo123(t *testing.T) {
	var source strings.Builder
	source.WriteString("package iterator\nfunc use(...any){}\n")
	for i, tc := range loopIteratorCases {
		source.WriteString("func case" + strings.Repeat("x", i+1) + "(xs []int){" + tc.body + "}\n")
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":      "module example.com/iterator\n\ngo 1.23\n",
		"iterator.go": source.String(),
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
	if countRules(findings)["type-info-unavailable"] != 0 {
		t.Fatalf("public analysis lost types: %+v", findings)
	}
	bySymbol := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "goroutine-in-loop" {
			bySymbol[finding.Symbol]++
		}
	}
	for i, tc := range loopIteratorCases {
		if got := bySymbol["case"+strings.Repeat("x", i+1)]; got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

// Race-detector evidence for the public repro and the pre-yield safe control.
// The reader and iterator write are deliberately unordered, while WaitGroup
// waits make termination deterministic without ordering those accesses.
func TestLoopIteratorRuntimeRace(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess race build")
	}
	for _, after := range []bool{false, true} {
		dir := t.TempDir()
		write := func(name, source string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
		}
		write("go.mod", "module example.com/range-race\n\ngo 1.23\n")
		iterator := "v++;yield()"
		if after {
			iterator = "yield();v++"
		}
		write("main_test.go", `package iterator
import("sync";"testing")
func use(v int) { if v < 0 { panic(v) } }
func TestRange(t *testing.T){var wg sync.WaitGroup;v:=0;for range func(yield func()bool){`+iterator+`}{wg.Add(1);go func(){defer wg.Done();use(v)}()};wg.Wait()}
`)
		cmd := exec.Command("go", "test", "-race", "-count=1", "./...")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if after {
			if err == nil || !strings.Contains(string(out), "DATA RACE") {
				t.Fatalf("expected runtime race: %v\n%s", err, out)
			}
		} else if err != nil {
			t.Fatalf("safe pre-yield control: %v\n%s", err, out)
		}
	}
}
