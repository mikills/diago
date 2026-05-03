package diago

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

type packageSignals struct {
	strings  map[string][]astLocation
	numbers  map[string][]astLocation
	exported int
	tests    int
}

func newPackageSignals(pkg goListPackage) *packageSignals {
	return &packageSignals{
		strings: map[string][]astLocation{},
		numbers: map[string][]astLocation{},
		tests:   len(pkg.TestGoFiles),
	}
}

func analyzeExtraFile(findings *[]ASTFinding, signals *packageSignals, ctx astContext, file *ast.File) {
	collectExportedSurface(signals, file)
	collectLiteralSignals(signals, ctx, file)
	findExtraFunctionSignals(findings, ctx, file)
}

func appendPackageSignalFindings(findings *[]ASTFinding, pkg goListPackage, signals *packageSignals) {
	if signals.tests == 0 && signals.exported > 10 {
		loc := astLocation{pkg: pkg.ImportPath, file: pkg.Dir, line: 1}
		msg := fmt.Sprintf("package has %d exported funcs/types and no test files", signals.exported)
		*findings = append(*findings, astFinding("untested-exported-surface", "high", loc, "", msg))
	}
	for lit, locs := range signals.strings {
		if len(locs) > 5 {
			msg := fmt.Sprintf("string literal %q appears %d times", lit, len(locs))
			*findings = append(*findings, astFinding("duplicate-string-literal", "low", locs[0], "", msg))
		}
	}
	for lit, locs := range signals.numbers {
		if len(locs) > 5 {
			msg := fmt.Sprintf("numeric literal %s appears %d times", lit, len(locs))
			*findings = append(*findings, astFinding("magic-number", "low", locs[0], "", msg))
		}
	}
}

func collectExportedSurface(signals *packageSignals, file *ast.File) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Name.IsExported() {
				signals.exported++
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
					signals.exported++
				}
			}
		}
	}
}

func collectLiteralSignals(signals *packageSignals, ctx astContext, file *ast.File) {
	if ctx.isTest {
		return
	}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok {
			return true
		}
		loc := astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: ctx.fset.Position(lit.Pos()).Line}
		switch lit.Kind {
		case token.STRING:
			value, err := strconv.Unquote(lit.Value)
			if err == nil && shouldTrackStringLiteral(value) {
				signals.strings[value] = append(signals.strings[value], loc)
			}
		case token.INT, token.FLOAT:
			if !isAllowedNumberLiteral(lit.Value) {
				signals.numbers[lit.Value] = append(signals.numbers[lit.Value], loc)
			}
		}
		return true
	})
}

func findExtraFunctionSignals(findings *[]ASTFinding, ctx astContext, file *ast.File) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := funcName(fn)
		if ctx.isTest {
			continue
		}
		findErrorHandlingSignals(findings, ctx, fn, name)
		findRecoverMisuse(findings, ctx, fn, name)
		findContextAndTimeoutSignals(findings, ctx, fn, name)
		findResourceCloseSignals(findings, ctx, fn, name)
		findMaintainabilitySignals(findings, ctx, fn, name)
	}
}

func findErrorHandlingSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	returnsErr := returnsError(fn)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok {
			appendIgnoredCallFinding(findings, ctx, name, assign)
		}
		if stmt, ok := n.(*ast.IfStmt); ok {
			appendErrorBranchFinding(findings, ctx, name, stmt, returnsErr)
		}
		return true
	})
}

func appendIgnoredCallFinding(findings *[]ASTFinding, ctx astContext, name string, stmt *ast.AssignStmt) {
	if len(stmt.Lhs) != 1 || len(stmt.Rhs) != 1 {
		return
	}
	ident, ok := stmt.Lhs[0].(*ast.Ident)
	if !ok || ident.Name != "_" || !isCallExpr(stmt.Rhs[0]) {
		return
	}
	loc := nodeLocation(ctx, stmt)
	*findings = append(*findings, astFinding("ignored-call-result", "medium", loc, name, "call result assigned to blank identifier"))
}

func appendErrorBranchFinding(findings *[]ASTFinding, ctx astContext, name string, stmt *ast.IfStmt, returnsErr bool) {
	if !isErrNotNil(stmt.Cond) {
		return
	}
	loc := nodeLocation(ctx, stmt)
	if len(stmt.Body.List) == 0 {
		*findings = append(*findings, astFinding("empty-error-branch", "high", loc, name, "if err != nil branch is empty"))
		return
	}
	if returnsErr && branchSwallowsError(stmt.Body) {
		*findings = append(*findings, astFinding("swallowed-error", "high", loc, name, "error branch returns without propagating err"))
	}
}

func findRecoverMisuse(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	findRecoverCalls(findings, ctx, name, fn.Body, false)
}

func findRecoverCalls(findings *[]ASTFinding, ctx astContext, name string, n ast.Node, inDefer bool) {
	ast.Inspect(n, func(child ast.Node) bool {
		if child == nil || child == n {
			return true
		}
		if d, ok := child.(*ast.DeferStmt); ok {
			findRecoverCalls(findings, ctx, name, d.Call, true)
			return false
		}
		call, ok := child.(*ast.CallExpr)
		if ok && isIdentCall(call, "recover") && !inDefer {
			*findings = append(*findings, astFinding("recover-outside-defer", "high", nodeLocation(ctx, call), name, "recover called outside deferred function"))
		}
		return true
	})
}

func findContextAndTimeoutSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	if shouldHaveContext(fn, name) && !hasContextParam(fn) {
		loc := nodeLocation(ctx, fn)
		*findings = append(*findings, astFinding("missing-context-param", "medium", loc, name, "service/exported function appears to perform I/O without context.Context parameter"))
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if isHTTPClientLiteralWithoutTimeout(x) {
				*findings = append(*findings, astFinding("http-client-without-timeout", "high", nodeLocation(ctx, x), name, "http.Client literal has no Timeout"))
			}
		case *ast.CallExpr:
			if isSelectorCall(x, "context", "Background") || isSelectorCall(x, "context", "TODO") {
				*findings = append(*findings, astFinding("background-context", "medium", nodeLocation(ctx, x), name, "context.Background/TODO used inside function"))
			}
		}
		return true
	})
}

func findResourceCloseSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	resources, closed := collectResourceCloseSignals(ctx, fn)
	for nameVar, loc := range resources {
		if !closed[nameVar] {
			msg := fmt.Sprintf("%s is opened/created but Close is not called in the function", nameVar)
			*findings = append(*findings, astFinding("resource-not-closed", "high", loc, name, msg))
		}
	}
}

func collectResourceCloseSignals(ctx astContext, fn *ast.FuncDecl) (map[string]astLocation, map[string]bool) {
	resources := map[string]astLocation{}
	closed := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if assign, ok := n.(*ast.AssignStmt); ok {
			collectClosableAssignments(ctx, assign, resources)
		}
		if call, ok := n.(*ast.CallExpr); ok {
			markClosedResource(call, closed)
		}
		return true
	})
	return resources, closed
}

func collectClosableAssignments(ctx astContext, stmt *ast.AssignStmt, resources map[string]astLocation) {
	for i, rhs := range stmt.Rhs {
		if i >= len(stmt.Lhs) || !callCreatesClosable(rhs) {
			continue
		}
		if id, ok := stmt.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
			resources[id.Name] = nodeLocation(ctx, stmt)
		}
	}
}

func markClosedResource(call *ast.CallExpr, closed map[string]bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Close" {
		return
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		closed[id.Name] = true
	}
}

func findMaintainabilitySignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	state := maintainabilityState{lineCount: ctx.fset.Position(fn.End()).Line - ctx.fset.Position(fn.Pos()).Line + 1}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		state.visit(findings, ctx, fn, name, n)
		return true
	})
	if state.returns > 10 {
		msg := fmt.Sprintf("function has %d return statements", state.returns)
		*findings = append(*findings, astFinding("too-many-returns", "medium", nodeLocation(ctx, fn), name, msg))
	}
}

type maintainabilityState struct {
	lineCount int
	returns   int
	anonDepth int
}

func (s *maintainabilityState) visit(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string, n ast.Node) {
	if ret, ok := n.(*ast.ReturnStmt); ok {
		s.returns++
		appendNakedReturnFinding(findings, maintainabilityContext{ctx: ctx, fn: fn, name: name, lineCount: s.lineCount}, ret)
	}
	if stmt, ok := n.(*ast.SwitchStmt); ok {
		appendLongSwitchFinding(findings, ctx, name, stmt)
	}
	if stmt, ok := n.(*ast.IfStmt); ok {
		appendLongIfChainFinding(findings, ctx, name, stmt)
	}
	if lit, ok := n.(*ast.CompositeLit); ok {
		appendLargeCompositeFinding(findings, ctx, name, lit)
	}
	if lit, ok := n.(*ast.FuncLit); ok {
		s.anonDepth++
		appendDeepAnonymousFinding(findings, ctx, name, lit, s.anonDepth)
	}
}

type maintainabilityContext struct {
	ctx       astContext
	fn        *ast.FuncDecl
	name      string
	lineCount int
}

func appendNakedReturnFinding(findings *[]ASTFinding, mctx maintainabilityContext, ret *ast.ReturnStmt) {
	if len(ret.Results) == 0 && hasNamedResults(mctx.fn) && mctx.lineCount > 20 {
		*findings = append(*findings, astFinding("naked-return", "medium", nodeLocation(mctx.ctx, ret), mctx.name, "naked return in function with named results"))
	}
}

func appendLongSwitchFinding(findings *[]ASTFinding, ctx astContext, name string, stmt *ast.SwitchStmt) {
	if len(stmt.Body.List) <= 10 {
		return
	}
	msg := fmt.Sprintf("switch has %d cases", len(stmt.Body.List))
	*findings = append(*findings, astFinding("long-switch", "medium", nodeLocation(ctx, stmt), name, msg))
}

func appendLongIfChainFinding(findings *[]ASTFinding, ctx astContext, name string, stmt *ast.IfStmt) {
	branches := countElseIfChain(stmt)
	if branches <= 5 {
		return
	}
	msg := fmt.Sprintf("if/else chain has %d branches", branches)
	*findings = append(*findings, astFinding("long-if-chain", "medium", nodeLocation(ctx, stmt), name, msg))
}

func appendLargeCompositeFinding(findings *[]ASTFinding, ctx astContext, name string, lit *ast.CompositeLit) {
	if len(lit.Elts) <= 40 {
		return
	}
	msg := fmt.Sprintf("composite literal has %d elements", len(lit.Elts))
	*findings = append(*findings, astFinding("large-composite-literal", "medium", nodeLocation(ctx, lit), name, msg))
}

func appendDeepAnonymousFinding(findings *[]ASTFinding, ctx astContext, name string, lit *ast.FuncLit, depth int) {
	if depth <= 2 {
		return
	}
	*findings = append(*findings, astFinding("deep-anonymous-function", "medium", nodeLocation(ctx, lit), name, "anonymous function nested more than 2 levels"))
}

func nodeLocation(ctx astContext, n ast.Node) astLocation {
	return astLocation{pkg: ctx.pkg.ImportPath, file: ctx.path, line: ctx.fset.Position(n.Pos()).Line}
}

func isCallExpr(e ast.Expr) bool { _, ok := e.(*ast.CallExpr); return ok }

func isErrNotNil(e ast.Expr) bool {
	b, ok := e.(*ast.BinaryExpr)
	if !ok || b.Op != token.NEQ {
		return false
	}
	return isIdentName(b.X, "err") && isIdentName(b.Y, "nil") || isIdentName(b.X, "nil") && isIdentName(b.Y, "err")
}

func branchSwallowsError(body *ast.BlockStmt) bool {
	if len(body.List) != 1 {
		return false
	}
	ret, ok := body.List[0].(*ast.ReturnStmt)
	if !ok {
		return false
	}
	if len(ret.Results) == 0 {
		return true
	}
	for _, result := range ret.Results {
		if !isIdentName(result, "nil") {
			return false
		}
	}
	return len(ret.Results) > 0
}

func returnsError(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if isErrorType(field.Type) {
			return true
		}
	}
	return false
}

func isErrorType(expr ast.Expr) bool { return isIdentName(expr, "error") }

func hasNamedResults(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, field := range fn.Type.Results.List {
		if len(field.Names) > 0 {
			return true
		}
	}
	return false
}

func isIdentName(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}

func isIdentCall(call *ast.CallExpr, name string) bool {
	ident, ok := call.Fun.(*ast.Ident)
	return ok && ident.Name == name
}

func isSelectorCall(call *ast.CallExpr, recv, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	return isIdentName(sel.X, recv)
}

func shouldHaveContext(fn *ast.FuncDecl, name string) bool {
	if !ast.IsExported(fn.Name.Name) && !strings.Contains(name, "Service") && !strings.Contains(name, "Store") && !strings.Contains(name, "Client") {
		return false
	}
	foundIO := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callCreatesClosable(call) || isLikelyIOCall(call) {
			foundIO = true
		}
		return true
	})
	return foundIO
}

func hasContextParam(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if selector, ok := p.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Context" && isIdentName(selector.X, "context") {
			return true
		}
	}
	return false
}

func isLikelyIOCall(call *ast.CallExpr) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		switch sel.Sel.Name {
		case "Do", "Get", "Post", "Query", "QueryRow", "Exec", "Begin", "Open", "ListenAndServe":
			return true
		}
	}
	return false
}

func isHTTPClientLiteralWithoutTimeout(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Client" || !isIdentName(sel.X, "http") {
		return false
	}
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok && isIdentName(kv.Key, "Timeout") {
			return false
		}
	}
	return true
}

func callCreatesClosable(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	if isSelectorCall(call, "os", "Open") || isSelectorCall(call, "http", "Get") || isSelectorCall(call, "http", "Post") {
		return true
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		switch sel.Sel.Name {
		case "Do", "Query", "QueryContext", "Open", "OpenFile":
			return true
		}
	}
	return false
}

func countElseIfChain(stmt *ast.IfStmt) int {
	count := 1
	for {
		next, ok := stmt.Else.(*ast.IfStmt)
		if !ok {
			return count
		}
		count++
		stmt = next
	}
}

func shouldTrackStringLiteral(s string) bool {
	if len(s) < 3 || looksLikeFormatString(s) {
		return false
	}
	switch s {
	case "low", "medium", "high", "critical", "text", "json", "test", "audit", "deps", "coverage", "ast":
		return false
	default:
		return true
	}
}

func looksLikeFormatString(s string) bool { return strings.Contains(s, "%") }

func isAllowedNumberLiteral(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	return s == "0" || s == "1" || s == "2" || s == "5" || s == "10" || s == "0644"
}

func isLargeNumberLiteral(s string) bool {
	clean := strings.Trim(strings.ToLower(s), "_")
	if v, err := strconv.ParseFloat(clean, 64); err == nil {
		return v > 1000 || v < -1000
	}
	return false
}
