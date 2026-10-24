package diago

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strconv"
)

// Truth is deliberately not a binding/value analysis. Only immutable boolean
// expressions prove an execution edge absent; mutable booleans remain unknown.
func (s *loopScanner) deferredTruth(expr ast.Expr) (value, known bool) {
	return s.deferredTruthSeen(expr, map[ast.Expr]bool{})
}

func (s *loopScanner) deferredTruthSeen(expr ast.Expr, seen map[ast.Expr]bool) (value, known bool) {
	expr = loopUnparen(expr)
	if expr == nil || seen[expr] {
		return false, false
	}
	seen[expr] = true
	defer delete(seen, expr)
	if value, known := s.deferredTypedTruth(expr); known {
		return value, true
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return s.deferredIdentTruth(e, seen)
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			v, k := s.deferredTruthSeen(e.X, seen)
			return !v, k
		}
	case *ast.BinaryExpr:
		if e.Op == token.LAND || e.Op == token.LOR {
			return s.deferredShortCircuitTruth(e, seen)
		}
	}
	return false, false
}

func (s *loopScanner) deferredTypedTruth(expr ast.Expr) (value, known bool) {
	info := s.env.ctx.types
	if info == nil {
		return false, false
	}
	tv, ok := info.Types[expr]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Bool {
		return false, false
	}
	return constant.BoolVal(tv.Value), true
}

// A named constant takes the truth of its declared value. The predeclared
// true and false are known only while unshadowed.
func (s *loopScanner) deferredIdentTruth(id *ast.Ident, seen map[ast.Expr]bool) (value, known bool) {
	if decl, kind := s.rangeDeclaration(id); kind == token.CONST {
		if spec, ok := decl.(*ast.ValueSpec); ok {
			for i, name := range spec.Names {
				if name.Name == id.Name && i < len(spec.Values) {
					return s.deferredTruthSeen(spec.Values[i], seen)
				}
			}
		}
	}
	if s.rangePredeclared(id) {
		switch id.Name {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// A known false operand decides &&, and a known true operand decides ||, even
// when the other operand is unknown. When both are known and neither decides,
// both hold the operator's identity value, which is then the result.
func (s *loopScanner) deferredShortCircuitTruth(e *ast.BinaryExpr, seen map[ast.Expr]bool) (value, known bool) {
	x, kx := s.deferredTruthSeen(e.X, seen)
	y, ky := s.deferredTruthSeen(e.Y, seen)
	decisive := e.Op == token.LOR
	if kx && x == decisive || ky && y == decisive {
		return decisive, true
	}
	if kx && ky {
		return !decisive, true
	}
	return false, false
}

// Constant len/cap do not evaluate their operands. unsafe's layout queries
// consume operand TYPES, including for variable-sized generic operands (the
// compiler lowers those to dictionary/layout queries, not operand execution).
// Recognize the actual builtin, never just a same-spelled function.
func (s *loopScanner) deferredUnevaluated(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	name, unsafe := s.deferredBuiltin(call.Fun)
	if name != "len" && name != "cap" && !(unsafe && (name == "Sizeof" || name == "Alignof" || name == "Offsetof")) {
		return false
	}
	if unsafe {
		return true
	}
	if info := s.env.ctx.types; info != nil {
		if tv, ok := info.Types[call]; ok {
			return tv.Value != nil
		}
	}
	arg := call.Args[0]
	if s.rangeConstantArray(arg) {
		return !s.rangeHasCallOrReceive(arg)
	}
	return name == "len" && s.rangeConstant(arg, map[ast.Expr]bool{})
}

// loopDeferredBuiltinLayout maps each builtin deferredBuiltin recognizes to
// whether it is one of unsafe's layout queries.
var loopDeferredBuiltinLayout = map[string]bool{
	"len": false, "cap": false,
	"Sizeof": true, "Alignof": true, "Offsetof": true,
}

func (s *loopScanner) deferredBuiltin(fun ast.Expr) (string, bool) {
	fun = loopUnparen(fun)
	var id *ast.Ident
	switch e := fun.(type) {
	case *ast.Ident:
		id = e
	case *ast.SelectorExpr:
		id = e.Sel
	default:
		return "", false
	}
	layout, ok := loopDeferredBuiltinLayout[id.Name]
	if !ok {
		return "", false
	}
	if info := s.env.ctx.types; info != nil {
		if obj := info.Uses[id]; obj != nil {
			if b, ok := obj.(*types.Builtin); ok {
				return b.Name(), b.Pkg() != nil && b.Pkg().Path() == "unsafe"
			}
			return "", false
		}
	}
	if plain, ok := fun.(*ast.Ident); ok {
		return s.deferredPlainBuiltin(plain, layout)
	}
	return s.deferredUnsafeSelector(fun.(*ast.SelectorExpr))
}

// Without type information an unshadowed spelling is the universe builtin,
// except that a dot import of unsafe supplies the layout queries.
func (s *loopScanner) deferredPlainBuiltin(id *ast.Ident, layout bool) (string, bool) {
	if !s.rangePredeclared(id) {
		return "", false
	}
	return id.Name, layout && s.env.ctx.file != nil && s.deferredImportsUnsafe(".")
}

func (s *loopScanner) deferredUnsafeSelector(sel *ast.SelectorExpr) (string, bool) {
	pkg, ok := loopUnparen(sel.X).(*ast.Ident)
	if !ok || s.index.ids[pkg] != s.index.external[pkg.Name] || s.env.ctx.file == nil {
		return "", false
	}
	// Package-level declarations and local shadowing also disqualify an import.
	if decl, _ := s.rangeDeclaration(pkg); decl != nil {
		return "", false
	}
	if s.deferredImportsUnsafe(pkg.Name) {
		return sel.Sel.Name, true
	}
	return "", false
}

// deferredImportsUnsafe reports whether the current file imports unsafe under
// name, where "." is a dot import.
func (s *loopScanner) deferredImportsUnsafe(name string) bool {
	for _, spec := range s.env.ctx.file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		alias := "unsafe"
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if path == "unsafe" && alias == name {
			return true
		}
	}
	return false
}
