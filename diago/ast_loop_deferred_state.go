package diago

// Immutable radix tree for saved operands at CFG boundaries. Each registration
// changes O(log slots) nodes; unchanged prefixes, joins, replay snapshots, and
// ordered operand environments share nodes. This is separate from source cells:
// saved operands cannot be read/written through a Go variable or exported by a
// deferred callee. Keys are program-local dense slot IDs, not source identities.
type loopDeferredBindings struct {
	children [16]*loopDeferredBindings
	value    loopValue
}

func loopDeferredGet(n *loopDeferredBindings, key uint32) loopValue {
	for shift := 28; shift >= 0 && n != nil; shift -= 4 {
		n = n.children[key>>shift&15]
	}
	if n == nil {
		return nil
	}
	return n.value
}

func loopDeferredPut(n *loopDeferredBindings, key uint32, value loopValue, shift int) *loopDeferredBindings {
	if shift < 0 {
		if len(value) == 0 {
			return nil
		}
		return &loopDeferredBindings{value: value}
	}
	if n == nil && len(value) == 0 {
		return nil
	}
	out := loopDeferredBindings{}
	if n != nil {
		out = *n
	}
	i := key >> shift & 15
	before := out.children[i]
	out.children[i] = loopDeferredPut(before, key, value, shift-4)
	if out.children[i] == before {
		return n
	}
	if out.children[i] == nil {
		empty := true
		for _, child := range out.children {
			empty = empty && child == nil
		}
		if empty {
			return nil
		}
	}
	return &out
}

func loopDeferredJoinValue(a, b loopValue) (loopValue, bool) {
	if len(a) == 0 {
		return b, len(b) != 0
	}
	for v, mode := range b {
		if loopEpochMergeMode(a[v], mode) != a[v] {
			out := loopValue{}
			loopMerge(out, a)
			loopMerge(out, b)
			return out, true
		}
	}
	return a, false
}

// Event binding maps are immutable too. Copy only on a genuinely growing
// join, rather than copying the same caller cells at every LIFO predecessor.
func loopDeferredJoinBindings(a, b map[*loopVariable]loopValue) (map[*loopVariable]loopValue, bool) {
	for v, value := range b {
		for target, mode := range value {
			if loopEpochMergeMode(a[v][target], mode) != a[v][target] {
				out := loopCloneValues(a)
				loopJoinBindings(out, b)
				return out, true
			}
		}
	}
	return a, false
}

func loopDeferredJoin(a, b *loopDeferredBindings, shift int) *loopDeferredBindings {
	if a == b || b == nil {
		return a
	}
	if a == nil {
		return b
	}
	var out *loopDeferredBindings
	if shift < 0 {
		if value, changed := loopDeferredJoinValue(a.value, b.value); changed {
			return &loopDeferredBindings{value: value}
		}
		return a
	}
	for i := range a.children {
		child := loopDeferredJoin(a.children[i], b.children[i], shift-4)
		if child != a.children[i] {
			if out == nil {
				copy := *a
				out = &copy
			}
			out.children[i] = child
		}
	}
	if out != nil {
		return out
	}
	return a
}

func loopDeferredEach(n *loopDeferredBindings, key uint32, shift int, visit func(uint32, loopValue)) {
	if n == nil {
		return
	}
	if shift < 0 {
		visit(key, n.value)
		return
	}
	for i, child := range n.children {
		loopDeferredEach(child, key|uint32(i)<<shift, shift-4, visit)
	}
}

func loopDeferredAge(n *loopDeferredBindings, born map[*loopVariable]bool, shift int) *loopDeferredBindings {
	if n == nil {
		return nil
	}
	if shift < 0 {
		value := loopEpochAgeValue(n.value, born)
		for v, mode := range value {
			if n.value[v] != mode {
				return &loopDeferredBindings{value: value}
			}
		}
		return n
	}
	var out *loopDeferredBindings
	for i, child := range n.children {
		next := loopDeferredAge(child, born, shift-4)
		if next != child {
			if out == nil {
				copy := *n
				out = &copy
			}
			out.children[i] = next
		}
	}
	if out != nil {
		return out
	}
	return n
}

func (d *loopDeferredFlow) savedValue(w *loopWalk, slot *loopVariable) loopValue {
	if w.persistentDeferred {
		return loopDeferredGet(w.deferredBindings, d.savedIDs[slot])
	}
	return w.values[slot]
}

func (d *loopDeferredFlow) saveValue(w *loopWalk, slot *loopVariable, value loopValue) {
	if w.persistentDeferred {
		w.deferredBindings = loopDeferredPut(w.deferredBindings, d.savedIDs[slot], value, 28)
	} else if len(value) == 0 {
		delete(w.values, slot)
	} else {
		w.values[slot] = value
	}
}

func (d *loopDeferredFlow) union(w *loopWalk, slot *loopVariable, value loopValue) {
	if after, changed := loopDeferredJoinValue(d.savedValue(w, slot), value); changed {
		d.saveValue(w, slot, after)
	}
}
