package diago

import (
	"fmt"
	"go/ast"
	"go/types"
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

// findIteratorErrorSignals checks direct iterator loops against later sibling
// statements. It deliberately favors precision: aliases, helper calls, and
// arbitrary control-flow proofs are not treated as terminal checks.
func findIteratorErrorSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	findIteratorErrorSignalsInBlock(findings, ctx, fn.Body.List, nil, name)
}

func findIteratorErrorSignalsInBlock(findings *[]ASTFinding, ctx astContext, statements, trailing []ast.Stmt, name string) {
	for index, statement := range statements {
		following := append(append([]ast.Stmt{}, statements[index+1:]...), trailing...)
		if loop, ok := iteratorLoopForStatement(ctx, statement); ok && !hasRuleIgnoreDirective(ctx, loop.loop, loop.rule.rule) && !iteratorErrorChecked(ctx, following, loop) {
			message := fmt.Sprintf("%s; call %s.Err() after the loop", loop.rule.message, iteratorReceiverName(loop.rule))
			*findings = append(*findings, astFinding(loop.rule.rule, "high", nodeLocation(ctx, loop.loop), name, message))
		}
		findIteratorErrorSignalsInNestedStatement(findings, ctx, statement, following, name)
	}
}

func findIteratorErrorSignalsInNestedStatement(findings *[]ASTFinding, ctx astContext, statement ast.Stmt, trailing []ast.Stmt, name string) {
	switch statement := statement.(type) {
	case *ast.BlockStmt:
		findIteratorErrorSignalsInBlock(findings, ctx, statement.List, trailing, name)
	case *ast.IfStmt:
		findIteratorErrorSignalsInIf(findings, ctx, statement, trailing, name)
	case *ast.SwitchStmt:
		findIteratorErrorSignalsInCases(findings, ctx, statement.Body.List, trailing, name)
	case *ast.TypeSwitchStmt:
		findIteratorErrorSignalsInCases(findings, ctx, statement.Body.List, trailing, name)
	case *ast.ForStmt:
		findIteratorErrorSignalsInBlock(findings, ctx, statement.Body.List, trailing, name)
	case *ast.RangeStmt:
		findIteratorErrorSignalsInBlock(findings, ctx, statement.Body.List, trailing, name)
	case *ast.LabeledStmt:
		findIteratorErrorSignalsInNestedStatement(findings, ctx, statement.Stmt, trailing, name)
	}
}

func findIteratorErrorSignalsInIf(findings *[]ASTFinding, ctx astContext, statement *ast.IfStmt, trailing []ast.Stmt, name string) {
	findIteratorErrorSignalsInBlock(findings, ctx, statement.Body.List, trailing, name)
	if statement.Else != nil {
		findIteratorErrorSignalsInNestedStatement(findings, ctx, statement.Else, trailing, name)
	}
}

func findIteratorErrorSignalsInCases(findings *[]ASTFinding, ctx astContext, clauses []ast.Stmt, trailing []ast.Stmt, name string) {
	for _, clause := range clauses {
		caseClause, ok := clause.(*ast.CaseClause)
		if ok {
			findIteratorErrorSignalsInBlock(findings, ctx, caseClause.Body, trailing, name)
		}
	}
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
	if !ok || ctx.types.Uses[ident] != iterator.obj {
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
