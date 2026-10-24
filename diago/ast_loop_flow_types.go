package diago

import "go/ast"

// make's result type is explicit even without go/types. Follow declarations for
// types only, never for retained contents (which must use current flow bindings).
func (f *loopFlow) rangeSyntax(x ast.Expr, seen map[*loopVariable]bool) ast.Expr {
	if typ := f.s.syntaxType(x); typ != nil {
		return typ
	}
	switch x := loopUnparen(x).(type) {
	case *ast.CallExpr:
		if id, ok := loopUnparen(x.Fun).(*ast.Ident); ok && id.Name == "make" && len(x.Args) != 0 &&
			f.s.index.ids[id] == f.s.index.external[id.Name] {
			return f.s.underlyingSyntax(x.Args[0])
		}
	case *ast.Ident:
		if v := f.s.index.ids[x]; v != nil && !seen[v] {
			seen[v] = true
			return f.rangeSyntax(f.s.syntaxInitializer(v), seen)
		}
	}
	return nil
}

// Cache missing iteration-variable syntax types, just as syntaxInitializer
// caches declaration syntax. This does not change AST nodes, identities, or
// retained values. In particular, scalar field selection from a ranged struct
// must not inherit that struct's coarse pointer/function summary without types.
func (f *loopFlow) rangeTargetTypes(rng *ast.RangeStmt) {
	typ := f.rangeSyntax(rng.X, map[*loopVariable]bool{})
	if p, ok := typ.(*ast.StarExpr); ok {
		typ = f.s.underlyingSyntax(p.X)
	}
	var key, value ast.Expr
	switch typ := typ.(type) {
	case *ast.ArrayType:
		key, value = ast.NewIdent("int"), typ.Elt
	case *ast.MapType:
		key, value = typ.Key, typ.Value
	case *ast.ChanType:
		key = typ.Value
	}
	for i, expr := range []ast.Expr{rng.Key, rng.Value} {
		if id, ok := loopUnparen(expr).(*ast.Ident); ok {
			if v := f.s.index.ids[id]; v != nil && v.typeExpr == nil {
				v.typeExpr = []ast.Expr{key, value}[i]
			}
		}
	}
}
