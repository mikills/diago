package diago

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// SymbolSummary names the variables, calls, and allocated types present on a hot source line.
type SymbolSummary struct {
	AssignedVars   []string `json:"assigned_vars,omitempty"`
	CalledFuncs    []string `json:"called_funcs,omitempty"`
	Args           []string `json:"args,omitempty"`
	AllocatedTypes []string `json:"allocated_types,omitempty"`
	SelectorBases  []string `json:"selector_bases,omitempty"`
	AppendTargets  []string `json:"append_targets,omitempty"`
}

// PerfRecommendation points at the source expression or line behind a profile hotspot.
type PerfRecommendation struct {
	ProfileType string        `json:"profile_type"`
	Severity    string        `json:"severity"`
	Function    string        `json:"function"`
	File        string        `json:"file,omitempty"`
	Line        int           `json:"line,omitempty"`
	CumPct      float64       `json:"cum_pct"`
	Source      string        `json:"source,omitempty"`
	Signals     []string      `json:"signals,omitempty"`
	Symbols     SymbolSummary `json:"symbols,omitempty"`
	PProfList   string        `json:"pprof_list,omitempty"`
	Message     string        `json:"message"`
}

func buildPerfRecommendations(report *Report, files profileFiles, limit int) []PerfRecommendation {
	if limit <= 0 {
		limit = 5
	}
	var out []PerfRecommendation
	for _, item := range report.Summary {
		profile := profileFileForType(files, item.ProfileType)
		rec := buildPerfRecommendation(item, profile)
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func profileFileForType(files profileFiles, profileType string) string {
	switch profileType {
	case "cpu":
		return files.cpu
	case "memory":
		return files.mem
	case "mutex":
		return files.mutex
	case "block":
		return files.block
	default:
		return ""
	}
}

func buildPerfRecommendation(item SummaryItem, profilePath string) PerfRecommendation {
	source := sourceLine(item.File, item.Line)
	signals := sourceSignals(item.File, item.Line)
	symbols := sourceSymbols(item.File, item.Line)
	list := pprofList(profilePath, item.Function)
	msg := perfMessage(item, signals)
	return PerfRecommendation{
		ProfileType: item.ProfileType,
		Severity:    perfSeverity(item.CumPct),
		Function:    item.Function,
		File:        item.File,
		Line:        item.Line,
		CumPct:      item.CumPct,
		Source:      source,
		Signals:     signals,
		Symbols:     symbols,
		PProfList:   list,
		Message:     msg,
	}
}

func perfSeverity(cumPct float64) string {
	switch {
	case cumPct >= 25:
		return "high"
	case cumPct >= 10:
		return "medium"
	default:
		return "low"
	}
}

func perfMessage(item SummaryItem, signals []string) string {
	prefix := fmt.Sprintf("%s uses %.2f%% cumulative %s profile", item.Function, item.CumPct, item.ProfileType)
	if len(signals) == 0 {
		return prefix + ". Inspect this source line and benchmark focused changes."
	}
	return prefix + ". Inspect detected source signals before changing behavior."
}

func sourceLine(path string, line int) string {
	if path == "" || line <= 0 {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	current := 1
	for scanner.Scan() {
		if current == line {
			return strings.TrimSpace(scanner.Text())
		}
		current++
	}
	return ""
}

func sourceSignals(path string, line int) []string {
	if path == "" || line <= 0 {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var signals []string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if !nodeSpansLine(fset, n, line) {
			return true
		}
		for _, signal := range nodeSignals(n) {
			if !seen[signal] {
				seen[signal] = true
				signals = append(signals, signal)
			}
		}
		return true
	})
	return signals
}

func sourceSymbols(path string, line int) SymbolSummary {
	if path == "" || line <= 0 {
		return SymbolSummary{}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return SymbolSummary{}
	}
	collector := newSymbolCollector()
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || !nodeSpansLine(fset, n, line) {
			return true
		}
		collectNodeSymbols(n, collector)
		return true
	})
	return collector.summary
}

type symbolCollector struct {
	summary SymbolSummary
	seen    map[string]bool
}

func newSymbolCollector() *symbolCollector {
	return &symbolCollector{seen: map[string]bool{}}
}

func (c *symbolCollector) add(kind, value string) {
	if value == "" || value == "_" {
		return
	}
	key := kind + "\x00" + value
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.append(kind, value)
}

func (c *symbolCollector) append(kind, value string) {
	switch kind {
	case "assigned":
		c.summary.AssignedVars = append(c.summary.AssignedVars, value)
	case "call":
		c.summary.CalledFuncs = append(c.summary.CalledFuncs, value)
	case "arg":
		c.summary.Args = append(c.summary.Args, value)
	case "alloc":
		c.summary.AllocatedTypes = append(c.summary.AllocatedTypes, value)
	case "selector":
		c.summary.SelectorBases = append(c.summary.SelectorBases, value)
	case "appendTarget":
		c.summary.AppendTargets = append(c.summary.AppendTargets, value)
	}
}

func collectNodeSymbols(n ast.Node, collector *symbolCollector) {
	switch x := n.(type) {
	case *ast.AssignStmt:
		collector.assignExprs(x.Lhs)
	case *ast.ValueSpec:
		collector.valueSpec(x)
	case *ast.RangeStmt:
		collector.rangeStmt(x)
	case *ast.ReturnStmt:
		collector.returnStmt(x)
	case *ast.CallExpr:
		collector.callExpr(x)
	case *ast.CompositeLit:
		collector.add("alloc", exprString(x.Type))
	case *ast.UnaryExpr:
		collector.addressExpr(x)
	}
}

func (c *symbolCollector) assignExprs(exprs []ast.Expr) {
	for _, expr := range exprs {
		c.add("assigned", exprString(expr))
		if sel, ok := expr.(*ast.SelectorExpr); ok {
			c.add("selector", exprString(sel.X))
		}
	}
}

func (c *symbolCollector) valueSpec(spec *ast.ValueSpec) {
	for _, name := range spec.Names {
		c.add("assigned", name.Name)
	}
}

func (c *symbolCollector) rangeStmt(stmt *ast.RangeStmt) {
	c.add("assigned", exprString(stmt.Key))
	c.add("assigned", exprString(stmt.Value))
	c.add("arg", exprString(stmt.X))
}

func (c *symbolCollector) returnStmt(stmt *ast.ReturnStmt) {
	for _, result := range stmt.Results {
		c.add("arg", exprString(result))
	}
}

func (c *symbolCollector) callExpr(call *ast.CallExpr) {
	name := exprString(call.Fun)
	c.add("call", name)
	c.callReceiver(call)
	c.callArgs(name, call.Args)
	c.callAllocation(name, call.Args)
}

func (c *symbolCollector) callReceiver(call *ast.CallExpr) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		c.add("selector", exprString(sel.X))
	}
}

func (c *symbolCollector) callArgs(name string, args []ast.Expr) {
	for i, arg := range args {
		argText := exprString(arg)
		c.add("arg", argText)
		if name == "append" && i == 0 {
			c.add("appendTarget", argText)
		}
	}
}

func (c *symbolCollector) callAllocation(name string, args []ast.Expr) {
	if name != "make" && name != "new" || len(args) == 0 {
		return
	}
	c.add("alloc", exprString(args[0]))
}

func (c *symbolCollector) addressExpr(expr *ast.UnaryExpr) {
	if expr.Op != token.AND {
		return
	}
	if lit, ok := expr.X.(*ast.CompositeLit); ok {
		c.add("alloc", "&"+exprString(lit.Type))
	}
}

func nodeSpansLine(fset *token.FileSet, n ast.Node, line int) bool {
	start := fset.Position(n.Pos()).Line
	end := fset.Position(n.End()).Line
	return line >= start && line <= end
}

func nodeSignals(n ast.Node) []string {
	switch x := n.(type) {
	case *ast.CallExpr:
		return callSignals(x)
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return []string{"address allocation: " + exprString(x)}
		}
	case *ast.CompositeLit:
		return []string{"composite literal: " + exprString(x.Type)}
	case *ast.MapType:
		return []string{"map type allocation/use"}
	case *ast.ArrayType:
		return []string{"slice/array type allocation/use"}
	}
	return nil
}

func callSignals(call *ast.CallExpr) []string {
	name := exprString(call.Fun)
	switch {
	case name == "make":
		return []string{"make allocation: " + exprString(call)}
	case name == "append":
		return []string{"append growth: " + exprString(call)}
	case strings.HasPrefix(name, "fmt."):
		return []string{"formatting call: " + name}
	case strings.HasPrefix(name, "json."):
		return []string{"json call: " + name}
	case strings.HasPrefix(name, "sort."):
		return []string{"sort call: " + name}
	case strings.Contains(name, "Parse") || strings.Contains(name, "Scan"):
		return []string{"parser/scanner call: " + name}
	default:
		return []string{"function call: " + name}
	}
}

func exprString(n any) string {
	node, ok := n.(ast.Node)
	if !ok || node == nil {
		return fmt.Sprintf("%T", n)
	}
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, token.NewFileSet(), node); err != nil {
		return fmt.Sprintf("%T", n)
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

func pprofList(profilePath, function string) string {
	if profilePath == "" || function == "" {
		return ""
	}
	cmd := exec.Command("go", "tool", "pprof", "-list", regexp.QuoteMeta(function), profilePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	return trimPProfList(out.String(), 60)
}

func trimPProfList(text string, maxLines int) string {
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= maxLines {
			out = append(out, "... truncated after "+strconv.Itoa(maxLines)+" lines")
			break
		}
	}
	return strings.Join(out, "\n")
}
