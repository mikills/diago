package diago

import "testing"

func TestDeferredReturnPositions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"blank before callback", `v:=0;defer func()(_ int,f func()){defer func(){f()}();return 0,func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
		{"grouped blanks before callback", `v:=0;defer func()(_, _ int,f func()){defer func(){f()}();return 0,1,func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
		{"discarded callback does not shift", `v:=0;defer func()(_ func(),f func()){defer func(){f()}();return func(){v++},func(){}}();for range xs{go func(){use(v)}()}`, 0},
		{"blank between callbacks", `v:=0;defer func()(a func(),_ int,b func()){defer func(){b()}();return func(){},0,func(){v++}}();for range xs{go func(){use(v)}()}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "1.23", mode)
				if countRules(got)["goroutine-in-loop"] != tc.want {
					t.Errorf("%s: got%+v want%d", mode, got, tc.want)
				}
			}
		})
	}
}

func TestBlankReturnPublic(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/returnpositions\n\ngo 1.23\n",
		"p.go": `package returnpositions
func use(int){}
func hazard(xs []int){v:=0;defer func()(_ int,f func()){defer func(){f()}();return 0,func(){v++}}();for range xs{go func(){use(v)}()}}
func safe(xs []int){v:=0;defer func()(_ func(),f func()){defer func(){f()}();return func(){v++},func(){}}();for range xs{go func(){use(v)}()}}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("incomplete fixture: %+v", finding)
		}
		if finding.Rule == "goroutine-in-loop" {
			got[finding.Symbol]++
		}
	}
	if got["hazard"] != 1 || got["safe"] != 0 {
		t.Fatalf("incorrect return positions: %v: %+v", got, findings)
	}
}
