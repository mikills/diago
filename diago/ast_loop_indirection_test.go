package diago

import "testing"

func TestDoubleIndirectionPublic(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/indirection\n\ngo 1.23\n",
		"p.go": `package indirection
func use(int) {}
func store(xs []int){for range xs{v:=0;p:=&v;pp:=&p;go func(){use(v)}();**pp=1}}
func load(xs []int){for range xs{v:=0;p:=&v;pp:=&p;go func(){v=1}();use(**pp)}}
func replaced(xs []int){for range xs{v,u:=0,0;p:=&v;pp:=&p;p=&u;go func(){use(v)}();**pp=1}}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("incomplete fixture types: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			got[finding.Symbol]++
		}
	}
	for name, want := range map[string]int{"store": 1, "load": 1, "replaced": 0} {
		if got[name] != want {
			t.Errorf("%s: got %d want %d: %+v", name, got[name], want, findings)
		}
	}
}

func TestLoadedAliasEpochs(t *testing.T) {
	cell, target, slot := &loopVariable{}, &loopVariable{}, &loopVariable{}
	w := &loopWalk{values: map[*loopVariable]loopValue{slot: {target: loopRead | loopPriorRef}}}
	aliases := map[*loopVariable]*loopVariable{cell: slot}
	if got := loopLoadedAddressAliases(loopValue{cell: loopRead | loopPriorRef}, w, aliases); len(got) != 0 {
		t.Fatalf("old cell loaded new contents: %v", got)
	}
	if got := loopLoadedAddressAliases(loopValue{cell: loopRead}, w, aliases); got[target] != loopRead|loopPriorRef {
		t.Fatalf("loaded pointer lost target provenance: %v", got)
	}
}
