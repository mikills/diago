package diago

import (
	"fmt"
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

type iteratorErrorRule struct {
	rule        string
	packagePath string
	receiver    string
	iterate     string
	message     string
}

var iteratorErrorRules = []iteratorErrorRule{
	{
		rule: "sql-rows-err", packagePath: "database/sql", receiver: "Rows", iterate: "Next",
		message: "sql.Rows.Err is not checked after iteration",
	},
	{
		rule: "scanner-err", packagePath: "bufio", receiver: "Scanner", iterate: "Scan",
		message: "bufio.Scanner.Err is not checked after scanning",
	},
}

type iteratorLoop struct {
	rule iteratorErrorRule
	obj  types.Object
	loop *ast.ForStmt
}

// IteratorErrorAnalyzer reports unchecked terminal errors after standard
// library iterator loops. Its diagnostic categories remain Diago's stable
// sql-rows-err and scanner-err rule IDs.
var IteratorErrorAnalyzer = &analysis.Analyzer{
	Name: "diagoiterators",
	Doc:  "report unchecked terminal iterator errors",
	Run:  runIteratorErrorAnalyzer,
}

func runIteratorErrorAnalyzer(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		path := pass.Fset.PositionFor(file.Pos(), false).Filename
		if isIgnoredFile(file) || isGeneratedFile(path, file) {
			continue
		}
		ctx := astContext{path: path, fset: pass.Fset, file: file, types: pass.TypesInfo}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			findIteratorErrorIssues(ctx, fn, func(loop iteratorLoop) {
				pass.Report(analysis.Diagnostic{
					Pos:      loop.loop.Pos(),
					End:      loop.loop.End(),
					Category: loop.rule.rule,
					Message:  iteratorErrorMessage(loop.rule),
				})
			})
		}
	}
	return nil, nil
}

// findIteratorErrorSignals checks direct iterator loops against later sibling
// statements. It deliberately favors precision: aliases, helper calls, and
// arbitrary control-flow proofs are not treated as terminal checks.
func findIteratorErrorSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	findIteratorErrorIssues(ctx, fn, func(loop iteratorLoop) {
		*findings = append(*findings, astFinding(loop.rule.rule, "high", nodeLocation(ctx, loop.loop), name, iteratorErrorMessage(loop.rule)))
	})
}

type iteratorErrorReporter func(iteratorLoop)

func findIteratorErrorIssues(ctx astContext, fn *ast.FuncDecl, report iteratorErrorReporter) {
	findIteratorErrorIssuesInBlock(ctx, fn.Body.List, nil, report)
}

func findIteratorErrorIssuesInBlock(ctx astContext, statements, trailing []ast.Stmt, report iteratorErrorReporter) {
	for index, statement := range statements {
		following := append(append([]ast.Stmt{}, statements[index+1:]...), trailing...)
		if loop, ok := iteratorLoopForStatement(ctx, statement); ok && !hasRuleIgnoreDirective(ctx, loop.loop, loop.rule.rule) && !iteratorErrorChecked(ctx, following, loop) {
			report(loop)
		}
		findIteratorErrorIssuesInNestedStatement(ctx, statement, following, report)
	}
}

func findIteratorErrorIssuesInNestedStatement(ctx astContext, statement ast.Stmt, trailing []ast.Stmt, report iteratorErrorReporter) {
	switch statement := statement.(type) {
	case *ast.BlockStmt:
		findIteratorErrorIssuesInBlock(ctx, statement.List, trailing, report)
	case *ast.IfStmt:
		findIteratorErrorIssuesInIf(ctx, statement, trailing, report)
	case *ast.SwitchStmt:
		findIteratorErrorIssuesInCases(ctx, statement.Body.List, trailing, report)
	case *ast.TypeSwitchStmt:
		findIteratorErrorIssuesInCases(ctx, statement.Body.List, trailing, report)
	case *ast.ForStmt:
		findIteratorErrorIssuesInBlock(ctx, statement.Body.List, trailing, report)
	case *ast.RangeStmt:
		findIteratorErrorIssuesInBlock(ctx, statement.Body.List, trailing, report)
	case *ast.LabeledStmt:
		findIteratorErrorIssuesInNestedStatement(ctx, statement.Stmt, trailing, report)
	}
}

func findIteratorErrorIssuesInIf(ctx astContext, statement *ast.IfStmt, trailing []ast.Stmt, report iteratorErrorReporter) {
	findIteratorErrorIssuesInBlock(ctx, statement.Body.List, trailing, report)
	if statement.Else != nil {
		findIteratorErrorIssuesInNestedStatement(ctx, statement.Else, trailing, report)
	}
}

func findIteratorErrorIssuesInCases(ctx astContext, clauses []ast.Stmt, trailing []ast.Stmt, report iteratorErrorReporter) {
	for _, clause := range clauses {
		caseClause, ok := clause.(*ast.CaseClause)
		if ok {
			findIteratorErrorIssuesInBlock(ctx, caseClause.Body, trailing, report)
		}
	}
}

func iteratorErrorMessage(rule iteratorErrorRule) string {
	return fmt.Sprintf("%s; call %s.Err() after the loop", rule.message, iteratorReceiverName(rule))
}

func iteratorLoopForStatement(ctx astContext, statement ast.Stmt) (iteratorLoop, bool) {
	loop, ok := statement.(*ast.ForStmt)
	if !ok {
		return iteratorLoop{}, false
	}
	call, ok := loop.Cond.(*ast.CallExpr)
	if !ok {
		return iteratorLoop{}, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return iteratorLoop{}, false
	}
	ident, ok := selector.X.(*ast.Ident)
	if !ok || ctx.types == nil {
		return iteratorLoop{}, false
	}
	method, ok := calledObject(ctx, call).(*types.Func)
	if !ok {
		return iteratorLoop{}, false
	}
	for _, rule := range iteratorErrorRules {
		if methodMatchesIteratorRule(method, rule) {
			return iteratorLoop{rule: rule, obj: ctx.types.Uses[ident], loop: loop}, true
		}
	}
	return iteratorLoop{}, false
}

func methodMatchesIteratorRule(method *types.Func, rule iteratorErrorRule) bool {
	if method == nil || method.Pkg() == nil || method.Pkg().Path() != rule.packagePath || method.Name() != rule.iterate {
		return false
	}
	return functionReceiverName(method) == rule.receiver
}

func iteratorErrorChecked(ctx astContext, statements []ast.Stmt, iterator iteratorLoop) bool {
	for _, statement := range statements {
		if statementChecksIteratorError(ctx, statement, iterator) {
			return true
		}
		if _, ok := statement.(*ast.ReturnStmt); ok {
			return false
		}
	}
	return false
}

func statementChecksIteratorError(ctx astContext, statement ast.Stmt, iterator iteratorLoop) bool {
	switch statement := statement.(type) {
	case *ast.ReturnStmt:
		return expressionsContainIteratorErr(ctx, statement.Results, iterator)
	case *ast.IfStmt:
		if expressionContainsIteratorErr(ctx, statement.Cond, iterator) {
			return true
		}
		return ifInitChecksIteratorErr(ctx, statement, iterator)
	default:
		return false
	}
}

func ifInitChecksIteratorErr(ctx astContext, statement *ast.IfStmt, iterator iteratorLoop) bool {
	assign, ok := statement.Init.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 || !expressionContainsIteratorErr(ctx, assign.Rhs[0], iterator) {
		return false
	}
	assigned, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return false
	}
	return expressionUsesObject(ctx, statement.Cond, assigned)
}

func expressionsContainIteratorErr(ctx astContext, expressions []ast.Expr, iterator iteratorLoop) bool {
	for _, expression := range expressions {
		if expressionContainsIteratorErr(ctx, expression, iterator) {
			return true
		}
	}
	return false
}

func expressionContainsIteratorErr(ctx astContext, expression ast.Expr, iterator iteratorLoop) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if found {
			return false
		}
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if ok && isIteratorErrCall(ctx, call, iterator) {
			found = true
		}
		return true
	})
	return found
}

func isIteratorErrCall(ctx astContext, call *ast.CallExpr, iterator iteratorLoop) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Err" {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	if !ok || ctx.types == nil || ctx.types.Uses[ident] != iterator.obj {
		return false
	}
	method, ok := calledObject(ctx, call).(*types.Func)
	if !ok || method.Name() != "Err" || method.Pkg() == nil || method.Pkg().Path() != iterator.rule.packagePath {
		return false
	}
	return functionReceiverName(method) == iterator.rule.receiver
}

func expressionUsesObject(ctx astContext, expression ast.Expr, definition *ast.Ident) bool {
	if ctx.types == nil {
		return false
	}
	obj := ctx.types.Defs[definition]
	if obj == nil {
		obj = ctx.types.Uses[definition]
	}
	if obj == nil {
		return false
	}
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if ok && ctx.types.Uses[ident] == obj {
			found = true
		}
		return !found
	})
	return found
}

func iteratorReceiverName(rule iteratorErrorRule) string {
	if rule.receiver == "Rows" {
		return "rows"
	}
	return "scanner"
}
