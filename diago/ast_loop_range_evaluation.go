package diago

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
)

// rangeExpressionEvaluated implements the range/constant-len exception, not an
// array-copy optimization. In particular a trailing blank value is omitted, but
// a blank key followed by a real value still requests two iteration values.
// Unknown types/calls deliberately keep their evaluation.
func (x *loopIndex) rangeExpressionEvaluated(rng *ast.RangeStmt) bool {
	if rng.Value != nil {
		if id, ok := loopUnparen(rng.Value).(*ast.Ident); !ok || id.Name != "_" {
			return true
		}
	}
	// Resolve names without recording reads, captures, locations, or literal
	// bodies. The syntax fallback and later rangeTargetTypes need identities
	// even when the operand never becomes an executable CFG node.
	x.resolveRangeNames(rng.X)
	s := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
	return !s.rangeConstantArray(rng.X) || s.rangeHasCallOrReceive(rng.X)
}

func (x *loopIndex) resolveRangeNames(expr ast.Expr) {
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.SelectorExpr:
			x.resolveRangeNames(n.X)
			return false
		case *ast.Ident:
			x.resolve(n)
		}
		return true
	})
}

func (s *loopScanner) rangeConstantArray(expr ast.Expr) bool {
	if info := s.env.ctx.types; info != nil {
		if typ := info.TypeOf(expr); typ != nil {
			// A type parameter's core type may be an array; len is nevertheless
			// not constant. Underlying preserves its interface constraint here.
			typ = typ.Underlying()
			if p, ok := typ.(*types.Pointer); ok {
				typ = p.Elem().Underlying()
			}
			_, array := typ.(*types.Array)
			return array
		}
	}
	typ := s.rangeOperandType(expr, map[ast.Expr]bool{})
	if p, ok := typ.(*ast.StarExpr); ok {
		typ = s.rangeUnderlyingType(p.X, map[ast.Expr]bool{})
	}
	a, ok := typ.(*ast.ArrayType)
	return ok && a.Len != nil
}

// Mirror the constant-len call test: conversions are not function calls;
// constant built-ins are not disqualifying calls. Do still inspect their
// operands (e.g. a nonconstant call inside unsafe.Sizeof). A literal's body is
// a separate execution frame, not part of constructing the function value.
func (s *loopScanner) rangeHasCallOrReceive(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.UnaryExpr:
			found = n.Op == token.ARROW
		case *ast.CallExpr:
			found = !s.rangeConversion(n.Fun) && !s.rangeConstant(n, map[ast.Expr]bool{})
		}
		return !found
	})
	return found
}

// Positive-only syntax fallback for constant expressions. TypeAndValue is the
// authority when supplied, including negative evidence for generic built-ins.
func (s *loopScanner) rangeConstant(expr ast.Expr, seen map[ast.Expr]bool) bool {
	expr = loopUnparen(expr)
	if expr == nil || seen[expr] {
		return false
	}
	if info := s.env.ctx.types; info != nil {
		if tv, ok := info.Types[expr]; ok {
			return tv.Value != nil
		}
	}
	seen[expr] = true
	defer delete(seen, expr)
	switch e := expr.(type) {
	case *ast.BasicLit:
		return true
	case *ast.Ident:
		return s.rangeConstantIdent(e)
	case *ast.UnaryExpr:
		return e.Op != token.ARROW && e.Op != token.AND && s.rangeConstant(e.X, seen)
	case *ast.BinaryExpr:
		return s.rangeConstant(e.X, seen) && s.rangeConstant(e.Y, seen)
	case *ast.CallExpr:
		return s.rangeConstantCall(e, seen)
	}
	return false
}

func (s *loopScanner) rangeConstantIdent(id *ast.Ident) bool {
	if _, kind := s.rangeDeclaration(id); kind == token.CONST {
		return true
	}
	return s.rangePredeclared(id) && (id.Name == "true" || id.Name == "false" || id.Name == "iota")
}

func (s *loopScanner) rangeConstantCall(call *ast.CallExpr, seen map[ast.Expr]bool) bool {
	if s.rangeConversion(call.Fun) {
		// Constant conversions are only to basic types. Composite and
		// interface conversions can contain constants without being one.
		id, ok := s.rangeUnderlyingType(call.Fun, map[ast.Expr]bool{}).(*ast.Ident)
		if !ok || !loopRangeBasicType(id.Name) || len(call.Args) != 1 {
			return false
		}
		return s.rangeConstant(call.Args[0], seen)
	}
	if s.rangeUnsafeConstant(call) {
		return true
	}
	id, ok := loopUnparen(call.Fun).(*ast.Ident)
	if !ok || !s.rangePredeclared(id) {
		return false
	}
	return s.rangeConstantBuiltin(id.Name, call.Args, seen)
}

// rangeConstantBuiltin applies the constant rules of the predeclared function
// name to its arguments.
func (s *loopScanner) rangeConstantBuiltin(name string, args []ast.Expr, seen map[ast.Expr]bool) bool {
	switch name {
	case "len", "cap":
		if len(args) != 1 {
			return false
		}
		if s.rangeConstantArray(args[0]) {
			return !s.rangeHasCallOrReceive(args[0])
		}
		return name == "len" && s.rangeConstant(args[0], seen)
	case "complex", "real", "imag", "min", "max":
		if len(args) == 0 {
			return false
		}
		for _, arg := range args {
			if !s.rangeConstant(arg, seen) {
				return false
			}
		}
		return true
	}
	return false
}

// rangeDeclaration supplies the declaration-kind evidence missing from the
// lexical mode's identifiers. Only exact indexed local identities or package
// declarations are used; an arbitrary same-spelled declaration is not evidence.
func (s *loopScanner) rangeDeclaration(id *ast.Ident) (ast.Node, token.Token) {
	if decl, kind := loopRangeObjectDeclaration(id); kind != token.ILLEGAL {
		return decl, kind
	}
	v := s.index.ids[id]
	local := v != nil && v != s.index.external[id.Name]
	if spec := s.rangeEnclosingTypeSpec(id); spec != nil {
		return spec, token.TYPE
	}
	// Type parameters are not ordinary indexed variables. Never mistake one
	// for a package type with the same spelling.
	if !local {
		if field := s.rangeTypeParam(id); field != nil {
			return field, token.TYPE
		}
	}
	switch {
	case s.env.ctx.file == nil:
		return nil, token.ILLEGAL
	case local:
		return s.rangeLocalDeclaration(v.ident)
	}
	return s.rangePackageDeclaration(id)
}

// loopRangeObjectDeclaration reads constant and type declarations from the
// parser's object resolution, when it recorded one.
func loopRangeObjectDeclaration(id *ast.Ident) (ast.Node, token.Token) {
	if id.Obj == nil {
		return nil, token.ILLEGAL
	}
	switch id.Obj.Kind {
	case ast.Con:
		if decl, ok := id.Obj.Decl.(ast.Node); ok {
			return decl, token.CONST
		}
	case ast.Typ:
		if spec, ok := id.Obj.Decl.(*ast.TypeSpec); ok {
			return spec, token.TYPE
		}
	}
	return nil, token.ILLEGAL
}

// Declaration type expressions are intentionally not ordinary reads, so
// their identifiers may never have entered ids. Recover only declarations
// in lexical blocks that actually enclose this reference.
func (s *loopScanner) rangeEnclosingTypeSpec(id *ast.Ident) *ast.TypeSpec {
	if s.index.ids[id] != nil || id.Obj != nil || s.env.ctx.file == nil {
		return nil
	}
	var nearest *ast.TypeSpec
	ast.Inspect(s.env.ctx.file, func(n ast.Node) bool {
		if n == nil || n.Pos() > id.Pos() || n.End() < id.End() {
			return false
		}
		for _, stmt := range loopRangeBlockStatements(n) {
			for _, spec := range loopRangeLocalTypeSpecs(stmt) {
				t := spec.(*ast.TypeSpec)
				if t.Name.Name == id.Name && t.Name.Pos() <= id.Pos() &&
					(nearest == nil || t.Name.Pos() > nearest.Name.Pos()) {
					nearest = t
				}
			}
		}
		return true
	})
	return nearest
}

func loopRangeBlockStatements(n ast.Node) []ast.Stmt {
	switch n := n.(type) {
	case *ast.BlockStmt:
		return n.List
	case *ast.CaseClause:
		return n.Body
	case *ast.CommClause:
		return n.Body
	}
	return nil
}

func loopRangeLocalTypeSpecs(stmt ast.Stmt) []ast.Spec {
	d, ok := stmt.(*ast.DeclStmt)
	if !ok {
		return nil
	}
	decl, ok := d.Decl.(*ast.GenDecl)
	if !ok || decl.Tok != token.TYPE {
		return nil
	}
	return decl.Specs
}

func (s *loopScanner) rangeTypeParam(id *ast.Ident) *ast.Field {
	if s.env.ctx.decl == nil || s.env.ctx.decl.Type.TypeParams == nil {
		return nil
	}
	for _, field := range s.env.ctx.decl.Type.TypeParams.List {
		for _, name := range field.Names {
			if name.Name == id.Name {
				return field
			}
		}
	}
	return nil
}

// rangeLocalDeclaration finds the spec that declares exactly ident.
func (s *loopScanner) rangeLocalDeclaration(ident *ast.Ident) (ast.Node, token.Token) {
	matches := func(name *ast.Ident) bool { return name == ident }
	var found ast.Node
	var kind token.Token
	ast.Inspect(s.env.ctx.file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok {
			return found == nil
		}
		if spec := loopRangeMatchingSpec(decl, matches); spec != nil {
			found, kind = spec, decl.Tok
		}
		return false
	})
	return found, kind
}

// rangePackageDeclaration finds a package-level declaration spelled like id. A
// plain function ends the search. Otherwise the last matching spec wins.
func (s *loopScanner) rangePackageDeclaration(id *ast.Ident) (ast.Node, token.Token) {
	matches := func(name *ast.Ident) bool { return name.Name == id.Name }
	var found ast.Node
	var kind token.Token
	for _, decl := range s.env.ctx.file.Decls {
		switch decl := decl.(type) {
		case *ast.GenDecl:
			if spec := loopRangeMatchingSpec(decl, matches); spec != nil {
				found, kind = spec, decl.Tok
			}
		case *ast.FuncDecl:
			if decl.Recv == nil && matches(decl.Name) {
				return decl, token.FUNC
			}
		}
	}
	return found, kind
}

// loopRangeMatchingSpec returns the last spec of decl that declares a matching
// name, or nil.
func loopRangeMatchingSpec(decl *ast.GenDecl, matches func(*ast.Ident) bool) ast.Spec {
	var found ast.Spec
	for _, spec := range decl.Specs {
		switch spec := spec.(type) {
		case *ast.TypeSpec:
			if matches(spec.Name) {
				found = spec
			}
		case *ast.ValueSpec:
			for _, name := range spec.Names {
				if matches(name) {
					found = spec
				}
			}
		}
	}
	return found
}

func (s *loopScanner) rangePredeclared(id *ast.Ident) bool {
	if s.index.ids[id] != s.index.external[id.Name] {
		return false
	}
	decl, _ := s.rangeDeclaration(id)
	return decl == nil
}

func (s *loopScanner) rangeConversion(fun ast.Expr) bool {
	if info := s.env.ctx.types; info != nil {
		if tv, ok := info.Types[fun]; ok {
			return tv.IsType()
		}
	}
	if id, ok := loopUnparen(fun).(*ast.Ident); ok {
		if decl, kind := s.rangeDeclaration(id); decl != nil {
			return kind == token.TYPE
		}
	}
	return s.conversion(fun)
}

func (s *loopScanner) rangeUnderlyingType(typ ast.Expr, seen map[ast.Expr]bool) ast.Expr {
	typ = loopUnparen(typ)
	if typ == nil || seen[typ] {
		return nil
	}
	if id, ok := typ.(*ast.Ident); ok {
		if decl, _ := s.rangeDeclaration(id); decl != nil {
			if spec, ok := decl.(*ast.TypeSpec); ok {
				seen[typ] = true
				defer delete(seen, typ)
				return s.rangeUnderlyingType(spec.Type, seen)
			}
		}
	}
	return typ
}

func (s *loopScanner) rangeOperandType(expr ast.Expr, seen map[ast.Expr]bool) ast.Expr {
	expr = loopUnparen(expr)
	if expr == nil || seen[expr] {
		return nil
	}
	seen[expr] = true
	defer delete(seen, expr)
	if id, ok := expr.(*ast.Ident); ok {
		decl, _ := s.rangeDeclaration(id)
		if spec, ok := decl.(*ast.ValueSpec); ok {
			if spec.Type != nil {
				return s.rangeUnderlyingType(spec.Type, seen)
			}
			for i, name := range spec.Names {
				if name.Name == id.Name && i < len(spec.Values) {
					return s.rangeOperandType(spec.Values[i], seen)
				}
			}
		}
	}
	if call, ok := expr.(*ast.CallExpr); ok && s.rangeConversion(call.Fun) {
		return s.rangeUnderlyingType(call.Fun, seen)
	}
	return s.rangeUnderlyingType(s.syntaxType(expr), seen)
}

// unsafe's size/offset built-ins are constant only for fixed-size types.
// Recognize imports by path, not merely a selector spelled "unsafe".
func (s *loopScanner) rangeUnsafeConstant(call *ast.CallExpr) bool {
	if len(call.Args) != 1 || s.env.ctx.file == nil {
		return false
	}
	sel, ok := loopUnparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := loopUnparen(sel.X).(*ast.Ident)
	if !ok || s.index.ids[id] != s.index.external[id.Name] {
		return false
	}
	imported := false
	for _, spec := range s.env.ctx.file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := "unsafe"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imported = imported || path == "unsafe" && name == id.Name
	}
	if !imported {
		return false
	}
	arg := call.Args[0]
	switch sel.Sel.Name {
	case "Sizeof", "Alignof":
	case "Offsetof":
		field, ok := loopUnparen(arg).(*ast.SelectorExpr)
		if !ok {
			return false
		}
		arg = field.X
	default:
		return false
	}
	typ := s.rangeOperandType(arg, map[ast.Expr]bool{})
	if sel.Sel.Name == "Offsetof" {
		typ = s.rangeUnderlyingType(typ, map[ast.Expr]bool{})
		if p, ok := typ.(*ast.StarExpr); ok {
			typ = p.X // offset depends on the struct layout, not pointer size
		}
	}
	return s.rangeFixedSize(typ, map[ast.Expr]bool{})
}

func (s *loopScanner) rangeFixedSize(typ ast.Expr, seen map[ast.Expr]bool) bool {
	typ = s.rangeUnderlyingType(typ, map[ast.Expr]bool{})
	if typ == nil || seen[typ] {
		return false
	}
	seen[typ] = true
	defer delete(seen, typ)
	switch typ := typ.(type) {
	case *ast.Ident:
		decl, _ := s.rangeDeclaration(typ)
		return decl == nil && loopRangeBasicType(typ.Name)
	case *ast.StarExpr, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return true
	case *ast.ArrayType:
		return typ.Len == nil || s.rangeFixedSize(typ.Elt, seen)
	case *ast.StructType:
		for _, field := range typ.Fields.List {
			if !s.rangeFixedSize(field.Type, seen) {
				return false
			}
		}
		return true
	}
	return false
}

func loopRangeBasicType(name string) bool {
	switch name {
	case "bool", "string", "byte", "rune", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128":
		return true
	}
	return false
}
