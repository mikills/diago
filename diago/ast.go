package diago

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"golang.org/x/tools/go/gcexportdata"
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
	ImportPath      string                      `json:"ImportPath"`
	Name            string                      `json:"Name"`
	Dir             string                      `json:"Dir"`
	Export          string                      `json:"Export"`
	GoFiles         []string                    `json:"GoFiles"`
	CgoFiles        []string                    `json:"CgoFiles"`
	CompiledGoFiles []string                    `json:"CompiledGoFiles"`
	ForTest         string                      `json:"ForTest"`
	TestGoFiles     []string                    `json:"TestGoFiles"`
	XTestGoFiles    []string                    `json:"XTestGoFiles"`
	Module          *struct{ GoVersion string } `json:"Module"`
	GoVersion       string                      `json:"-"`
	exports         map[string]string
	testExports     map[string]string // sparse test-variant overrides over exports
	importOverrides map[string]string
	goArch          string
	ownedFields     map[string]map[string]bool
}

const largeFileLineLimit = 5000

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
		analyzePackage(&findings, pkg)
	}
	return findings, nil
}

func analyzePackage(findings *[]ASTFinding, pkg goListPackage) {
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
	// Cgo's compiler-generated inputs carry declarations needed by ordinary Go
	// files. They participate in type checking, but are not audited as source.
	for _, file := range compilerExtraFiles(pkg) {
		path := packageFilePath(pkg, file)
		parsed, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err == nil {
			parsedFiles[file] = parsed
		}
	}
	typeInfo := checkPackageTypes(pkg, fset, parsedFiles)
	if typeInfo == nil {
		loc := astLocation{pkg: pkg.ImportPath, file: pkg.Dir, line: 1}
		msg := "type checking unavailable (type errors or export-data/toolchain skew); type-dependent rules skipped"
		*findings = append(*findings, astFinding("type-info-unavailable", "medium", loc, "", msg))
	}
	testInfos := checkTestTypes(pkg, fset, parsedFiles)
	warnedTestGroups := map[string]bool{}
	for _, file := range files {
		if info, ok := testInfos[file]; ok && info == nil {
			group := parsedFiles[file].Name.Name
			if !warnedTestGroups[group] {
				loc := astLocation{pkg: pkg.ImportPath, file: filepath.Join(pkg.Dir, file), line: 1}
				msg := "test type checking unavailable (type errors or export-data/toolchain skew); type-dependent rules skipped"
				*findings = append(*findings, astFinding("type-info-unavailable", "medium", loc, "", msg))
				warnedTestGroups[group] = true
			}
		}
	}
	releases := collectReleaseParams(parsedFiles, pkg.GoFiles)
	cancelers := collectCancelParams(pkg.ImportPath, parsedFiles, pkg.GoFiles, typeInfo)
	params := analyzePackageFileParams{
		pkg: pkg, fset: fset, typeInfo: typeInfo, releases: releases, cancelers: cancelers,
		signals: signals,
	}
	for _, file := range files {
		if parsed := parsedFiles[file]; parsed != nil {
			params.file = file
			params.parsed = parsed
			if info, ok := testInfos[file]; ok {
				params.typeInfo = info
			} else {
				params.typeInfo = typeInfo
			}
			analyzePackageFile(findings, params)
		}
	}
	appendPackageSignalFindings(findings, pkg, signals)
}

type analyzePackageFileParams struct {
	pkg       goListPackage
	file      string
	parsed    *ast.File
	fset      *token.FileSet
	typeInfo  *types.Info
	releases  map[string]map[int]bool
	cancelers map[string]map[int]bool
	signals   *packageSignals
}

func analyzePackageFile(findings *[]ASTFinding, params analyzePackageFileParams) {
	path := filepath.Join(params.pkg.Dir, params.file)
	// A //diago:ignore file is excluded entirely: no findings or signals.
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
	appendLargeFileFinding(findings, ctx, lineCount)
	findCommentDebt(findings, params.pkg.ImportPath, path, params.fset, params.parsed)

	analyzeExtraFile(findings, params.signals, ctx, params.parsed)
	for _, decl := range params.parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		analyzeFunc(findings, ctx, fn)
	}
}

func checkTestTypes(pkg goListPackage, fset *token.FileSet, parsed map[string]*ast.File) map[string]*types.Info {
	out := map[string]*types.Info{}
	groups := map[string][]string{}
	for _, name := range append(append([]string{}, pkg.TestGoFiles...), pkg.XTestGoFiles...) {
		file := parsed[name]
		if file == nil {
			continue
		}
		groups[file.Name.Name] = append(groups[file.Name.Name], name)
	}
	for group, names := range groups {
		checkPkg := pkg
		checkPkg.importOverrides = pkg.testExports
		checkNames := append([]string{}, names...)
		if group == pkg.Name {
			// Internal tests share a package scope with production declarations.
			checkNames = append(append([]string{}, pkg.GoFiles...), names...)
		} else {
			checkPkg.ImportPath += "_test"
			checkPkg.CompiledGoFiles = nil
		}
		info := checkFilesTypes(checkPkg, fset, parsed, checkNames)
		for _, name := range names {
			// Retain nil entries: production info cannot describe test AST nodes.
			out[name] = info
		}
	}
	return out
}

func checkFilesTypes(pkg goListPackage, fset *token.FileSet, parsed map[string]*ast.File, names []string) *types.Info {
	names = append(append([]string{}, names...), compilerExtraFiles(pkg)...)
	files := make([]*ast.File, 0, len(names))
	for _, name := range names {
		file := parsed[name]
		if file == nil {
			return nil
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return newTypesInfo()
	}
	version := ""
	if pkg.GoVersion != "" {
		version = "go" + strings.TrimPrefix(pkg.GoVersion, "go")
	}
	arch := pkg.goArch
	if arch == "" {
		arch = runtime.GOARCH
	}
	conf := types.Config{
		Importer:  &exportDataImporter{fset: fset, exports: pkg.exports, overrides: pkg.importOverrides, imports: make(map[string]*types.Package)},
		Error:     func(error) {},
		GoVersion: version,
		Sizes:     types.SizesFor("gc", arch),
	}
	info := newTypesInfo()
	if detail := tryTypeCheck(&conf, pkg.ImportPath, fset, files, info); detail != nil {
		return nil
	}
	return info
}

func packageFilePath(pkg goListPackage, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(pkg.Dir, name)
}

func compilerExtraFiles(pkg goListPackage) []string {
	source := map[string]bool{}
	for _, names := range [][]string{pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
		for _, name := range names {
			source[packageFilePath(pkg, name)] = true
		}
	}
	var extra []string
	for _, name := range pkg.CompiledGoFiles {
		if !source[packageFilePath(pkg, name)] {
			extra = append(extra, name)
			source[packageFilePath(pkg, name)] = true
		}
	}
	return extra
}

func newTypesInfo() *types.Info {
	return &types.Info{
		Types:        make(map[ast.Expr]types.TypeAndValue),
		Defs:         make(map[*ast.Ident]types.Object),
		Uses:         make(map[*ast.Ident]types.Object),
		Selections:   make(map[*ast.SelectorExpr]*types.Selection),
		FileVersions: make(map[*ast.File]string),
	}
}

func checkPackageTypes(pkg goListPackage, fset *token.FileSet, parsed map[string]*ast.File) *types.Info {
	return checkFilesTypes(pkg, fset, parsed, pkg.GoFiles)
}

// exportDataImporter uses the current x/tools decoder, rather than the
// standard-library importer bundled with the Go version that built Diago.
// This lets Diago inspect export data written by a newer target toolchain.
type exportDataImporter struct {
	fset      *token.FileSet
	exports   map[string]string
	overrides map[string]string // test-variant export data, preferred over exports
	imports   map[string]*types.Package
}

func (imp *exportDataImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if pkg := imp.imports[path]; pkg != nil && pkg.Complete() {
		return pkg, nil
	}
	export := imp.overrides[path]
	if export == "" {
		export = imp.exports[path]
	}
	if export == "" {
		return nil, fmt.Errorf("no export data for %s", path)
	}
	file, err := os.Open(export)
	if err != nil {
		return nil, fmt.Errorf("open export data for %s: %w", path, err)
	}
	defer file.Close()
	reader, err := gcexportdata.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("locate export data for %s: %w", path, err)
	}
	pkg, err := gcexportdata.Read(reader, imp.fset, imp.imports, path)
	if err != nil {
		return nil, fmt.Errorf("decode export data for %s: %w", path, err)
	}
	return pkg, nil
}

// tryTypeCheck returns type errors and export-data skew panics as values;
// unexpected panics are re-raised. Partial information must not be used as if
// every identifier and expression had been successfully checked.
func tryTypeCheck(conf *types.Config, importPath string, fset *token.FileSet, files []*ast.File, info *types.Info) (skewDetail any) {
	defer func() {
		if r := recover(); r != nil {
			if isImporterSkewPanic(r) {
				skewDetail = r
				return
			}
			panic(r)
		}
	}()
	_, err := conf.Check(importPath, fset, files, info)
	if err != nil {
		return err
	}
	return nil
}

func isImporterSkewPanic(r any) bool {
	var msg string
	switch v := r.(type) {
	case error:
		msg = v.Error()
	case string:
		msg = v
	case fmt.Stringer:
		msg = v.String()
	default:
		return false
	}
	return strings.Contains(msg, "export data") || strings.Contains(msg, "cannot decode")
}

func appendLargeFileFinding(findings *[]ASTFinding, ctx astContext, lineCount int) {
	if ctx.generated || ctx.isTest || lineCount <= largeFileLineLimit {
		return
	}
	loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: 1}
	msg := fmt.Sprintf("file has %d lines", lineCount)
	*findings = append(*findings, astFinding("large-file", severity(lineCount, largeFileLineLimit/2, largeFileLineLimit, largeFileLineLimit*3/2), loc, "", msg))
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
		if pkg.Module != nil {
			pkg.GoVersion = pkg.Module.GoVersion
			if pkg.GoVersion == "" {
				pkg.GoVersion = "1.16" // go.mod's default when no go directive exists.
			}
		}
		pkgs = append(pkgs, pkg)
	}
	envCmd := exec.Command("go", "env", "-json", "GOARCH", "GOVERSION")
	envCmd.Dir = workDir
	envJSON, err := envCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env target: %w", err)
	}
	var targetEnv struct{ GOARCH, GOVERSION string }
	if err := json.Unmarshal(envJSON, &targetEnv); err != nil {
		return nil, fmt.Errorf("decoding go env target: %w", err)
	}
	compiled := listExportPackages(workDir, target)
	exports := make(map[string]string)
	production := make(map[string]goListPackage)
	testOverrides := make(map[string]map[string]string)
	for _, pkg := range compiled {
		if pkg.ForTest == "" {
			production[pkg.ImportPath] = pkg
			if pkg.Export != "" {
				exports[pkg.ImportPath] = pkg.Export
			}
		} else if pkg.Export != "" {
			if testOverrides[pkg.ForTest] == nil {
				testOverrides[pkg.ForTest] = make(map[string]string)
			}
			// Archives retain canonical paths, unlike synthetic go-list IDs.
			path, _, _ := strings.Cut(pkg.ImportPath, " [")
			testOverrides[pkg.ForTest][path] = pkg.Export
		}
	}
	for i := range pkgs {
		pkgs[i].goArch = targetEnv.GOARCH
		if pkgs[i].Module == nil {
			// GOPATH and standard-library packages use the selected toolchain's
			// language version rather than a module's go directive.
			version := strings.TrimPrefix(targetEnv.GOVERSION, "go")
			if validGoVersion(version) {
				pkgs[i].GoVersion = version
			}
		}
		pkgs[i].exports = exports
		pkgs[i].testExports = testOverrides[pkgs[i].ImportPath]
		pkgs[i].CompiledGoFiles = production[pkgs[i].ImportPath].CompiledGoFiles
	}
	return pkgs, nil
}

func listExportPackages(workDir, target string) []goListPackage {
	cmd := exec.Command("go", "list", "-e", "-deps", "-test", "-compiled", "-export", "-json", target)
	cmd.Dir = workDir
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var pkgs []goListPackage
	for dec.More() {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil
		}
		pkgs = append(pkgs, pkg)
	}
	return pkgs
}

func analyzeFunc(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl) {
	name := funcName(fn)
	if ctx.isTest {
		appendLongTestNameFinding(findings, ctx, fn, name)
	} else {
		appendFunctionMetricFindings(findings, ctx, fn, name)
	}
	findDangerousCalls(findings, ctx, fn, name)
	scanLoops(loopEnv{findings: findings, ctx: loopContext{pkg: ctx.pkg.ImportPath, path: ctx.path, fset: ctx.fset, fn: name, types: ctx.types, file: ctx.file, decl: fn, goVersion: sourceGoVersion(ctx)}, shade: map[string]bool{}}, fn.Body)
}

// sourceGoVersion follows go/types' per-file language version rules, including
// build constraints that opt a file into (or out of) Go 1.22 loop semantics.
func sourceGoVersion(ctx astContext) string {
	if ctx.file != nil {
		if ctx.types != nil && ctx.types.FileVersions[ctx.file] != "" {
			return ctx.types.FileVersions[ctx.file]
		}
		if ctx.file.GoVersion != "" {
			if goVersionAtLeast(ctx.file.GoVersion, "1.21") {
				return ctx.file.GoVersion
			}
			return "1.21"
		}
	}
	return ctx.pkg.GoVersion
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
	if descriptor, ok := ruleCatalog[rule]; ok && descriptor.Kind == "analysis" {
		sev = descriptor.Severity
	}
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
	lines := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}
