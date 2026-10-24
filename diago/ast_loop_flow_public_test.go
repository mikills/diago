package diago

import "testing"

func TestLoopFlowPublicAnalyzeAST(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/loopflow\n\ngo 1.22\n",
		"flow.go": `package loopflow
func use(...any) {}
func ranges() {
	var i int
	for i=0;i<3;i++ {
		for _,f:=range []func(){func(){use(i)}} {go f()}
	}
}
func fields() {
	var i int
	for i=0;i<3;i++ {
		var s struct{F func()}
		s.F=func(){use(i)}
		go s.F()
	}
}
func aliases() {
	var i int
	for i=0;i<3;i++ {
		var s struct{F func()}
		p:=&s
		p.F=func(){use(i)}
		go s.F()
	}
}
func safe() {
	var i int
	for i=0;i<3;i++ {
		var s struct{N int}
		s.N=i
		go use(s.N)
		for k:=range []*int{&i} {go use(k)}
	}
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := countRules(findings)
	if counts["type-info-unavailable"] != 0 || counts["goroutine-in-loop"] != 3 {
		t.Fatalf("expected three typed flow findings: %+v", findings)
	}
	byFunction := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "goroutine-in-loop" {
			byFunction[finding.Symbol]++
		}
	}
	for _, name := range []string{"ranges", "fields", "aliases"} {
		if byFunction[name] != 1 {
			t.Fatalf("expected one finding in %s, got %v", name, byFunction)
		}
	}
	if byFunction["safe"] != 0 {
		t.Fatalf("scalar values must remain safe: %v", byFunction)
	}
}
