package diago

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
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
	ImportPath   string   `json:"ImportPath"`
	Name         string   `json:"Name"`
	Dir          string   `json:"Dir"`
	Export       string   `json:"Export"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
	exports      map[string]string
	ownedFields  map[string]map[string]bool
}

type packageStats struct {
	importPath string
	files      int
	funcs      int
}

const (
	largePackageFileLimit = 50
	largePackageFuncLimit = 1000
)

type astContext struct {
	pkg       goListPackage
	path      string
	isTest    bool
	generated bool
	fset      *token.FileSet
	file      *ast.File
	types     *types.Info
	releases  map[string]map[int]bool
	cancelers map[string]map[int]bool
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
	ownedFields := collectLifecycleOwnedFields(pkgs)

	var findings []ASTFinding
	for _, pkg := range pkgs {
		pkg.ownedFields = ownedFields
		stats := analyzePackage(&findings, pkg)
		appendLargePackageFinding(&findings, pkg.Dir, stats)
	}
	return findings, nil
}

func analyzePackage(findings *[]ASTFinding, pkg goListPackage) packageStats {
	stats := packageStats{importPath: pkg.ImportPath}
	signals := newPackageSignals(pkg)
	files := append(append([]string{}, pkg.GoFiles...), pkg.TestGoFiles...)
	files = append(files, pkg.XTestGoFiles...)
	fset := token.NewFileSet()
	parsedFiles := make(map[string]*ast.File, len(files))
	for _, file := range files {
		path := filepath.Join(pkg.Dir, file)
		parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			loc := astLocation{pkg: pkg.ImportPath, file: path}
			*findings = append(*findings, astFinding("parse-error", "high", loc, "", err.Error()))
			continue
		}
		parsedFiles[file] = parsed
	}
	typeInfo := checkPackageTypes(pkg, fset, parsedFiles)
	releases := collectReleaseParams(parsedFiles, pkg.GoFiles)
	cancelers := collectCancelParams(pkg.ImportPath, parsedFiles, pkg.GoFiles, typeInfo)
	params := analyzePackageFileParams{
		pkg: pkg, fset: fset, typeInfo: typeInfo, releases: releases, cancelers: cancelers,
		stats: &stats, signals: signals,
	}
	for _, file := range files {
		if parsed := parsedFiles[file]; parsed != nil {
			params.file = file
			params.parsed = parsed
			analyzePackageFile(findings, params)
		}
	}
	appendPackageSignalFindings(findings, pkg, signals)
	return stats
}

type analyzePackageFileParams struct {
	pkg       goListPackage
	file      string
	parsed    *ast.File
	fset      *token.FileSet
	typeInfo  *types.Info
	releases  map[string]map[int]bool
	cancelers map[string]map[int]bool
	stats     *packageStats
	signals   *packageSignals
}

func analyzePackageFile(findings *[]ASTFinding, params analyzePackageFileParams) {
	path := filepath.Join(params.pkg.Dir, params.file)
	// A //diago:ignore file is excluded entirely: no findings, signals, or stats.
	if isIgnoredFile(params.parsed) {
		return
	}

	generated := isGeneratedFile(path, params.parsed)
	lineCount := fileLineCount(path)
	isTest := strings.HasSuffix(params.file, "_test.go")
	ctx := astContext{
		pkg: params.pkg, path: path, isTest: isTest, generated: generated,
		fset: params.fset, file: params.parsed, types: params.typeInfo, releases: params.releases,
		cancelers: params.cancelers,
	}
	// large-package weighs production code only, so tests don't inflate the count.
	if !isTest && !generated {
		params.stats.files++
	}
	appendLargeFileFinding(findings, ctx, lineCount)
	findCommentDebt(findings, params.pkg.ImportPath, path, params.fset, params.parsed)

	analyzeExtraFile(findings, params.signals, ctx, params.parsed)
	for _, decl := range params.parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if !isTest && !generated {
			params.stats.funcs++
		}
		analyzeFunc(findings, ctx, fn)
	}
}

func checkPackageTypes(pkg goListPackage, fset *token.FileSet, parsed map[string]*ast.File) *types.Info {
	files := make([]*ast.File, 0, len(pkg.GoFiles))
	for _, name := range pkg.GoFiles {
		if file := parsed[name]; file != nil {
			files = append(files, file)
		}
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	if len(files) == 0 {
		return info
	}
	lookup := func(path string) (io.ReadCloser, error) {
		export := pkg.exports[path]
		if export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(export)
	}
	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", lookup),
		Error:    func(error) {},
	}
	_, _ = conf.Check(pkg.ImportPath, fset, files, info)
	return info
}

func appendLargeFileFinding(findings *[]ASTFinding, ctx astContext, lineCount int) {
	if ctx.generated || ctx.isTest || lineCount <= 1000 {
		return
	}
	loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: 1}
	msg := fmt.Sprintf("file has %d lines", lineCount)
	*findings = append(*findings, astFinding("large-file", severity(lineCount, 500, 1000, 1500), loc, "", msg))
}

func isGeneratedFile(path string, file *ast.File) bool {
	return hasGeneratedFilename(path) || headerCommentMatchesAST(file, isGeneratedMarker)
}

// isIgnoredFile reports whether the parsed file carries a //diago:ignore
// directive in its header.
func isIgnoredFile(file *ast.File) bool {
	return headerCommentMatchesAST(file, isDiagoIgnoreMarker)
}

// headerCommentMatchesAST reports whether any comment before the package clause
// matches. Restricting to the header keeps a mid-file comment from mislabeling
// hand-written code.
func headerCommentMatchesAST(file *ast.File, match func(string) bool) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			if match(comment.Text) {
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

// A header comment marks generated code if it contains "do not edit" (which
// rarely appears in prose) or begins with a generator phrase. The phrases are
// anchored to the start so ordinary text like "...was generated by hand" or
// "the code generated below" does not trigger a skip. The canonical
// "Code generated ... DO NOT EDIT" and swaggo's "Package docs Code generated by
// ... DO NOT EDIT" both match on "do not edit".
var generatedSubstrings = []string{"do not edit"}
var generatedPrefixes = []string{"code generated", "generated by", "auto-generated"}

// isGeneratedMarker reports whether a comment carries a generated-code marker.
func isGeneratedMarker(comment string) bool {
	text := strings.ToLower(stripCommentDelims(comment))
	for _, s := range generatedSubstrings {
		if strings.Contains(text, s) {
			return true
		}
	}
	for _, p := range generatedPrefixes {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

const diagoIgnoreDirective = "diago:ignore"

// isDiagoIgnoreMarker reports whether a comment is a //diago:ignore directive,
// optionally followed by a reason.
func isDiagoIgnoreMarker(comment string) bool {
	text := stripCommentDelims(comment)
	return text == diagoIgnoreDirective || strings.HasPrefix(text, diagoIgnoreDirective+" ")
}

func stripCommentDelims(comment string) string {
	text := strings.TrimSpace(comment)
	text = strings.TrimPrefix(text, "//")
	text = strings.TrimPrefix(text, "/*")
	text = strings.TrimSuffix(text, "*/")
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "*") // block-comment continuation line
	return strings.TrimSpace(text)
}

// isGeneratedSourcePath reports whether the file at path is generated, from its
// name and header. It reads the file rather than the parsed AST so it can filter
// findings from external tools (gopls, staticcheck) that only report file paths.
func isGeneratedSourcePath(path string) bool {
	generated, _ := headerMarkers(path)
	return generated
}

// isIgnoredSourcePath reports whether the file at path carries a //diago:ignore
// directive in its header.
func isIgnoredSourcePath(path string) bool {
	_, ignored := headerMarkers(path)
	return ignored
}

// headerMarkers scans the file header once (the lines before the package clause)
// and reports whether it marks the file generated or //diago:ignore'd. A
// "package " line inside a /* */ comment does not end the header.
func headerMarkers(path string) (generated, ignored bool) {
	generated = hasGeneratedFilename(path)
	f, err := os.Open(path)
	if err != nil {
		return generated, ignored
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	inBlock := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !inBlock && strings.HasPrefix(line, "package ") {
			break
		}
		if isGeneratedMarker(line) {
			generated = true
		}
		if isDiagoIgnoreMarker(line) {
			ignored = true
		}
		if generated && ignored {
			break
		}
		inBlock = nextBlockState(inBlock, line)
	}
	if scanner.Err() != nil {
		return generated, false
	}
	return generated, ignored
}

// nextBlockState approximates whether the line after this one is inside a /* */
// block comment.
func nextBlockState(inBlock bool, line string) bool {
	if inBlock {
		return !strings.Contains(line, "*/")
	}
	return strings.HasPrefix(line, "/*") && !strings.Contains(line, "*/")
}

// shouldSkipFile reports whether findings in the file should be dropped. Files
// with a //diago:ignore directive are always skipped; generated files are
// skipped unless includeGenerated is set.
func shouldSkipFile(path string, includeGenerated bool) bool {
	generated, ignored := headerMarkers(path)
	if ignored {
		return true
	}
	return generated && !includeGenerated
}

// filterSkippedFindings drops findings located in skipped files (see
// shouldSkipFile). Relative paths are resolved against workDir; skip status is
// cached per file.
func filterSkippedFindings(findings []ASTFinding, workDir string, includeGenerated bool) []ASTFinding {
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
		skip, ok := cache[path]
		if !ok {
			skip = shouldSkipFile(path, includeGenerated)
			cache[path] = skip
		}
		if skip {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

func appendLargePackageFinding(findings *[]ASTFinding, dir string, stats packageStats) {
	// Total lines are covered by large-file (1k per file) plus the file count, so
	// the package only weighs file and function counts here.
	if stats.files <= largePackageFileLimit && stats.funcs <= largePackageFuncLimit {
		return
	}
	loc := astLocation{pkg: stats.importPath, file: dir, line: 1}
	msg := fmt.Sprintf("package has %d files and %d funcs", stats.files, stats.funcs)
	*findings = append(*findings, astFinding("large-package", "medium", loc, "", msg))
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
	exports := listExportFiles(workDir, target)
	for i := range pkgs {
		pkgs[i].exports = exports
	}
	return pkgs, nil
}

func listExportFiles(workDir, target string) map[string]string {
	cmd := exec.Command("go", "list", "-e", "-deps", "-export", "-json", target)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	exports := make(map[string]string)
	for dec.More() {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil
		}
		if pkg.Export != "" {
			exports[pkg.ImportPath] = pkg.Export
		}
	}
	return exports
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
		mustPanic := isMustConstructor(fn) && nodeContainsWithoutNestedFunc(fn.Body, call)
		if isPanicCall(call) && !mustPanic && !isRepanicAfterRecover(fn.Body, call) {
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

func isMustConstructor(fn *ast.FuncDecl) bool {
	name := fn.Name.Name
	if !strings.HasPrefix(name, "Must") || len(name) == len("Must") || fieldCount(fn.Type.Results) == 0 || returnsError(fn) {
		return false
	}
	next := name[len("Must")]
	return next >= 'A' && next <= 'Z'
}

func isRepanicAfterRecover(body *ast.BlockStmt, panicCall *ast.CallExpr) bool {
	if len(panicCall.Args) != 1 {
		return false
	}
	panicArg, ok := panicCall.Args[0].(*ast.Ident)
	if !ok {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		deferStmt, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		lit, ok := deferStmt.Call.Fun.(*ast.FuncLit)
		if !ok || !nodeContainsWithoutNestedFunc(lit.Body, panicCall) {
			return true
		}
		found = blockHasRepanic(lit.Body, panicCall, panicArg.Name)
		return !found
	})
	return found
}

func blockHasRepanic(block *ast.BlockStmt, panicCall *ast.CallExpr, name string) bool {
	recovered := false
	for _, stmt := range block.List {
		if !nodeContainsWithoutNestedFunc(stmt, panicCall) {
			if isRecoverAssignment(stmt, name) {
				recovered = true
			}
			continue
		}
		if recovered {
			return true
		}
		if statementInitializesRecover(stmt, name) {
			return true
		}
		return nestedBlockHasRepanic(stmt, panicCall, name)
	}
	return false
}

func statementInitializesRecover(stmt ast.Stmt, name string) bool {
	ifStmt, ok := stmt.(*ast.IfStmt)
	return ok && isRecoverAssignment(ifStmt.Init, name)
}

func nestedBlockHasRepanic(stmt ast.Stmt, panicCall *ast.CallExpr, name string) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		if n == nil || found {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		nested, ok := n.(*ast.BlockStmt)
		if !ok || !nodeContainsWithoutNestedFunc(nested, panicCall) {
			return true
		}
		found = blockHasRepanic(nested, panicCall, name)
		return false
	})
	return found
}

func isRecoverAssignment(stmt ast.Stmt, name string) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !isIdentName(assign.Lhs[0], name) {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	return ok && isIdentCall(call, "recover")
}

func nodeContainsWithoutNestedFunc(root, target ast.Node) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if n == target {
			found = true
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		return !found
	})
	return found
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
