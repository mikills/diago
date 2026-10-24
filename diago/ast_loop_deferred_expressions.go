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
	if info := s.env.ctx.types; info != nil {
		if tv, ok := info.Types[expr]; ok && tv.Value != nil && tv.Value.Kind() == constant.Bool {
			return constant.BoolVal(tv.Value), true
		}
	}
	switch e := expr.(type) {
	case *ast.Ident:
		if decl, kind := s.rangeDeclaration(e); kind == token.CONST {
			if spec, ok := decl.(*ast.ValueSpec); ok {
				for i, name := range spec.Names {
					if name.Name == e.Name && i < len(spec.Values) {
						return s.deferredTruthSeen(spec.Values[i], seen)
					}
				}
			}
		}
		if s.rangePredeclared(e) {
			switch e.Name {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		}
	case *ast.UnaryExpr:
		if e.Op == token.NOT {
			v, k := s.deferredTruthSeen(e.X, seen)
			return !v, k
		}
	case *ast.BinaryExpr:
		if e.Op == token.LAND || e.Op == token.LOR {
			x, kx := s.deferredTruthSeen(e.X, seen)
			y, ky := s.deferredTruthSeen(e.Y, seen)
			if e.Op == token.LAND {
				if kx && !x || ky && !y {
					return false, true
				}
				if kx && ky {
					return x && y, true
				}
			} else {
				if kx && x || ky && y {
					return true, true
				}
				if kx && ky {
					return x || y, true
				}
			}
		}
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
	switch id.Name {
	case "len", "cap", "Sizeof", "Alignof", "Offsetof":
	default:
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
		if s.rangePredeclared(plain) {
			if s.env.ctx.file != nil && (plain.Name == "Sizeof" || plain.Name == "Alignof" || plain.Name == "Offsetof") {
				for _, spec := range s.env.ctx.file.Imports {
					path, _ := strconv.Unquote(spec.Path.Value)
					if path == "unsafe" && spec.Name != nil && spec.Name.Name == "." {
						return plain.Name, true
					}
				}
			}
			return plain.Name, false
		}
		return "", false
	}
	sel := fun.(*ast.SelectorExpr)
	pkg, ok := loopUnparen(sel.X).(*ast.Ident)
	if !ok || s.index.ids[pkg] != s.index.external[pkg.Name] || s.env.ctx.file == nil {
		return "", false
	}
	// Package-level declarations and local shadowing also disqualify an import.
	if decl, _ := s.rangeDeclaration(pkg); decl != nil {
		return "", false
	}
	for _, spec := range s.env.ctx.file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		alias := "unsafe"
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		if path == "unsafe" && alias == pkg.Name {
			return sel.Sel.Name, true
		}
	}
	return "", false
}
