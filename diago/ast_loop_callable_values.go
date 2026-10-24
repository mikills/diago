package diago

import (
	"go/ast"
	"go/types"
)

// Callable contents share the value lattice with ordinary retained data, but
// their executable modes must not run merely because the value is copied or
// compared. Keep loopRetain unshifted so even reference-only captures remain
// nonzero through joins, symbolic copied-content liveness, and pruning.
const (
	loopCallableShift    = 8
	loopCallableExecMask = loopReadAccessMask | loopWrite | loopIndirectRead | loopIndirectWrite | loopInvoke
	// Preserve the field-routing hint along with executable capture modes.
	loopCallableCaptureMask = loopCallableExecMask | loopFieldInvoke | loopEmbeddedPointer
	loopCallableLatentMask  = loopCallableCaptureMask << loopCallableShift
)

func loopActivateCallableMode(mode loopAccess) loopAccess {
	return mode&^loopCallableLatentMask | (mode&loopCallableLatentMask)>>loopCallableShift
}

// knownInvocationAlias requires the private address-provenance slots, never a
// captured variable's ordinary value summary. This avoids executing callbacks
// merely because a called function compares them or a method binds a receiver.
func (s *loopScanner) knownInvocationAlias(from, target *loopVariable, w *loopWalk) bool {
	if slot := w.aliases[from]; slot != nil {
		return loopEpochCurrentMode(w.values[slot][target])&loopRead != 0
	}
	return false
}

// bindMethodReceiver retains receiver data without executing it when the method
// value is copied or compared. A method call is opaque: latent callbacks stored
// inside its receiver are not themselves invocation edges.
func loopBindMethodReceiver(value loopValue) loopValue {
	out := loopValue{}
	for v, mode := range value {
		executable := mode & loopCallableExecMask
		out[v] = mode&^(loopCallableLatentMask|loopCallableCaptureMask) | loopRetain | executable<<loopCallableShift
	}
	return out
}

// invokeValue is the execution boundary, not an expansion of current bindings.
// Use it instead of value for actual Go callees, retained Go callback arguments,
// and known synchronous callees; expand its result against the same walk state.
// All storage/copy/projection/comparison paths must continue to use value.
func (s *loopScanner) invokeValue(expr ast.Expr, w *loopWalk) loopValue {
	out := s.value(expr, w)
	for v, mode := range out {
		out[v] = loopActivateCallableMode(mode)
	}
	return out
}

// callablePointer recognizes only a statically known chain of pointers ending
// at a function signature. In particular a function that merely captures other
// functions is not such a chain. No heap or call-result alias inference is used.
func (s *loopScanner) callablePointer(expr ast.Expr) bool {
	if info := s.env.ctx.types; info != nil {
		if typ := info.TypeOf(expr); typ != nil {
			seen := map[types.Type]bool{}
			pointer := false
			for !seen[typ] {
				seen[typ] = true
				switch t := typ.Underlying().(type) {
				case *types.Pointer:
					pointer, typ = true, t.Elem()
				case *types.Signature:
					return pointer
				default:
					return false
				}
			}
			return false
		}
	}
	typ := s.syntaxType(expr)
	seen := map[ast.Expr]bool{}
	pointer := false
	for typ != nil && !seen[typ] {
		seen[typ] = true
		switch t := s.underlyingSyntax(typ).(type) {
		case *ast.StarExpr:
			pointer, typ = true, t.X
		case *ast.FuncType:
			return pointer
		default:
			return false
		}
	}
	return false
}

// Loading *p copies the current contents of known local pointees, not their
// slot addresses. Do exactly one load: **p's next StarExpr supplies the next
// load, and expand follows captured pointer chains at invocation time. Unknown
// symbolic references are preserved for the copied-content liveness pass.
func (s *loopScanner) loadCallablePointer(pointer ast.Expr, w *loopWalk) loopValue {
	out := loopValue{}
	for target, mode := range s.value(pointer, w) {
		if target.ident != nil && mode&loopRead != 0 && loopEpochCurrentMode(mode) != 0 {
			loopMerge(out, w.values[target])
		} else {
			out[target] = loopEpochMergeMode(out[target], mode)
		}
	}
	return out
}
