package diago

import (
	"go/ast"
	"go/types"
	"strings"
)

const contextPackagePath = "context"

// shouldHaveContext only reports exported APIs with typed evidence of
// cancellable work. Unresolved calls and convenience APIs are not inferred
// from broad method names.
func shouldHaveContext(ctx astContext, fn *ast.FuncDecl) bool {
	if !ast.IsExported(fn.Name.Name) {
		return false
	}
	sibling := contextSiblingForDeclaredFunction(ctx, fn)
	foundIO := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, nested := n.(*ast.FuncLit); nested {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if ok && !isCallToSibling(ctx, call, sibling) && isContextOperation(ctx, call) {
			foundIO = true
		}
		return !foundIO
	})
	return foundIO
}

func isCallToSibling(ctx astContext, call *ast.CallExpr, sibling types.Object) bool {
	return sibling != nil && calledObject(ctx, call) == sibling
}

func isContextOperation(ctx astContext, call *ast.CallExpr) bool {
	obj := calledObject(ctx, call)
	if obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == contextPackagePath {
		return false
	}
	if isNonOperationConstructor(obj) {
		return false
	}
	signature := callSignature(ctx, call)
	if signature != nil && signatureAcceptsContext(signature) {
		return true
	}
	if obj == nil {
		return false
	}
	return knownCancellableCall(obj)
}

func isNonOperationConstructor(obj types.Object) bool {
	fn, ok := obj.(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "os/exec" &&
		functionReceiverName(fn) == "" && (fn.Name() == "Command" || fn.Name() == "CommandContext")
}

func callSignature(ctx astContext, call *ast.CallExpr) *types.Signature {
	if ctx.types == nil {
		return nil
	}
	typeAndValue, ok := ctx.types.Types[call.Fun]
	if !ok || typeAndValue.Type == nil {
		return nil
	}
	signature, _ := typeAndValue.Type.Underlying().(*types.Signature)
	return signature
}

func calledObject(ctx astContext, call *ast.CallExpr) types.Object {
	if ctx.types == nil {
		return nil
	}
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return ctx.types.Uses[fun]
	case *ast.SelectorExpr:
		if selection := ctx.types.Selections[fun]; selection != nil {
			return selection.Obj()
		}
		return ctx.types.Uses[fun.Sel]
	default:
		return nil
	}
}

func signatureAcceptsContext(signature *types.Signature) bool {
	params := signature.Params()
	for i := 0; i < params.Len(); i++ {
		if isContextType(params.At(i).Type()) {
			return true
		}
	}
	return false
}

func isContextType(t types.Type) bool {
	t = types.Unalias(t)
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == contextPackagePath && named.Obj().Name() == "Context"
}

func contextSiblingForDeclaredFunction(ctx astContext, fn *ast.FuncDecl) types.Object {
	if ctx.types == nil {
		return nil
	}
	declared, _ := ctx.types.Defs[fn.Name].(*types.Func)
	return contextSiblingObject(declared)
}

func contextSiblingObject(base *types.Func) types.Object {
	if base == nil || base.Pkg() == nil {
		return nil
	}
	signature, _ := base.Type().(*types.Signature)
	if signature == nil {
		return nil
	}
	if signature.Recv() != nil {
		return lookupContextMethod(signature.Recv().Type(), base)
	}
	candidate, _ := base.Pkg().Scope().Lookup(base.Name() + "Context").(*types.Func)
	if compatibleContextAlternative(signature, candidate) {
		return candidate
	}
	return nil
}

func lookupContextMethod(receiver types.Type, base *types.Func) types.Object {
	if receiver == nil || base == nil || base.Pkg() == nil {
		return nil
	}
	candidates := []types.Type{receiver}
	if _, pointer := receiver.(*types.Pointer); !pointer {
		candidates = append(candidates, types.NewPointer(receiver))
	}
	baseSignature, _ := base.Type().(*types.Signature)
	for _, candidateReceiver := range candidates {
		selection := types.NewMethodSet(candidateReceiver).Lookup(base.Pkg(), base.Name()+"Context")
		if selection == nil {
			continue
		}
		candidate, _ := selection.Obj().(*types.Func)
		if compatibleContextAlternative(baseSignature, candidate) {
			return candidate
		}
	}
	return nil
}

func compatibleContextAlternative(base *types.Signature, candidate *types.Func) bool {
	if base == nil {
		return false
	}
	alternative := functionSignature(candidate)
	if !contextAlternativeShapeMatches(base, alternative) {
		return false
	}
	return contextAlternativeParamsMatch(base, alternative)
}

func functionSignature(fn *types.Func) *types.Signature {
	if fn == nil {
		return nil
	}
	signature, _ := fn.Type().(*types.Signature)
	return signature
}

func contextAlternativeShapeMatches(base, alternative *types.Signature) bool {
	if alternative == nil {
		return false
	}
	if alternative.Params().Len() != base.Params().Len()+1 {
		return false
	}
	if !isContextType(alternative.Params().At(0).Type()) {
		return false
	}
	if alternative.Variadic() != base.Variadic() {
		return false
	}
	return types.Identical(alternative.Results(), base.Results())
}

func contextAlternativeParamsMatch(base, alternative *types.Signature) bool {
	for i := 0; i < base.Params().Len(); i++ {
		if !types.Identical(base.Params().At(i).Type(), alternative.Params().At(i+1).Type()) {
			return false
		}
	}
	return true
}

func knownCancellableCall(obj types.Object) bool {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return false
	}
	receiver := functionReceiverName(fn)
	switch fn.Pkg().Path() {
	case "database/sql":
		return databaseCancellableCalls[receiver][fn.Name()]
	case "net/http":
		return httpCancellableCalls[receiver][fn.Name()]
	case "net":
		return networkCancellableCalls[receiver][fn.Name()]
	case "os/exec":
		return execCancellableCalls[receiver][fn.Name()]
	default:
		return false
	}
}

func functionReceiverName(fn *types.Func) string {
	signature, _ := fn.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return ""
	}
	receiver := types.Unalias(signature.Recv().Type())
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = types.Unalias(pointer.Elem())
	}
	named, _ := receiver.(*types.Named)
	if named == nil {
		return ""
	}
	return named.Obj().Name()
}

var databaseCancellableCalls = map[string]map[string]bool{
	"DB":   nameSet("Begin", "Exec", "Ping", "Prepare", "Query", "QueryRow"),
	"Tx":   nameSet("Exec", "Prepare", "Query", "QueryRow"),
	"Stmt": nameSet("Exec", "Query", "QueryRow"),
}

var httpCancellableCalls = map[string]map[string]bool{
	"":          nameSet("Get", "Head", "ListenAndServe", "ListenAndServeTLS", "Post", "PostForm", "Serve", "ServeTLS"),
	"Client":    nameSet("Do", "Get", "Head", "Post", "PostForm"),
	"Server":    nameSet("ListenAndServe", "ListenAndServeTLS", "Serve", "ServeTLS"),
	"Transport": nameSet("RoundTrip"),
}

var networkCancellableCalls = map[string]map[string]bool{
	"":       nameSet("Dial", "Listen", "ListenPacket"),
	"Dialer": nameSet("Dial"),
}

var execCancellableCalls = map[string]map[string]bool{
	"Cmd": nameSet("CombinedOutput", "Output", "Run", "Start"),
}

func nameSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

func findBackgroundContextSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		root, ok := n.(*ast.CallExpr)
		if !ok || !isContextRoot(ctx, root) || backgroundContextAllowed(ctx, fn, root) ||
			hasRuleIgnoreDirective(ctx, root, "background-context") {
			return true
		}
		*findings = append(*findings, astFinding(
			"background-context", "medium", nodeLocation(ctx, root), name,
			"context.Background/TODO used inside function",
		))
		return true
	})
}

func isContextRoot(ctx astContext, call *ast.CallExpr) bool {
	obj := calledObject(ctx, call)
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == contextPackagePath &&
		(obj.Name() == "Background" || obj.Name() == "TODO")
}

func backgroundContextAllowed(ctx astContext, fn *ast.FuncDecl, root *ast.CallExpr) bool {
	if isDirectMainRoot(ctx, fn, root) {
		return true
	}
	if isContextWrapperRoot(ctx, fn, root) {
		return true
	}
	return rootHasAllowedConstructor(ctx, fn, root)
}

func isDirectMainRoot(ctx astContext, fn *ast.FuncDecl, root *ast.CallExpr) bool {
	return ctx.pkg.Name == "main" && fn.Recv == nil && fn.Name.Name == "main" && lexicalBody(fn.Body, root) == fn.Body
}

func isContextWrapperRoot(ctx astContext, fn *ast.FuncDecl, root *ast.CallExpr) bool {
	if lexicalBody(fn.Body, root) != fn.Body {
		return false
	}
	sibling := contextSiblingForDeclaredFunction(ctx, fn)
	return sibling != nil && rootPassedDirectlyTo(ctx, fn.Body, root, sibling)
}

func rootHasAllowedConstructor(ctx astContext, fn *ast.FuncDecl, root *ast.CallExpr) bool {
	allowed := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		constructor, ok := contextConstructorContainingRoot(n, root)
		if !ok {
			return true
		}
		allowed = contextConstructorAllowsRoot(ctx, fn, constructor, root)
		return !allowed
	})
	return allowed
}

func contextConstructorContainingRoot(n ast.Node, root *ast.CallExpr) (*ast.CallExpr, bool) {
	constructor, ok := n.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	if len(constructor.Args) == 0 {
		return nil, false
	}
	if !nodeContainsWithoutNestedFunc(constructor.Args[0], root) {
		return nil, false
	}
	return constructor, true
}

func contextConstructorAllowsRoot(
	ctx astContext,
	fn *ast.FuncDecl,
	constructor *ast.CallExpr,
	root *ast.CallExpr,
) bool {
	switch contextFunctionName(ctx, constructor) {
	case "WithTimeout", "WithDeadline":
		return sameExpression(constructor.Args[0], root)
	case "WithCancel":
		return cancelOwnershipResolved(ctx, fn, constructor)
	default:
		return false
	}
}

func contextFunctionName(ctx astContext, call *ast.CallExpr) string {
	obj := calledObject(ctx, call)
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	if obj.Pkg().Path() != contextPackagePath {
		return ""
	}
	return obj.Name()
}

func rootPassedDirectlyTo(ctx astContext, body *ast.BlockStmt, root *ast.CallExpr, target types.Object) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || calledObject(ctx, call) != target {
			return true
		}
		for _, arg := range call.Args {
			if sameExpression(arg, root) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func sameExpression(expr ast.Expr, target ast.Expr) bool {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr == target
		}
		expr = paren.X
	}
}

func cancelOwnershipResolved(ctx astContext, fn *ast.FuncDecl, constructor *ast.CallExpr) bool {
	scope := lexicalScopeFor(fn, constructor)
	cancel := assignedCallResult(scope.body, constructor, 1)
	if cancel == "" || cancel == "_" {
		return false
	}
	resources := map[string]astLocation{cancel: nodeLocation(ctx, constructor)}
	resolved := map[string]bool{}
	owners := map[string]map[string]bool{}
	externalOwners := parameterOwnersForFields(scope.recv, scope.params)
	returned := map[string]bool{}
	for _, statement := range scope.body.List {
		switch node := statement.(type) {
		case *ast.AssignStmt:
			collectOwnershipAssignments(ctx, node, owners)
		case *ast.ExprStmt:
			resolveCancelCall(ctx, node.X, cancel, resolved)
		case *ast.DeferStmt:
			resolveCancelCall(ctx, node.Call, cancel, resolved)
		case *ast.ReturnStmt:
			markReturnedResources(node, returned)
			markReturnedCancelCallback(ctx, node, cancel, resolved)
		}
	}
	classifyReturnedResources(resources, resolved, externalOwners, returned)
	resolveTransferredResources(resolved, owners, externalOwners)
	return resolved[cancel]
}

type lexicalScope struct {
	body   *ast.BlockStmt
	recv   *ast.FieldList
	params *ast.FieldList
}

func lexicalScopeFor(fn *ast.FuncDecl, target ast.Node) lexicalScope {
	scope := lexicalScope{body: fn.Body, recv: fn.Recv, params: fn.Type.Params}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		literal, ok := n.(*ast.FuncLit)
		if !ok || !nodeContains(literal.Body, target) {
			return true
		}
		scope = lexicalScope{body: literal.Body, params: literal.Type.Params}
		return true
	})
	return scope
}

func lexicalBody(body *ast.BlockStmt, target ast.Node) *ast.BlockStmt {
	result := body
	ast.Inspect(body, func(n ast.Node) bool {
		literal, ok := n.(*ast.FuncLit)
		if ok && nodeContains(literal.Body, target) {
			result = literal.Body
		}
		return true
	})
	return result
}

func nodeContains(root, target ast.Node) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		if n == target {
			found = true
		}
		return !found
	})
	return found
}

func resolveCancelCall(ctx astContext, expr ast.Expr, cancel string, resolved map[string]bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return
	}
	if isIdentName(call.Fun, cancel) {
		resolved[cancel] = true
	}
	markCancelHelperCall(ctx, call, cancel, resolved)
	literal, ok := call.Fun.(*ast.FuncLit)
	if !ok {
		return
	}
	for _, statement := range literal.Body.List {
		switch nested := statement.(type) {
		case *ast.ExprStmt:
			resolveCancelCall(ctx, nested.X, cancel, resolved)
		case *ast.DeferStmt:
			resolveCancelCall(ctx, nested.Call, cancel, resolved)
		}
	}
}

func markReturnedCancelCallback(ctx astContext, ret *ast.ReturnStmt, cancel string, resolved map[string]bool) {
	for _, result := range ret.Results {
		literal, ok := result.(*ast.FuncLit)
		if !ok {
			continue
		}
		for _, statement := range literal.Body.List {
			switch node := statement.(type) {
			case *ast.ExprStmt:
				resolveCancelCall(ctx, node.X, cancel, resolved)
			case *ast.DeferStmt:
				resolveCancelCall(ctx, node.Call, cancel, resolved)
			}
		}
	}
}

func assignedCallResult(body *ast.BlockStmt, target *ast.CallExpr, resultIndex int) string {
	name := ""
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			rhsIndex, index := assignmentResultIndexes(assign, i)
			if rhsIndex >= len(assign.Rhs) || assign.Rhs[rhsIndex] != target || index != resultIndex {
				continue
			}
			if ident, ok := lhs.(*ast.Ident); ok {
				name = ident.Name
			}
			return false
		}
		return true
	})
	return name
}

func collectCancelParams(
	importPath string,
	files map[string]*ast.File,
	names []string,
	info *types.Info,
) map[string]map[int]bool {
	cancelers := map[string]map[int]bool{}
	functions := collectFunctionsWithParams(importPath, files, names, info)
	seedCancelParams(cancelers, functions, info)
	propagateCancelParams(cancelers, functions, info)
	return cancelers
}

type functionParams struct {
	key    string
	fn     *ast.FuncDecl
	params map[types.Object]int
}

func collectFunctionsWithParams(
	importPath string,
	files map[string]*ast.File,
	names []string,
	info *types.Info,
) []functionParams {
	var functions []functionParams
	for _, name := range names {
		file := files[name]
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := declaredFunctionKey(importPath, fn)
			params := parameterObjectIndexes(fn.Type.Params, info)
			functions = append(functions, functionParams{key: key, fn: fn, params: params})
		}
	}
	return functions
}

func seedCancelParams(cancelers map[string]map[int]bool, functions []functionParams, info *types.Info) {
	for _, function := range functions {
		for _, statement := range function.fn.Body.List {
			for _, call := range directStatementCalls(statement) {
				for index := range directlyCalledParameters(call, function.params, info) {
					markCancelParam(cancelers, function.key, index)
				}
			}
			for index := range transferredParameterIndexes(function.fn, statement, function.params, info) {
				markCancelParam(cancelers, function.key, index)
			}
		}
	}
}

func propagateCancelParams(cancelers map[string]map[int]bool, functions []functionParams, info *types.Info) {
	for changed := true; changed; {
		changed = false
		for _, function := range functions {
			if propagateFunctionCancelParams(cancelers, function, info) {
				changed = true
			}
		}
	}
}

func propagateFunctionCancelParams(
	cancelers map[string]map[int]bool,
	function functionParams,
	info *types.Info,
) bool {
	changed := false
	for _, statement := range function.fn.Body.List {
		for _, call := range directStatementCalls(statement) {
			if propagateCallCancelParams(cancelers, function, call, info) {
				changed = true
			}
		}
	}
	return changed
}

func propagateCallCancelParams(
	cancelers map[string]map[int]bool,
	function functionParams,
	call *ast.CallExpr,
	info *types.Info,
) bool {
	changed := false
	callee := calledFunctionKey(calledObject(astContext{types: info}, call))
	for argument := range cancelers[callee] {
		if argument >= len(call.Args) {
			continue
		}
		for index := range parameterIndexesInNode(call.Args[argument], function.params, info) {
			if markCancelParam(cancelers, function.key, index) {
				changed = true
			}
		}
	}
	return changed
}

func markCancelParam(cancelers map[string]map[int]bool, key string, index int) bool {
	if cancelers[key] == nil {
		cancelers[key] = map[int]bool{}
	}
	if cancelers[key][index] {
		return false
	}
	cancelers[key][index] = true
	return true
}

func directStatementCalls(statement ast.Stmt) []*ast.CallExpr {
	switch node := statement.(type) {
	case *ast.ExprStmt:
		call, _ := node.X.(*ast.CallExpr)
		if call != nil {
			return []*ast.CallExpr{call}
		}
	case *ast.DeferStmt:
		return []*ast.CallExpr{node.Call}
	case *ast.AssignStmt:
		calls := make([]*ast.CallExpr, 0, len(node.Rhs))
		for _, rhs := range node.Rhs {
			if call, ok := rhs.(*ast.CallExpr); ok {
				calls = append(calls, call)
			}
		}
		return calls
	case *ast.ReturnStmt:
		calls := make([]*ast.CallExpr, 0, len(node.Results))
		for _, result := range node.Results {
			if call, ok := result.(*ast.CallExpr); ok {
				calls = append(calls, call)
			}
		}
		return calls
	}
	return nil
}

func transferredParameterIndexes(
	fn *ast.FuncDecl,
	statement ast.Stmt,
	params map[types.Object]int,
	info *types.Info,
) map[int]bool {
	transferred := map[int]bool{}
	assign, ok := statement.(*ast.AssignStmt)
	if !ok {
		return transferred
	}
	externalOwners := parameterOwners(fn)
	for i, lhs := range assign.Lhs {
		selector, ok := lhs.(*ast.SelectorExpr)
		if !ok || !externalOwners[rootIdent(selector.X)] {
			continue
		}
		rhsIndex, _ := assignmentResultIndexes(assign, i)
		if rhsIndex < len(assign.Rhs) {
			mergeIndexes(transferred, parameterIndexesInNode(assign.Rhs[rhsIndex], params, info))
		}
	}
	return transferred
}

func parameterIndexesInNode(node ast.Node, params map[types.Object]int, info *types.Info) map[int]bool {
	indexes := map[int]bool{}
	ast.Inspect(node, func(child ast.Node) bool {
		ident, ok := child.(*ast.Ident)
		if ok {
			if index, parameter := params[info.Uses[ident]]; parameter {
				indexes[index] = true
			}
		}
		return true
	})
	return indexes
}

func mergeIndexes(target, source map[int]bool) {
	for index := range source {
		target[index] = true
	}
}

func directlyCalledParameters(
	call *ast.CallExpr,
	params map[types.Object]int,
	info *types.Info,
) map[int]bool {
	called := map[int]bool{}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		if index, parameter := params[info.Uses[ident]]; parameter {
			called[index] = true
		}
		return called
	}
	literal, ok := call.Fun.(*ast.FuncLit)
	if !ok {
		return called
	}
	for _, statement := range literal.Body.List {
		var nested *ast.CallExpr
		switch node := statement.(type) {
		case *ast.ExprStmt:
			nested, _ = node.X.(*ast.CallExpr)
		case *ast.DeferStmt:
			nested = node.Call
		}
		if nested == nil {
			continue
		}
		for index := range directlyCalledParameters(nested, params, info) {
			called[index] = true
		}
	}
	return called
}

func parameterObjectIndexes(fields *ast.FieldList, info *types.Info) map[types.Object]int {
	indexes := map[types.Object]int{}
	if fields == nil || info == nil {
		return indexes
	}
	index := 0
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, name := range field.Names {
			if object := info.Defs[name]; object != nil {
				indexes[object] = index
			}
			index++
		}
	}
	return indexes
}

func parameterOwnersForFields(recv, params *ast.FieldList) map[string]bool {
	owners := map[string]bool{}
	for _, fields := range []*ast.FieldList{recv, params} {
		if fields == nil {
			continue
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				owners[name.Name] = true
			}
		}
	}
	return owners
}

func declaredFunctionKey(importPath string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return importPath + "." + fn.Name.Name
	}
	receiver := strings.TrimPrefix(receiverName(fn.Recv.List[0].Type), "*")
	return importPath + "." + receiver + "." + fn.Name.Name
}

func markCancelHelperCall(ctx astContext, call *ast.CallExpr, cancel string, resolved map[string]bool) {
	key := calledFunctionKey(calledObject(ctx, call))
	for index := range ctx.cancelers[key] {
		if index < len(call.Args) && cancelOwnedByArgument(ctx, call.Args[index], cancel) {
			resolved[cancel] = true
		}
	}
}

func cancelOwnedByArgument(ctx astContext, argument ast.Expr, cancel string) bool {
	if rootIdent(argument) == cancel {
		return true
	}
	literal, ok := argument.(*ast.FuncLit)
	if !ok {
		return false
	}
	resolved := map[string]bool{}
	for _, statement := range literal.Body.List {
		switch node := statement.(type) {
		case *ast.ExprStmt:
			resolveCancelCall(ctx, node.X, cancel, resolved)
		case *ast.DeferStmt:
			resolveCancelCall(ctx, node.Call, cancel, resolved)
		}
	}
	return resolved[cancel]
}

func calledFunctionKey(obj types.Object) string {
	fn, ok := obj.(*types.Func)
	if !ok || fn.Pkg() == nil {
		return ""
	}
	signature, _ := fn.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return fn.Pkg().Path() + "." + fn.Name()
	}
	receiver := signature.Recv().Type()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, ok := receiver.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path() + "." + named.Obj().Name() + "." + fn.Name()
}
