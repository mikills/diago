package diago

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePProfText(t *testing.T) {
	t.Run("with source lines", func(t *testing.T) {
		input := `File: petname.test
Type: cpu
Duration: 10.83s, Total samples = 8.69s (80.27%)
Showing nodes accounting for 5.49s, 63.18% of 8.69s total
Dropped 106 nodes (cum <= 0.04s)
Showing top 30 nodes out of 106
      flat  flat%   sum%        cum   cum%
         0     0%     0%      5.43s 62.49%  testing.(*B).launch /usr/lib/go/src/testing/benchmark.go:316
     0.09s  1.04%  1.04%      3.85s 44.30%  petname.(*Generator).GenerateWithSuffix /home/user/petname/petname.go:51
     0.59s  6.79% 27.73%      1.55s 17.84%  petname.(*Generator).pick2 /home/user/petname/petname.go:82`

		findings := ParsePProfText(input, 2.0)

		if len(findings) != 3 {
			t.Fatalf("expected 3 findings, got %d", len(findings))
		}
		if findings[0].Function != "testing.(*B).launch" {
			t.Errorf("expected testing.(*B).launch, got %s", findings[0].Function)
		}
		if findings[0].File != "/usr/lib/go/src/testing/benchmark.go" {
			t.Errorf("expected benchmark.go path, got %s", findings[0].File)
		}
		if findings[0].Line != 316 {
			t.Errorf("expected line 316, got %d", findings[0].Line)
		}
		if findings[1].Function != "petname.(*Generator).GenerateWithSuffix" {
			t.Errorf("expected GenerateWithSuffix, got %s", findings[1].Function)
		}
		if findings[1].File != "/home/user/petname/petname.go" {
			t.Errorf("expected petname.go, got %s", findings[1].File)
		}
		if findings[1].Line != 51 {
			t.Errorf("expected line 51, got %d", findings[1].Line)
		}
		if findings[1].CumPct != 44.30 {
			t.Errorf("expected cum%% 44.30, got %.2f", findings[1].CumPct)
		}
	})

	t.Run("without source lines", func(t *testing.T) {
		input := `      flat  flat%   sum%        cum   cum%
     0.09s  1.04%  1.04%      3.85s 44.30%  petname.(*Generator).GenerateWithSuffix`

		findings := ParsePProfText(input, 0)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		if findings[0].Function != "petname.(*Generator).GenerateWithSuffix" {
			t.Errorf("expected function name, got %s", findings[0].Function)
		}
		if findings[0].File != "" {
			t.Errorf("expected empty file for no-lines format, got %s", findings[0].File)
		}
	})

	t.Run("inline annotation", func(t *testing.T) {
		input := `      flat  flat%   sum%        cum   cum%
     0.59s  6.79% 34.52%      1.42s 16.34%  strings.(*Builder).WriteString (inline) /usr/lib/go/src/strings/builder.go:75`

		findings := ParsePProfText(input, 0)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
		if findings[0].CumPct != 16.34 {
			t.Errorf("expected 16.34%%, got %.2f%%", findings[0].CumPct)
		}
	})

	t.Run("threshold filtering", func(t *testing.T) {
		input := `      flat  flat%   sum%        cum   cum%
     0.09s  1.04%  1.04%      3.85s 44.30%  bigFunc /x.go:1
     0.01s  0.50%  1.54%      0.10s  0.80%  smallFunc /x.go:2`

		findings := ParsePProfText(input, 1.0)
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		findings := ParsePProfText("", 1.0)
		if len(findings) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(findings))
		}
	})

	t.Run("garbage input", func(t *testing.T) {
		input := `this is not pprof output
some random text
7.20MB)   4.00%    0.00%
another broken line 123 456`

		findings := ParsePProfText(input, 0)
		if len(findings) != 0 {
			t.Fatalf("expected 0 findings for garbage, got %d", len(findings))
		}
	})
}

func TestParseEscapeOutput(t *testing.T) {
	t.Run("valid output", func(t *testing.T) {
		input := `# petname
./petname.go:97:6: variable sb escapes to heap
./petname.go:105:6: variable sb escapes to heap
./petname.go:42:13: (*Generator).Generate ... argument does not escape`

		findings := ParseEscapeOutput(input)
		if len(findings) != 2 {
			t.Fatalf("expected 2 findings, got %d", len(findings))
		}
		if findings[0].Line != 97 {
			t.Errorf("expected line 97, got %d", findings[0].Line)
		}
	})

	t.Run("moved to heap", func(t *testing.T) {
		findings := ParseEscapeOutput("./petname.go:42:6: moved to heap: sb")
		if len(findings) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(findings))
		}
	})

	t.Run("ignores inlining", func(t *testing.T) {
		input := `./petname.go:55:14: inlining call to strings.(*Builder).WriteString
./petname.go:60:2: can inline (*Generator).pick2`

		findings := ParseEscapeOutput(input)
		if len(findings) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(findings))
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if len(ParseEscapeOutput("")) != 0 {
			t.Fatal("expected 0 findings for empty input")
		}
	})
}

func TestConfigDefaults(t *testing.T) {
	cfg := &Config{}
	cfg.defaults()

	if cfg.OutputFile != "diago_findings.txt" {
		t.Errorf("wrong default output: %s", cfg.OutputFile)
	}
	if cfg.BenchFilter != "." {
		t.Errorf("wrong default filter: %s", cfg.BenchFilter)
	}
	if cfg.Threshold != 1.0 {
		t.Errorf("wrong default threshold: %f", cfg.Threshold)
	}
	if cfg.Format != FormatText {
		t.Errorf("wrong default format: %s", cfg.Format)
	}
}

func TestConfigDefaultsPreserveValues(t *testing.T) {
	cfg := &Config{
		OutputFile:  "custom.json",
		BenchFilter: "BenchmarkX",
		Threshold:   5.0,
		Format:      FormatJSON,
	}
	cfg.defaults()

	if cfg.OutputFile != "custom.json" || cfg.BenchFilter != "BenchmarkX" ||
		cfg.Threshold != 5.0 || cfg.Format != FormatJSON {
		t.Error("defaults should not overwrite existing values")
	}
}

func TestWriteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.json")

	report := &Report{
		Package:   "./test",
		Threshold: 2.0,
		CPUFindings: []Finding{
			{Function: "main.hot", File: "/src/main.go", Line: 42, FlatPct: 50, CumPct: 75},
		},
		EscapeFindings: []EscapeFinding{
			{File: "main.go", Line: 10, Detail: "x escapes to heap"},
		},
	}

	if err := writeJSON(path, report); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	data, _ := os.ReadFile(path)
	var parsed Report
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.CPUFindings[0].File != "/src/main.go" {
		t.Errorf("expected file in JSON, got %s", parsed.CPUFindings[0].File)
	}
	if parsed.CPUFindings[0].Line != 42 {
		t.Errorf("expected line 42, got %d", parsed.CPUFindings[0].Line)
	}
}

func TestWriteText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.txt")

	report := &Report{
		Package:   "./test",
		Threshold: 2.0,
		Summary: []SummaryItem{
			{ProfileType: "cpu", Function: "hot", File: "hot.go", Line: 10, CumPct: 50},
		},
		CPUFindings: []Finding{
			{Function: "main.hot", File: "/src/main.go", Line: 42, FlatPct: 50, CumPct: 75},
		},
		MutexFindings: []Finding{
			{Function: "sync.Lock", FlatPct: 10, CumPct: 30},
		},
	}

	if err := writeText(path, report); err != nil {
		t.Fatalf("writeText: %v", err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	for _, s := range []string{
		"=== diago findings ===",
		"--- summary:",
		"[cpu] hot",
		"hot.go:10",
		"--- cpu hotspots",
		"/src/main.go:42",
		"--- mutex contention",
	} {
		if !strings.Contains(content, s) {
			t.Errorf("text output missing: %q", s)
		}
	}
}

func TestBuildSummary(t *testing.T) {
	report := &Report{
		CPUFindings: []Finding{
			{Function: "runtime.mallocgc", CumPct: 80},
			{Function: "myapp.HotFunc", File: "hot.go", Line: 10, CumPct: 50},
			{Function: "myapp.WarmFunc", File: "warm.go", Line: 20, CumPct: 30},
		},
		MemFindings: []Finding{
			{Function: "strings.(*Builder).grow", CumPct: 90},
			{Function: "myapp.AllocFunc", File: "alloc.go", Line: 5, CumPct: 40},
		},
	}

	summary := buildSummary(report)

	// runtime/stdlib should be filtered out
	for _, s := range summary {
		if strings.HasPrefix(s.Function, "runtime.") || strings.HasPrefix(s.Function, "strings.") {
			t.Errorf("summary should filter out stdlib: %s", s.Function)
		}
	}

	if len(summary) == 0 {
		t.Fatal("expected non-empty summary")
	}
	if summary[0].Function != "myapp.HotFunc" {
		t.Errorf("expected HotFunc first, got %s", summary[0].Function)
	}
}

func TestUserFindings(t *testing.T) {
	input := []Finding{
		{Function: "runtime.mallocgc"},
		{Function: "testing.(*B).launch"},
		{Function: "sync.(*Mutex).Lock"},
		{Function: "internal/bytealg.MakeNoZero"},
		{Function: "strings.(*Builder).Grow"},
		{Function: "myapp.DoWork"},
		{Function: "myapp.Process"},
	}

	out := userFindings(input)
	if len(out) != 2 {
		t.Fatalf("expected 2 user findings, got %d", len(out))
	}
	if out[0].Function != "myapp.DoWork" {
		t.Errorf("expected myapp.DoWork, got %s", out[0].Function)
	}
}

func TestCompareReports(t *testing.T) {
	before := &Report{
		CPUFindings: []Finding{
			{Function: "hot", CumPct: 50},
			{Function: "warm", CumPct: 20},
			{Function: "removed", CumPct: 10},
		},
		EscapeFindings: []EscapeFinding{
			{File: "a.go", Line: 10, Detail: "x escapes to heap"},
			{File: "b.go", Line: 20, Detail: "y escapes to heap"},
		},
	}

	after := &Report{
		CPUFindings: []Finding{
			{Function: "hot", CumPct: 30},
			{Function: "warm", CumPct: 20.2},
			{Function: "new", CumPct: 15},
		},
		EscapeFindings: []EscapeFinding{
			{File: "a.go", Line: 10, Detail: "x escapes to heap"},
			{File: "c.go", Line: 30, Detail: "z escapes to heap"},
		},
	}

	cr := CompareReports(before, after)

	// hot went from 50 -> 30, should be improved
	if len(cr.CPUImproved) < 1 {
		t.Fatal("expected at least 1 cpu improvement")
	}
	foundHot := false
	for _, c := range cr.CPUImproved {
		if c.Function == "hot" {
			foundHot = true
			if c.DeltaPct != -20.0 {
				t.Errorf("expected delta -20, got %.2f", c.DeltaPct)
			}
		}
	}
	if !foundHot {
		t.Error("hot should appear in improvements")
	}

	// "removed" disappeared — should be in improved
	foundRemoved := false
	for _, c := range cr.CPUImproved {
		if c.Function == "removed" {
			foundRemoved = true
		}
	}
	if !foundRemoved {
		t.Error("removed function should appear in improvements")
	}

	// "new" appeared — should be in regressed
	foundNew := false
	for _, c := range cr.CPURegressed {
		if c.Function == "new" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Error("new function should appear in regressions")
	}

	// warm barely changed (0.2%), should not appear
	for _, c := range cr.CPUImproved {
		if c.Function == "warm" {
			t.Error("warm should be filtered out (change < 0.5%)")
		}
	}
	for _, c := range cr.CPURegressed {
		if c.Function == "warm" {
			t.Error("warm should be filtered out (change < 0.5%)")
		}
	}

	// escape diffs
	if len(cr.EscapesAdded) != 1 {
		t.Fatalf("expected 1 added escape, got %d", len(cr.EscapesAdded))
	}
	if cr.EscapesAdded[0].File != "c.go" {
		t.Errorf("expected c.go added, got %s", cr.EscapesAdded[0].File)
	}

	if len(cr.EscapesRemoved) != 1 {
		t.Fatalf("expected 1 removed escape, got %d", len(cr.EscapesRemoved))
	}
	if cr.EscapesRemoved[0].File != "b.go" {
		t.Errorf("expected b.go removed, got %s", cr.EscapesRemoved[0].File)
	}
}

func TestWriteCompareText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compare.txt")

	cr := &CompareReport{
		CPUImproved: []ChangedFinding{
			{Function: "hot", File: "hot.go", Line: 10, OldCumPct: 50, NewCumPct: 30, DeltaPct: -20},
		},
		CPURegressed: []ChangedFinding{
			{Function: "new", OldCumPct: 0, NewCumPct: 15, DeltaPct: 15},
		},
		EscapesAdded: []EscapeFinding{
			{File: "c.go", Line: 30, Detail: "z escapes to heap"},
		},
	}

	if err := writeCompareText(path, cr); err != nil {
		t.Fatalf("writeCompareText: %v", err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	for _, s := range []string{
		"=== comparison report ===",
		"cpu improvements",
		"hot (hot.go:10)",
		"50.00% -> 30.00%",
		"cpu regressions",
		"new:",
		"new heap escapes",
		"c.go:30",
	} {
		if !strings.Contains(content, s) {
			t.Errorf("compare output missing: %q", s)
		}
	}
}

func TestWriteCompareJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compare.json")

	cr := &CompareReport{
		CPUImproved: []ChangedFinding{
			{Function: "hot", OldCumPct: 50, NewCumPct: 30, DeltaPct: -20},
		},
	}

	if err := WriteCompareReport(path, cr, FormatJSON); err != nil {
		t.Fatalf("WriteCompareReport JSON: %v", err)
	}

	data, _ := os.ReadFile(path)
	var parsed CompareReport
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.CPUImproved) != 1 {
		t.Errorf("expected 1 improvement, got %d", len(parsed.CPUImproved))
	}
}

func BenchmarkParsePProfText(b *testing.B) {
	input := strings.Repeat(`      flat  flat%   sum%        cum   cum%
     0.09s  1.04%  1.04%      3.85s 44.30%  github.com/mikills/diago/diago.ParsePProfText /src/diago/diago.go:500
     0.59s  6.79% 27.73%      1.55s 17.84%  github.com/mikills/diago/diago.ParseEscapeOutput /src/diago/diago.go:550
`, 100)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ParsePProfText(input, 1.0)
	}
}

func BenchmarkParseEscapeOutput(b *testing.B) {
	input := strings.Repeat(`./diago.go:97:6: variable sb escapes to heap
./diago.go:105:6: variable findings escapes to heap
./diago.go:42:13: argument does not escape
`, 100)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ParseEscapeOutput(input)
	}
}

func BenchmarkCompareReports(b *testing.B) {
	before := &Report{}
	after := &Report{}
	for i := 0; i < 500; i++ {
		before.CPUFindings = append(before.CPUFindings, Finding{Function: fmt.Sprintf("fn-%d", i), CumPct: float64(i % 100)})
		after.CPUFindings = append(after.CPUFindings, Finding{Function: fmt.Sprintf("fn-%d", i), CumPct: float64((i + 10) % 100)})
		before.EscapeFindings = append(before.EscapeFindings, EscapeFinding{File: "a.go", Line: i, Detail: "x escapes to heap"})
		after.EscapeFindings = append(after.EscapeFindings, EscapeFinding{File: "a.go", Line: i + 5, Detail: "x escapes to heap"})
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = CompareReports(before, after)
	}
}
