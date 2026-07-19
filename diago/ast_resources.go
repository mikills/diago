package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

func collectLifecycleOwnedFields(pkgs []goListPackage) map[string]map[string]bool {
	owned := map[string]map[string]bool{}
	for _, pkg := range pkgs {
		files := parseProductionFiles(pkg)
		mergeOwnedFields(owned, collectLifecycleOwnedFieldsFromFiles(pkg.ImportPath, files))
	}
	return owned
}

func parseProductionFiles(pkg goListPackage) map[string]*ast.File {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range pkg.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, 0)
		if err == nil {
			files[name] = file
		}
	}
	return files
}

func mergeOwnedFields(dst, src map[string]map[string]bool) {
	for key, fields := range src {
		if dst[key] == nil {
			dst[key] = map[string]bool{}
		}
		for field := range fields {
			dst[key][field] = true
		}
	}
}

func collectLifecycleOwnedFieldsFromFiles(importPath string, files map[string]*ast.File) map[string]map[string]bool {
	owned := map[string]map[string]bool{}
	for _, file := range files {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key, receiver, ok := lifecycleReceiver(importPath, fn)
			if !ok {
				continue
			}
			fields := lifecycleOwnedFieldNames(fn.Body, receiver)
			if len(fields) > 0 {
				owned[key] = fields
			}
		}
	}
	return owned
}

func lifecycleReceiver(importPath string, fn *ast.FuncDecl) (key, receiver string, ok bool) {
	if fn.Recv == nil || fn.Body == nil || !isLifecycleMethod(fn.Name.Name) {
		return "", "", false
	}
	field := fn.Recv.List[0]
	if len(field.Names) != 1 {
		return "", "", false
	}
	typeName := strings.TrimPrefix(receiverName(field.Type), "*")
	return importPath + "." + typeName, field.Names[0].Name, true
}

func lifecycleOwnedFieldNames(body *ast.BlockStmt, receiver string) map[string]bool {
	fields := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if field, ok := releasedReceiverField(call, receiver); ok {
			fields[field] = true
		}
		return true
	})
	return fields
}

func releasedReceiverField(call *ast.CallExpr, receiver string) (string, bool) {
	method, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !isLifecycleMethod(method.Sel.Name) {
		return "", false
	}
	field, ok := method.X.(*ast.SelectorExpr)
	if !ok || rootIdent(field.X) != receiver {
		return "", false
	}
	return field.Sel.Name, true
}

func collectReleaseParams(files map[string]*ast.File, names []string) map[string]map[int]bool {
	releases := map[string]map[int]bool{}
	for _, name := range names {
		file := files[name]
		if file == nil {
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			collectFunctionReleaseParams(releases, fn)
		}
	}
	return releases
}

func collectFunctionReleaseParams(releases map[string]map[int]bool, fn *ast.FuncDecl) {
	params := parameterIndexes(fn.Type.Params)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !isLifecycleMethod(selector.Sel.Name) {
			return true
		}
		index, ok := params[rootIdent(selector.X)]
		if !ok {
			return true
		}
		if releases[fn.Name.Name] == nil {
			releases[fn.Name.Name] = map[int]bool{}
		}
		releases[fn.Name.Name][index] = true
		return true
	})
}

func parameterIndexes(fields *ast.FieldList) map[string]int {
	indexes := map[string]int{}
	if fields == nil {
		return indexes
	}
	index := 0
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, name := range field.Names {
			indexes[name.Name] = index
			index++
		}
	}
	return indexes
}

func findResourceCloseSignals(findings *[]ASTFinding, ctx astContext, fn *ast.FuncDecl, name string) {
	resources, resolved := collectResourceCloseSignals(ctx, fn)
	for nameVar, loc := range resources {
		if !resolved[nameVar] {
			msg := fmt.Sprintf("%s is opened/created but Close is not called in the function", nameVar)
			*findings = append(*findings, astFinding("resource-not-closed", "high", loc, name, msg))
		}
	}
}

// collectResourceCloseSignals returns opened resources and the set whose
// ownership is resolved by lifecycle calls, returns, or owner transfers.
func collectResourceCloseSignals(ctx astContext, fn *ast.FuncDecl) (map[string]astLocation, map[string]bool) {
	resources := map[string]astLocation{}
	resolved := map[string]bool{}
	owners := map[string]map[string]bool{}
	externalOwners := parameterOwners(fn)
	returned := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			collectClosableAssignments(ctx, node, resources)
			collectOwnershipAssignments(ctx, node, owners)
		case *ast.CallExpr:
			markReleasedResource(ctx, node, resolved)
		case *ast.ReturnStmt:
			markReturnedResources(node, returned)
		}
		return true
	})
	classifyReturnedResources(resources, resolved, externalOwners, returned)
	resolveTransferredResources(resolved, owners, externalOwners)
	return resources, resolved
}

func classifyReturnedResources(
	resources map[string]astLocation,
	resolved map[string]bool,
	externalOwners map[string]bool,
	returned map[string]bool,
) {
	for name := range returned {
		if _, isResource := resources[name]; isResource {
			resolved[name] = true
		} else {
			externalOwners[name] = true
		}
	}
}

func parameterOwners(fn *ast.FuncDecl) map[string]bool {
	owners := map[string]bool{}
	for _, fields := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
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

func collectClosableAssignments(ctx astContext, stmt *ast.AssignStmt, resources map[string]astLocation) {
	for i, lhs := range stmt.Lhs {
		rhsIndex, resultIndex := assignmentResultIndexes(stmt, i)
		if rhsIndex >= len(stmt.Rhs) || !resultNeedsClose(ctx, stmt.Rhs[rhsIndex], resultIndex) {
			continue
		}
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			resources[id.Name] = nodeLocation(ctx, stmt)
		}
	}
}

func collectOwnershipAssignments(ctx astContext, stmt *ast.AssignStmt, owners map[string]map[string]bool) {
	for i, lhs := range stmt.Lhs {
		rhsIndex, resultIndex := assignmentResultIndexes(stmt, i)
		if rhsIndex >= len(stmt.Rhs) {
			continue
		}
		rhs := stmt.Rhs[rhsIndex]
		if selector, ok := lhs.(*ast.SelectorExpr); ok {
			addOwnershipEdges(owners, rootIdent(selector.X), rhs)
			continue
		}
		owner, ok := lhs.(*ast.Ident)
		if !ok || owner.Name == "_" {
			continue
		}
		switch rhs.(type) {
		case *ast.CompositeLit, *ast.UnaryExpr:
			addOwnershipEdges(owners, owner.Name, rhs)
		case *ast.CallExpr:
			addConstructorOwnershipEdges(ctx, owners, owner.Name, rhs, resultIndex)
		}
	}
}

func assignmentResultIndexes(stmt *ast.AssignStmt, lhsIndex int) (rhsIndex, resultIndex int) {
	if len(stmt.Rhs) == 1 {
		return 0, lhsIndex
	}
	return lhsIndex, 0
}

func addOwnershipEdges(owners map[string]map[string]bool, owner string, expr ast.Expr) {
	if owner == "" {
		return
	}
	for _, resource := range ownershipValueRoots(expr) {
		if resource != owner {
			addOwnershipEdge(owners, resource, owner)
		}
	}
}

func ownershipValueRoots(expr ast.Expr) []string {
	switch value := expr.(type) {
	case *ast.Ident:
		return []string{value.Name}
	case *ast.SelectorExpr, *ast.IndexExpr, *ast.StarExpr:
		if root := rootIdent(expr); root != "" {
			return []string{root}
		}
	case *ast.ParenExpr:
		return ownershipValueRoots(value.X)
	case *ast.UnaryExpr:
		return ownershipValueRoots(value.X)
	case *ast.CompositeLit:
		return compositeOwnershipRoots(value)
	}
	return nil
}

func compositeOwnershipRoots(literal *ast.CompositeLit) []string {
	var roots []string
	for _, element := range literal.Elts {
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			roots = append(roots, ownershipValueRoots(pair.Value)...)
			continue
		}
		roots = append(roots, ownershipValueRoots(element)...)
	}
	return roots
}

func addConstructorOwnershipEdges(ctx astContext, owners map[string]map[string]bool, owner string, expr ast.Expr, resultIndex int) {
	call := expr.(*ast.CallExpr)
	resultType := expressionResultType(ctx, expr, resultIndex)
	if resultType == nil {
		return
	}
	for _, arg := range call.Args {
		argType := expressionResultType(ctx, arg, 0)
		if argType == nil || !resultOwnsExactInterfaceField(ctx.pkg.ownedFields, resultType, argType) {
			continue
		}
		if resource := rootIdent(arg); resource != "" {
			addOwnershipEdge(owners, resource, owner)
		}
	}
}

func resultOwnsExactInterfaceField(owned map[string]map[string]bool, resultType, argType types.Type) bool {
	if _, ok := argType.Underlying().(*types.Interface); !ok {
		return false
	}
	if pointer, ok := resultType.(*types.Pointer); ok {
		resultType = pointer.Elem()
	}
	named, ok := resultType.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	ownedFields := owned[named.Obj().Pkg().Path()+"."+named.Obj().Name()]
	structure, ok := named.Underlying().(*types.Struct)
	if !ok {
		return false
	}
	for i := 0; i < structure.NumFields(); i++ {
		field := structure.Field(i)
		if ownedFields[field.Name()] && types.Identical(field.Type(), argType) {
			return true
		}
	}
	return false
}

func addOwnershipEdge(owners map[string]map[string]bool, resource, owner string) {
	if resource == "" || owner == "" || resource == owner {
		return
	}
	if owners[resource] == nil {
		owners[resource] = map[string]bool{}
	}
	owners[resource][owner] = true
}

func resolveTransferredResources(resolved map[string]bool, owners map[string]map[string]bool, externalOwners map[string]bool) {
	changed := true
	for changed {
		changed = resolveOneTransferPass(resolved, owners, externalOwners)
	}
}

func resolveOneTransferPass(resolved map[string]bool, owners map[string]map[string]bool, externalOwners map[string]bool) bool {
	changed := false
	for resource, candidates := range owners {
		if resolved[resource] {
			continue
		}
		for owner := range candidates {
			if resolved[owner] || externalOwners[owner] {
				resolved[resource] = true
				changed = true
				break
			}
		}
	}
	return changed
}

func resultNeedsClose(ctx astContext, expr ast.Expr, resultIndex int) bool {
	if _, ok := expr.(*ast.CallExpr); !ok {
		return false
	}
	t := expressionResultType(ctx, expr, resultIndex)
	return t != nil && typeNeedsClose(t)
}

func expressionResultType(ctx astContext, expr ast.Expr, resultIndex int) types.Type {
	if ctx.types == nil {
		return nil
	}
	typeAndValue, ok := ctx.types.Types[expr]
	if !ok || typeAndValue.Type == nil {
		return nil
	}
	t := typeAndValue.Type
	if tuple, ok := t.(*types.Tuple); ok {
		if resultIndex >= tuple.Len() {
			return nil
		}
		return tuple.At(resultIndex).Type()
	}
	if resultIndex > 0 {
		return nil
	}
	return t
}

func typeNeedsClose(t types.Type) bool {
	if hasCloseMethod(t) {
		return true
	}
	pointer, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := pointer.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Response"
}

func hasCloseMethod(t types.Type) bool {
	candidates := []types.Type{t}
	if _, ok := t.(*types.Pointer); !ok {
		candidates = append(candidates, types.NewPointer(t))
	}
	for _, candidate := range candidates {
		selection := types.NewMethodSet(candidate).Lookup(nil, "Close")
		if selection == nil {
			continue
		}
		signature, ok := selection.Obj().Type().(*types.Signature)
		if isCloseSignature(signature, ok) {
			return true
		}
	}
	return false
}

func isCloseSignature(signature *types.Signature, ok bool) bool {
	return ok && signature.Params().Len() == 0 && signature.Results().Len() == 1 &&
		types.Identical(signature.Results().At(0).Type(), types.Universe.Lookup("error").Type())
}

// markReleasedResource resolves the root variable a lifecycle call targets, so
// resp.Body.Close() resolves resp and server.Shutdown(ctx) resolves server.
func markReleasedResource(ctx astContext, call *ast.CallExpr, resolved map[string]bool) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isLifecycleMethod(sel.Sel.Name) {
		if root := rootIdent(sel.X); root != "" {
			resolved[root] = true
		}
	}
	markHelperReleasedResources(ctx, call, resolved)
}

func markHelperReleasedResources(ctx astContext, call *ast.CallExpr, resolved map[string]bool) {
	callee, ok := call.Fun.(*ast.Ident)
	if !ok {
		return
	}
	for index := range ctx.releases[callee.Name] {
		if index >= len(call.Args) {
			continue
		}
		if root := rootIdent(call.Args[index]); root != "" {
			resolved[root] = true
		}
	}
}

func isLifecycleMethod(name string) bool {
	switch name {
	case "Close", "GracefulStop", "Shutdown", "Stop":
		return true
	default:
		return false
	}
}

// markReturnedResources resolves resources returned to the caller, which then
// owns the cleanup.
func markReturnedResources(ret *ast.ReturnStmt, resolved map[string]bool) {
	for _, result := range ret.Results {
		for _, resource := range ownershipValueRoots(result) {
			resolved[resource] = true
		}
	}
}

// rootIdent returns the leftmost identifier name in a selector/index/deref
// chain, or "" if there is none.
func rootIdent(expr ast.Expr) string {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			return e.Name
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		case *ast.ParenExpr:
			expr = e.X
		default:
			return ""
		}
	}
}
