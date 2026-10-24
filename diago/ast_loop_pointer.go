package diago

import (
	"go/ast"
	"go/types"
)

// Loading a retained value copies a proven local pointee's contents, not its
// address. Heap allocations without an indexed local slot keep the ordinary
// coarse value fallback; old aggregate contents are not read from a new epoch.
func (s *loopScanner) loadKnownPointee(expr ast.Expr, w *loopWalk) (loopValue, bool) {
	if w.aliases == nil {
		return nil, false
	}
	flow := loopFlow{s: s, aliases: w.aliases}
	targets := flow.addressAliases(expr, w)
	if len(targets) == 0 {
		return nil, false
	}
	value := loopValue{}
	for target, mode := range targets {
		if loopEpochCurrentMode(mode) != 0 {
			loopMerge(value, w.values[target])
		}
	}
	return value, true
}

// Loading a pointer stored in a proven local cell preserves that pointer's
// address provenance. Prior-only cells must not read a new allocation's value.
func loopLoadedAddressAliases(targets loopValue, w *loopWalk, aliases map[*loopVariable]*loopVariable) loopValue {
	out := loopValue{}
	for target, mode := range targets {
		if loopEpochCurrentMode(mode) == 0 || mode&loopAddressPartial != 0 {
			continue // a collapsed member is not the root's pointer cell
		}
		if slot := aliases[target]; slot != nil {
			loopMerge(out, w.values[slot])
		}
	}
	return out
}

func (s *loopScanner) pointerValue(expr ast.Expr) bool {
	if info := s.env.ctx.types; info != nil && info.TypeOf(expr) != nil {
		_, pointer := info.TypeOf(expr).Underlying().(*types.Pointer)
		return pointer
	}
	_, pointer := s.syntaxType(expr).(*ast.StarExpr)
	return pointer
}

func (s *loopScanner) referenceIndex(expr ast.Expr) bool {
	if info := s.env.ctx.types; info != nil && info.TypeOf(expr) != nil {
		switch info.TypeOf(expr).Underlying().(type) {
		case *types.Pointer, *types.Slice, *types.Map:
			return true
		}
		return false
	}
	switch typ := s.syntaxType(expr).(type) {
	case *ast.StarExpr, *ast.MapType:
		return true
	case *ast.ArrayType:
		return typ.Len == nil
	}
	return false
}

// Taking an address preserves the addressed storage even when evaluating that
// operand as a copied scalar would not retain any references.
func (s *loopScanner) addressValue(expr ast.Expr, w *loopWalk) loopValue {
	switch expr := loopUnparen(expr).(type) {
	case *ast.StarExpr:
		return s.value(expr.X, w)
	case *ast.SelectorExpr:
		if value, handled := s.promotedFieldAddressValue(expr, w); handled {
			return value
		}
		if s.pointerValue(expr.X) {
			return s.value(expr.X, w)
		}
		return s.addressValue(expr.X, w)
	case *ast.IndexExpr:
		return s.addressValue(expr.X, w)
	default:
		return s.value(expr, w)
	}
}
