package diago

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var longTestNamePattern = regexp.MustCompile(`^Test[A-Za-z0-9_]{32,}$`)

// ASTFinding is a native source-structure finding from Go's parser/AST.
type ASTFinding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Package  string `json:"package"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Symbol   string `json:"symbol,omitempty"`
	Message  string `json:"message"`
}

type goListPackage struct {
	ImportPath  string   `json:"ImportPath"`
	Name        string   `json:"Name"`
	Dir         string   `json:"Dir"`
	GoFiles     []string `json:"GoFiles"`
	TestGoFiles []string `json:"TestGoFiles"`
}

type packageStats struct {
	importPath string
	files      int
	lines      int
	funcs      int
}

type astContext struct {
	pkg       goListPackage
	path      string
	isTest    bool
	generated bool
	fset      *token.FileSet
}

type astLocation struct {
	pkg  string
	file string
	line int
}

// AnalyzeAST runs native AST checks. It uses only the Go toolchain and standard library.
func AnalyzeAST(workDir, target string) ([]ASTFinding, error) {
	pkgs, err := listPackages(workDir, target)
	if err != nil {
		return nil, err
	}

	var findings []ASTFinding
	for _, pkg := range pkgs {
		stats := analyzePackage(&findings, pkg)
		appendLargePackageFinding(&findings, pkg.Dir, stats)
	}
	return findings, nil
}

func analyzePackage(findings *[]ASTFinding, pkg goListPackage) packageStats {
	stats := packageStats{importPath: pkg.ImportPath}
	signals := newPackageSignals(pkg)
	files := append(append([]string{}, pkg.GoFiles...), pkg.TestGoFiles...)
	for _, file := range files {
		analyzePackageFile(findings, pkg, file, &stats, signals)
	}
	appendPackageSignalFindings(findings, pkg, signals)
	return stats
}

func analyzePackageFile(findings *[]ASTFinding, pkg goListPackage, file string, stats *packageStats, signals *packageSignals) {
	path := filepath.Join(pkg.Dir, file)
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		loc := astLocation{pkg: pkg.ImportPath, file: path}
		*findings = append(*findings, astFinding("parse-error", "high", loc, "", err.Error()))
		return
	}

	generated := isGeneratedFile(path, parsed)
	lineCount := fileLineCount(path)
	stats.files++
	stats.lines += lineCount
	appendLargeFileFinding(findings, pkg.ImportPath, path, lineCount, generated)
	findCommentDebt(findings, pkg.ImportPath, path, fset, parsed)

	ctx := astContext{pkg: pkg, path: path, isTest: strings.HasSuffix(file, "_test.go"), generated: generated, fset: fset}
	analyzeExtraFile(findings, signals, ctx, parsed)
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		stats.funcs++
		analyzeFunc(findings, ctx, fn)
	}
}

func appendLargeFileFinding(findings *[]ASTFinding, pkg, path string, lineCount int, generated bool) {
	if generated || lineCount <= 1000 {
		return
	}
	loc := astLocation{pkg: pkg, file: path, line: 1}
	msg := fmt.Sprintf("file has %d lines", lineCount)
	*findings = append(*findings, astFinding("large-file", severity(lineCount, 500, 1000, 1500), loc, "", msg))
}

func isGeneratedFile(path string, file *ast.File) bool {
	if hasGeneratedFilename(path) {
		return true
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if isGeneratedMarker(comment.Text) {
				return true
			}
		}
	}
	return false
}

// hasGeneratedFilename reports whether a filename matches a common codegen
// naming convention (oapi-codegen *.gen.go, sqlc *.sql.go, *.generated.*).
func hasGeneratedFilename(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".gen.go") ||
		strings.Contains(base, ".generated.") ||
		strings.HasSuffix(base, ".sql.go")
}

// isGeneratedMarker reports whether a comment is the standard
// "Code generated ... DO NOT EDIT" generated-file marker.
func isGeneratedMarker(comment string) bool {
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(comment, "//"), "/*"))
	return strings.HasPrefix(text, "Code generated ") && strings.Contains(text, "DO NOT EDIT")
}

// isGeneratedSourcePath reports whether the file at path is generated, using
// only its name and header. It reads the file rather than the parsed AST so it
// can be used to filter findings from external tools (gopls, staticcheck) that
// only report file paths. The marker must appear before the package clause.
func isGeneratedSourcePath(path string) bool {
	if hasGeneratedFilename(path) {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "package ") {
			return false
		}
		if isGeneratedMarker(line) {
			return true
		}
	}
	return false
}

// filterGeneratedFindings drops findings located in generated files. Relative
// file paths are resolved against workDir. Generated status is cached per file.
func filterGeneratedFindings(findings []ASTFinding, workDir string) []ASTFinding {
	cache := make(map[string]bool)
	kept := make([]ASTFinding, 0, len(findings))
	for _, f := range findings {
		if f.File == "" {
			kept = append(kept, f)
			continue
		}
		path := f.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		gen, ok := cache[path]
		if !ok {
			gen = isGeneratedSourcePath(path)
			cache[path] = gen
		}
		if gen {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

func appendLargePackageFinding(findings *[]ASTFinding, dir string, stats packageStats) {
	largeSignals := 0
	if stats.files > 50 {
		largeSignals++
	}
	if stats.funcs > 250 {
		largeSignals++
	}
	if stats.lines > 6000 {
		largeSignals++
	}
	if largeSignals == 0 {
		return
	}

	sev := "low"
	if largeSignals >= 2 || stats.files > 80 || stats.funcs > 500 || stats.lines > 12000 {
		sev = "medium"
	}
	loc := astLocation{pkg: stats.importPath, file: dir, line: 1}
	msg := fmt.Sprintf("package has %d files, %d funcs, %d lines", stats.files, stats.funcs, stats.lines)
	*findings = append(*findings, astFinding("large-package", sev, loc, "", msg))
}

func listPackages(workDir, target string) ([]goListPackage, error) {
	cmd := exec.Command("go", "list", "-json", target)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list -json: %w\n%s", err, out.String())
	}

	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var pkgs []goListPackage
	for dec.More() {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil, err
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs, nil
}

func analyzeFunc(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl) {
	name := funcName(fn)
	if ctx.isTest {
		appendLongTestNameFinding(findings, ctx, fn, name)
	} else {
		appendFunctionMetricFindings(findings, ctx, fn, name)
	}
	findDangerousCalls(findings, ctx, fn, name)
	findLoopHazards(findings, loopContext{pkg: ctx.pkg.ImportPath, path: ctx.path, fset: ctx.fset, fn: name}, fn.Body, 0)
}

func appendLongTestNameFinding(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	if !isLongScenarioTestName(fn.Name.Name) {
		return
	}
	loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: ctx.fset.Position(fn.Pos()).Line}
	msg := fmt.Sprintf("test name has %d characters after Test. Prefer t.Run scenarios for long cases", len(fn.Name.Name)-len("Test"))
	*findings = append(*findings, astFinding("long-test-name", "low", loc, name, msg))
}

func isLongScenarioTestName(name string) bool {
	return longTestNamePattern.MatchString(name)
}

func appendFunctionMetricFindings(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	start := ctx.fset.Position(fn.Pos())
	loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: start.Line}
	metrics := []struct {
		rule     string
		value    int
		warn     int
		high     int
		critical int
		message  string
	}{
		{"function-length", ctx.fset.Position(fn.End()).Line - start.Line + 1, 80, 150, 250, "function has %d lines"},
		{"cyclomatic-complexity", cyclomatic(fn.Body), 10, 15, 25, "cyclomatic complexity is %d"},
		{"nesting-depth", maxNesting(fn.Body), 4, 6, 8, "max nesting depth is %d"},
		{"parameter-count", fieldCount(fn.Type.Params), 5, 8, 12, "function has %d parameters"},
	}
	for _, m := range metrics {
		if m.value > m.warn {
			msg := fmt.Sprintf(m.message, m.value)
			*findings = append(*findings, astFinding(m.rule, severity(m.value, m.warn, m.high, m.critical), loc, name, msg))
		}
	}
}

func findDangerousCalls(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	if ctx.isTest || ctx.pkg.Name == "main" {
		return
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isPanicCall(call) {
			loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: ctx.fset.Position(call.Pos()).Line}
			*findings = append(*findings, astFinding("panic-outside-main", "high", loc, name, "panic used outside main/test code"))
		}
		if isOSExitCall(call) {
			loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: ctx.fset.Position(call.Pos()).Line}
			*findings = append(*findings, astFinding("os-exit-outside-main", "critical", loc, name, "os.Exit used outside main/test code"))
		}
		return true
	})
}

func isPanicCall(call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == "panic"
}

func isOSExitCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Exit" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "os"
}

func cyclomatic(body *ast.BlockStmt) int {
	c := 1
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CaseClause, *ast.CommClause:
			c++
		case *ast.BinaryExpr:
			if x.Op.String() == "&&" || x.Op.String() == "||" {
				c++
			}
		}
		return true
	})
	return c
}

func maxNesting(body *ast.BlockStmt) int { return maxNestingNode(body, 0) }

func maxNestingNode(n ast.Node, depth int) int {
	max := depth
	ast.Inspect(n, func(child ast.Node) bool {
		if child == nil || child == n {
			return true
		}
		if !isNestingNode(child) {
			return true
		}
		d := maxNestingNode(child, depth+1)
		if d > max {
			max = d
		}
		return false
	})
	return max
}

func isNestingNode(n ast.Node) bool {
	switch n.(type) {
	case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		return true
	default:
		return false
	}
}

type loopContext struct {
	pkg  string
	path string
	fset *token.FileSet
	fn   string
}

func findLoopHazards(findings *[]ASTFinding, ctx loopContext, n ast.Node, loopDepth int) {
	ast.Inspect(n, func(child ast.Node) bool {
		if child == nil || child == n {
			return true
		}
		if body, ok := loopBody(child); ok {
			findLoopHazards(findings, ctx, body, loopDepth+1)
			return false
		}
		appendLoopHazard(findings, ctx, child, loopDepth)
		return true
	})
}

func loopBody(n ast.Node) (*ast.BlockStmt, bool) {
	switch x := n.(type) {
	case *ast.ForStmt:
		return x.Body, true
	case *ast.RangeStmt:
		return x.Body, true
	default:
		return nil, false
	}
}

func appendLoopHazard(findings *[]ASTFinding, ctx loopContext, n ast.Node, loopDepth int) {
	if loopDepth == 0 {
		return
	}
	loc := astLocation{pkg: ctx.pkg, file: ctx.path, line: ctx.fset.Position(n.Pos()).Line}
	switch n.(type) {
	case *ast.DeferStmt:
		*findings = append(*findings, astFinding("defer-in-loop", "high", loc, ctx.fn, "defer inside loop can delay resource release"))
	case *ast.GoStmt:
		*findings = append(*findings, astFinding("goroutine-in-loop", "high", loc, ctx.fn, "goroutine launched inside loop"))
	}
}

func findCommentDebt(findings *[]ASTFinding, pkg, path string, fset *token.FileSet, file *ast.File) {
	for _, group := range file.Comments {
		for _, c := range group.List {
			marker, ok := commentDebtMarker(c.Text)
			if !ok {
				continue
			}
			sev := "low"
			if marker == "FIXME" || marker == "HACK" {
				sev = "medium"
			}
			loc := astLocation{pkg: pkg, file: path, line: fset.Position(c.Pos()).Line}
			*findings = append(*findings, astFinding("comment-debt", sev, loc, "", strings.TrimSpace(c.Text)))
		}
	}
}

func commentDebtMarker(text string) (string, bool) {
	upper := strings.ToUpper(text)
	for _, marker := range []string{"TODO", "FIXME", "HACK"} {
		if strings.Contains(upper, marker) {
			return marker, true
		}
	}
	return "", false
}

func astFinding(rule, sev string, loc astLocation, symbol, msg string) ASTFinding {
	return ASTFinding{Rule: rule, Severity: sev, Package: loc.pkg, File: loc.file, Line: loc.line, Symbol: symbol, Message: msg}
}

func severity(v, warn, high, critical int) string {
	switch {
	case v > critical:
		return "critical"
	case v > high:
		return "high"
	case v > warn:
		return "medium"
	default:
		return "low"
	}
}

func fieldCount(fields *ast.FieldList) int {
	if fields == nil {
		return 0
	}
	count := 0
	for _, f := range fields.List {
		if len(f.Names) == 0 {
			count++
		} else {
			count += len(f.Names)
		}
	}
	return count
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return receiverName(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

func receiverName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + receiverName(x.X)
	case *ast.IndexExpr:
		return receiverName(x.X)
	case *ast.IndexListExpr:
		return receiverName(x.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return 0
	}
	return bytes.Count(data, []byte("\n")) + 1
}
