package diago

import "go/ast"

// resetCaptureEffects separates lexical retention from execution. The lexical
// index deliberately propagates free-variable references through every enclosing
// literal: constructing an inner literal needs those references, but does not
// execute its body. Keep that key set (including write-only captures), with no
// executable modes, before the first CFG replay.
//
// Integration in scanLoops, AFTER discovery and BEFORE report:
//
//	s.resetCaptureEffects()
//	for {
//		s.refineLaunchBindings(n)
//		if !s.updateCaptureEffects() {
//			break
//		}
//	}
//
// Do not reset inside the loop. refineLaunchBindings must replace launch values
// and synthesized effects on each replay, and filter index.direct to reachable
// accesses before updateCaptureEffects. Each literal body is its own frame;
// literal construction is never an executable access in the enclosing frame.
// The final no-change replay supplies launches/effects using converged summaries.
//
// No new fields are needed. s.value(FuncLit) already copies captures, while
// loopRetain keeps reference-only entries nonzero through binding joins and
// liveness pruning. Expansion follows a function slot's contents only when
// invoked: copying or comparing a function value does not execute its body. Reporting, including
// direct/sibling hazard helpers, must ignore entries without any executable bit.
func (s *loopScanner) resetCaptureEffects() {
	for _, captures := range s.index.captures {
		for v := range captures {
			captures[v] = loopRetain
		}
	}
}

// updateCaptureEffects grows source-global fallback summaries from one complete
// CFG replay. Direct includes reachable identifiers and synthetic effects from
// active contexts only (finishFlowEffects excludes obsolete constructor seeds).
// These monotone unions are not execution summaries for known literal callees:
// refineContext collects those separately at their actual call/launch bindings,
// including copied parameters. Otherwise an effect derived through a captured
// function slot could be frozen into an unrelated lexical retention key and
// survive a safe overwrite of that function slot.
//
// A go's retained callee/argument values contribute to its owning frame
// as well: invoking that frame can launch those effects, but merely constructing
// a literal nested in it cannot. Never propagate an inner frame's accesses
// directly to its lexical ancestors; calls/launches establish that connection on
// later replays instead. The lexical capture key set stays fixed: effects on
// variables outside that set remain dependent on the captured carrier. Effects
// that coincide with a lexical retention key can remain May in these fallback
// unions; known calls/launches must use their context-local execution summaries.
//
// Only function-literal summaries are changed. Unknown go callees still retain
// their pointer arguments with the ordinary value/expand read semantics; this
// must not be implemented by clearing executable bits on arbitrary loopValues.
// This is not call/return inference, nor does it model captured binding writes.
//
// Each successful call adds an executable bit to a lexical (literal, variable)
// capture pair; no keys change and no bits are removed. Thus C capture pairs
// and B executable bits allow at most B*C successful rounds (normally dependency
// depth), plus the final no-change replay. Work here is linear in direct accesses and
// launch-value entries; CFG construction/replay remains the per-round cost.
func (s *loopScanner) updateCaptureEffects() bool {
	changed := false
	add := func(owner *ast.FuncLit, v *loopVariable, mode loopAccess) {
		mode &= loopCallableCaptureMask
		if owner == nil || v == nil || v.owner == owner || mode == 0 {
			return
		}
		captures := s.index.captures[owner]
		if _, retained := captures[v]; !retained {
			// Effects reached only through a captured function/pointer slot
			// must remain dependent on that slot's current binding. expand
			// resolves them at the call/launch, not at literal construction.
			return
		}
		if captures[v]&mode == mode {
			return
		}
		captures[v] |= loopRetain | mode
		changed = true
	}
	for _, access := range s.index.direct {
		add(access.owner, access.variable, access.mode)
	}
	for _, launch := range s.launches {
		for v, mode := range launch.values {
			add(launch.owner, v, mode)
		}
	}
	return changed
}
