package diago

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/cfg"
)

// loopDeferredFlow owns hidden, immutable-value slots. Share one per loopFlow,
// including its recursively refined contexts. Production CFGs keep operands in
// a persistent tree, separate from source-cell maps; top tokens stay in the
// binding lattice. No hidden slot may occur in a launch's retained values.
//
// CFG integration invariants (wired in ast_loop_deferred_flow.go):
//   - Make a plan before liveness, after iterator expansion. Pass explicit exit
//     and entry markers for inlined functions (see iteratorExit/iteratorEntry).
//   - Add companions to both loopPruneBindings and liveness's dependency graph,
//     just like address-proof slots. Include them in captured context seeds.
//   - Call plan.live at each liveness node; merge its uses/defs/dependencies into
//     the existing equations BEFORE their transitive closure. Route symbolic
//     contents through the existing sources/copies graph, not mutable slots.
//   - Call flow.transfer BEFORE ordinary transfer, in convergence AND replay.
//     Call plan.transfer at the same two places. Registration has no effects.
//   - At a matching exit: evaluate return operands and assign named results;
//     executeExit; then read/copy results. Do not record the old combined
//     ReturnStmt read/write effect before executing defers.
//   - executeExit's runner must execute a KNOWN literal with inv.seed, returning
//     its captured/proven-caller-cell updates. In replay it must also project its physical
//     accesses and escaping Go events at THIS exit occurrence in the caller CFG.
//     Refine constructor contexts separately; they are not execution contexts.
//     For opaque callees, record inv.effects at that occurrence, not registration.
//     For known callees use their actual frame, not the union in inv.effects.
//     Apply inv.referenceMode to direct captured-slot accesses and inv.writeback
//     to exported binding/alias/origin updates (using each address proof's own
//     epoch for noncaptures). Keep exported caller bindings live at callee exits,
//     including explicit empty overwrites. Nested actual
//     calls use callees with their own saved callable value as reference context.
//   - Pending callback ReturnStmt nodes assign results but have successors:
//     install pendingExit in their FINAL continuation after iterator unwinding.
//     Never execute the caller's deferred calls at the initial callback return.
//
// The runner separates pure, memoized exit-binding simulation from actual
// context replay and guarded physical projection. Substituting constructor
// summaries loses parameter bindings, deferred binding updates, and Go statements
// escaping from a deferred literal.
//
// Stack shape is bounded: one top slot per owner and one predecessor/snapshot
// group per registration syntax. Repeated registrations weakly accumulate saved
// values; predecessor edges form a finite graph, possibly cyclic. No unbounded
// stack, source-order sorting, or mutation of shared loopValues is used.
type loopDeferredFlow struct {
	s             *loopScanner
	aliases       map[*loopVariable]*loopVariable
	opaque        *loopVariable
	blocked       *loopVariable // runner-only non-returning invocation marker
	companions    map[*loopVariable]*loopVariable
	literalTokens map[*ast.FuncLit]*loopVariable
	literals      map[*loopVariable]*ast.FuncLit
	owners        map[*ast.FuncLit]*loopDeferredOwner
	slots         map[*ast.DeferStmt]*loopDeferredSlot
	markers       map[ast.Node]loopDeferredMarker
	savedSlots    map[*loopVariable]bool // hidden stack/operand storage, never source cells
	savedIDs      map[*loopVariable]uint32
}

type loopDeferredOwner struct {
	top, bottom *loopVariable
	slots       []*loopDeferredSlot
}

type loopDeferredSlot struct {
	stmt                             *ast.DeferStmt
	owner                            *ast.FuncLit
	token, previous, callee, origins *loopVariable
	args, addresses, argOrigins      []*loopVariable
}

type loopDeferredMarker struct {
	owner          *ast.FuncLit
	entry          bool
	result         *ast.ReturnStmt
	completionOnly bool // return operands/assignments ran before iterator unwinding
}

type loopDeferredPlan struct {
	flow       *loopDeferredFlow
	owner      *ast.FuncLit
	exits      map[ast.Node]loopDeferredMarker
	returns    map[ast.Node]loopDeferredMarker
	static     map[*loopDeferredSlot]loopDeferredStatic
	persistent bool
}

// loopDeferredInvocation is one saved registration, not a re-evaluation of its
// Call AST. seed copies current captured VARIABLE bindings, then installs saved
// parameter VALUES/address proofs/origins. effects activates only the saved
// callee. Arguments are not arbitrarily invoked or assumed to alias call results.
type loopDeferredInvocation struct {
	stmt            *ast.DeferStmt
	owner           *ast.FuncLit
	literal         *ast.FuncLit // nil means an opaque callee/method
	seed            map[*loopVariable]loopValue
	callee          loopValue
	args, addresses []loopValue
	effects         loopValue
}

// referenceMode projects a directly captured slot access into the allocation
// epoch saved in the function value. Actual literal frames must apply this to
// direct captured-slot accesses; interpreting their AST as current storage would
// undo the provenance of a closure saved on an earlier loop iteration.
func (inv loopDeferredInvocation) referenceMode(v *loopVariable, mode loopAccess) loopAccess {
	reference := inv.callee[v]
	if reference&loopPriorRef == 0 {
		return mode
	}
	prior := mode&^(loopPriorRef|loopEpochMixedRef) | loopPriorRef
	if loopEpochCurrentMode(reference) == 0 {
		return prior
	}
	return loopEpochMergeMode(mode, prior)
}

// writeback projects a captured VARIABLE's value/alias/origin slots. Proven
// address exports use the same filter with the address proof's own epoch.
func (inv loopDeferredInvocation) writeback(v *loopVariable, before, after loopValue) loopValue {
	return loopDeferredWriteback(inv.callee[v], before, after)
}

// An old reference cannot overwrite a new allocation. Mixed old/current
// references permit only weak updates. Apply identically to all companion slots.
func loopDeferredWriteback(reference loopAccess, before, after loopValue) loopValue {
	if reference&loopPriorRef == 0 {
		return after
	}
	if loopEpochCurrentMode(reference) == 0 {
		return before
	}
	out := loopValue{}
	loopMerge(out, before)
	loopMerge(out, after)
	return out
}

// replay=false must be pure (no findings/effects/context publication); replay=true
// is called only at converged inputs. Return bindings after the invocation; a
// nil return means no binding changes. A state containing flow.blocked denotes
// an invocation that cannot return; it has effects but no predecessor-stack or
// result-copy continuation. The caller can use invocation.seed to
// refine the actual literal, but must export only captured bindings and proven
// caller-addressed cells (never callee locals/parameters) into the caller state.
type loopDeferredRunner func(loopDeferredInvocation, map[*loopVariable]loopValue, bool) map[*loopVariable]loopValue

func newLoopDeferredFlow(s *loopScanner, aliases ...map[*loopVariable]*loopVariable) *loopDeferredFlow {
	var addressSlots map[*loopVariable]*loopVariable
	if len(aliases) != 0 {
		addressSlots = aliases[0]
	}
	return &loopDeferredFlow{s: s, aliases: addressSlots, opaque: &loopVariable{}, blocked: &loopVariable{},
		companions: map[*loopVariable]*loopVariable{}, literalTokens: map[*ast.FuncLit]*loopVariable{},
		literals: map[*loopVariable]*ast.FuncLit{}, owners: map[*ast.FuncLit]*loopDeferredOwner{},
		slots: map[*ast.DeferStmt]*loopDeferredSlot{}, markers: map[ast.Node]loopDeferredMarker{}, savedSlots: map[*loopVariable]bool{}, savedIDs: map[*loopVariable]uint32{},
	}
}

func (d *loopDeferredFlow) owner(owner *ast.FuncLit) *loopDeferredOwner {
	if d.owners[owner] == nil {
		d.owners[owner] = &loopDeferredOwner{top: &loopVariable{}, bottom: &loopVariable{}}
		d.savedSlots[d.owners[owner].top] = true
	}
	return d.owners[owner]
}

func (d *loopDeferredFlow) companion(v *loopVariable) *loopVariable {
	if v == nil {
		return nil
	}
	if d.companions[v] == nil {
		d.companions[v] = &loopVariable{}
	}
	return d.companions[v]
}

func (d *loopDeferredFlow) literal(lit *ast.FuncLit) *loopVariable {
	if d.literalTokens[lit] == nil {
		v := &loopVariable{}
		d.literalTokens[lit], d.literals[v] = v, lit
	}
	return d.literalTokens[lit]
}

func (d *loopDeferredFlow) slot(stmt *ast.DeferStmt) *loopDeferredSlot {
	if d.slots[stmt] != nil {
		return d.slots[stmt]
	}
	owner := d.s.index.locations[stmt].owner
	s := &loopDeferredSlot{stmt: stmt, owner: owner, token: &loopVariable{}, previous: &loopVariable{}, callee: &loopVariable{}, origins: &loopVariable{}}
	for range stmt.Call.Args {
		s.args = append(s.args, &loopVariable{})
		s.addresses = append(s.addresses, &loopVariable{})
		s.argOrigins = append(s.argOrigins, &loopVariable{})
	}
	d.slots[stmt] = s
	for _, slot := range s.storage() {
		d.savedSlots[slot] = true
		d.savedIDs[slot] = uint32(len(d.savedIDs))
	}
	o := d.owner(owner)
	o.slots = append(o.slots, s)
	return s
}

// plan must see only primitive CFG nodes, not recursively index nested bodies.
// The explicit marker table is authoritative for iterator owners. Remaining
// ReturnStmt nodes belong to this CFG's function, including its implicit return.
// A ReturnStmt with successors is a pending callback return, not an exit; its
// final continuation needs pendingExit. Never derive an implicit return's owner
// from a lexical ancestor's type.
func (d *loopDeferredFlow) plan(graph *cfg.CFG, owner *ast.FuncLit) *loopDeferredPlan {
	p := &loopDeferredPlan{flow: d, owner: owner, exits: map[ast.Node]loopDeferredMarker{}, returns: map[ast.Node]loopDeferredMarker{}}
	d.owner(owner)
	for _, block := range graph.Blocks {
		for _, node := range block.Nodes {
			if stmt, ok := node.(*ast.DeferStmt); ok {
				d.slot(stmt)
			}
			if marker, ok := d.markers[node]; ok {
				if !marker.entry {
					p.returns[node] = marker
					// Tagged iterator return operands remain visible to later
					// nested expansion. Their continuation owns the final exit.
					if _, ret := node.(*ast.ReturnStmt); !ret || len(block.Succs) == 0 {
						p.exits[node] = marker
					}
				}
			} else if ret, ok := node.(*ast.ReturnStmt); ok {
				marker := loopDeferredMarker{owner: owner, result: ret}
				p.returns[node] = marker
				// A return in a range callback first unwinds its iterators.
				// Its successor is a continuation, not the function's exit.
				if len(block.Succs) == 0 {
					p.exits[node] = marker
				}
			}
			// Allocate stable provenance companions before liveness. They are
			// finite metadata, not values returned by scanner.value(FuncLit).
			ast.Inspect(node, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					d.literal(n)
					for v := range d.s.index.captures[n] {
						d.companion(v)
					}
					return false
				case *ast.Ident:
					d.companion(d.s.index.ids[n])
				}
				return true
			})
		}
	}
	return p
}

// iteratorExit is the single-node exit form for an already expanded CFG.
// Production iterator templates instead retain/tag the ReturnStmt and install a
// pendingExit continuation: later nested expansion must still see that return
// and put false-return cleanup BEFORE its completion. Both normal and stopped
// clones need completions. iteratorEntry belongs only on the fresh invocation
// edge, never yield/resume edges.
func (d *loopDeferredFlow) iteratorExit(ret *ast.ReturnStmt, owner *ast.FuncLit) ast.Node {
	// Keep the original return as an AST child: recordSyncNode and the
	// ordinary liveness/reachability walkers must still see its operands and
	// their synchronous calls. cfg.Block.Return must NOT mistake the wrapper
	// for another pending return in the caller's callback.
	marker := &ast.BlockStmt{Lbrace: ret.Pos(), List: []ast.Stmt{ret}, Rbrace: ret.End()}
	d.markers[marker] = loopDeferredMarker{owner: owner, result: ret}
	d.owner(owner)
	return marker
}

// pendingExit belongs in the final pending-return continuation AFTER all
// stopped iterators finish. Keep the original ReturnStmt at the callback site
// to evaluate/assign results there, but do not run root defers at that site.
func (d *loopDeferredFlow) pendingExit(ret *ast.ReturnStmt, owner *ast.FuncLit) ast.Node {
	marker := &ast.EmptyStmt{Semicolon: ret.End(), Implicit: true}
	d.markers[marker] = loopDeferredMarker{owner: owner, result: ret, completionOnly: true}
	d.owner(owner)
	return marker
}

func (d *loopDeferredFlow) iteratorEntry(owner *ast.FuncLit, pos token.Pos) ast.Node {
	marker := &ast.EmptyStmt{Semicolon: pos, Implicit: true}
	d.markers[marker] = loopDeferredMarker{owner: owner, entry: true}
	d.owner(owner)
	return marker
}

// enter initializes just this function's stack, never its lexical ancestors'.
// Use at root CFG entry too, before the first prune. Captured bindings survive.
func (d *loopDeferredFlow) enter(owner *ast.FuncLit, w *loopWalk) {
	o := d.owner(owner)
	w.values[o.top] = loopValue{o.bottom: loopRetain}
	for _, s := range o.slots {
		for _, slot := range s.storage() {
			d.saveValue(w, slot, nil)
		}
	}
}
func (s *loopDeferredSlot) storage() []*loopVariable {
	out := []*loopVariable{s.previous, s.callee, s.origins}
	out = append(out, s.args...)
	out = append(out, s.addresses...)
	out = append(out, s.argOrigins...)
	return out
}

// origins is a separate companion lattice: callable identity cannot be recovered
// from a capture map (two literals can have identical captures). Copying f saves
// its origins NOW, while a closure referring to f loads f's origins at exit.
func (d *loopDeferredFlow) origins(expr ast.Expr, w *loopWalk) loopValue {
	switch expr := loopUnparen(expr).(type) {
	case *ast.FuncLit:
		return loopValue{d.literal(expr): loopRetain}
	case *ast.Ident:
		if value := w.values[d.companion(d.s.index.ids[expr])]; len(value) != 0 {
			return value
		}
	case *ast.StarExpr:
		proofs := (&loopFlowEffects{s: d.s, aliases: d.aliases}).addressAliases(expr.X, w)
		out := loopValue{}
		for target, mode := range proofs {
			if loopEpochCurrentMode(mode) == 0 {
				continue
			}
			value := w.values[d.companion(target)]
			if len(value) == 0 {
				value = loopValue{d.opaque: loopRetain}
			}
			loopMerge(out, value)
		}
		if len(out) != 0 {
			return out
		}
	case *ast.SelectorExpr, *ast.IndexExpr:
		if projected, ok := d.s.projection(expr); ok {
			return d.origins(projected, w)
		}
	case *ast.CallExpr:
		if len(expr.Args) == 1 && d.s.conversion(expr.Fun) {
			return d.origins(expr.Args[0], w)
		}
	}
	return loopValue{d.opaque: loopRetain}
}

// callees resolves invocation-time local function values, including saved
// callback parameters. Use it for actual call contexts/projection inside the
// runner; immutable-initializer lookup alone cannot resolve reassigned f or a
// function-valued parameter. opaque preserves an unknown alternative at joins.
func (d *loopDeferredFlow) callees(expr ast.Expr, w *loopWalk) (targets []*ast.FuncLit, opaque bool) {
	for token := range d.origins(expr, w) {
		if lit := d.literals[token]; lit != nil {
			targets = append(targets, lit)
		} else {
			opaque = true
		}
	}
	if len(targets) == 0 {
		opaque = true
	}
	return
}

// assignOrigins mirrors whole-variable writes and proven whole-cell stores.
// Aggregate field/index stores are deliberately not callable identities.
func (d *loopDeferredFlow) assignOrigins(lhs ast.Expr, value, proofs loopValue, w *loopWalk) {
	if id, ok := loopUnparen(lhs).(*ast.Ident); ok {
		if v := d.s.index.ids[id]; v != nil {
			w.values[d.companion(v)] = value
		}
		return
	}
	for target, mode := range loopWholeCellStoreTargets(d.s, lhs, proofs) {
		loopStoreCallableBinding(d.companion(target), mode, len(proofs) == 1, value, w)
	}
}

// transfer tracks callable origins using the PRE-transfer state for simultaneous
// assignments, including destination addresses. Call before ordinary transfer.
// No function body runs here.
func (d *loopDeferredFlow) transfer(node ast.Node, w *loopWalk) {
	var lhs []ast.Expr
	var rhs []ast.Expr
	switch n := node.(type) {
	case *ast.AssignStmt:
		lhs, rhs = n.Lhs, n.Rhs
	case *ast.ValueSpec:
		for _, name := range n.Names {
			lhs = append(lhs, name)
		}
		rhs = n.Values
	default:
		return
	}
	values, proofs := make([]loopValue, len(lhs)), make([]loopValue, len(lhs))
	for i, expr := range lhs {
		if i < len(rhs) {
			values[i] = d.origins(rhs[i], w)
		}
		if star, ok := loopUnparen(expr).(*ast.StarExpr); ok {
			proofs[i] = (&loopFlowEffects{s: d.s, aliases: d.aliases}).addressAliases(star.X, w)
		}
	}
	for i, expr := range lhs {
		d.assignOrigins(expr, values[i], proofs[i], w)
	}
}

func (p *loopDeferredPlan) transfer(node ast.Node, w *loopWalk, aliases func(ast.Expr, *loopWalk) loopValue) {
	d := p.flow
	if marker, ok := d.markers[node]; ok && marker.entry {
		d.enter(marker.owner, w)
		return
	}
	stmt, ok := node.(*ast.DeferStmt)
	if !ok {
		return
	}
	s := d.slot(stmt)
	o := d.owner(s.owner)
	// Save operands once, without invoking either the callee or arguments.
	if p.static[s].literal != nil {
		// Literal identity and reference modes are invariant within this CFG.
		// No argument expressions exist on this factored path.
	} else if saved := w.operands[stmt.Call]; saved != nil {
		d.union(w, s.callee, saved.callee)
		d.union(w, s.origins, saved.origins)
		for i := range stmt.Call.Args {
			d.union(w, s.args[i], saved.args[i])
			d.union(w, s.argOrigins[i], saved.argOrigins[i])
			d.union(w, s.addresses[i], saved.addresses[i])
		}
	} else {
		d.union(w, s.callee, d.s.value(stmt.Call.Fun, w))
		d.union(w, s.origins, d.origins(stmt.Call.Fun, w))
		for i, arg := range stmt.Call.Args {
			d.union(w, s.args[i], d.s.value(arg, w))
			d.union(w, s.argOrigins[i], d.origins(arg, w))
			if aliases != nil {
				d.union(w, s.addresses[i], aliases(arg, w))
			}
		}
	}
	if p.static[s].previous == nil {
		previous := w.values[o.top]
		if len(previous) == 0 {
			previous = loopValue{o.bottom: loopRetain}
		}
		d.union(w, s.previous, previous)
	}
	w.values[o.top] = loopValue{s.token: loopRetain}
}

// live retains source-cell dependencies, never mutable operand storage. The
// persistent representation uses one dependency root per owner; the unfactored
// reference representation uses individual slots. Merge uses/defs into the
// node's equations; merge dependencies before loopFlowLivePropagate.
// Pass the existing symbolic walk so copied contents remain symbols, not a
// dependency on the source variable's mutable slot. A closure's captures remain
// real variable dependencies and therefore retain their EXIT-time bindings.
func (p *loopDeferredPlan) live(node ast.Node, symbolic *loopWalk, aliases func(ast.Expr, *loopWalk) loopValue) (uses, defs map[*loopVariable]bool, dependencies map[*loopVariable]loopValue) {
	d := p.flow
	uses, defs = map[*loopVariable]bool{}, map[*loopVariable]bool{}
	dependencies = map[*loopVariable]loopValue{}
	if marker, ok := d.markers[node]; ok && marker.entry {
		o := d.owner(marker.owner)
		defs[o.top] = true
		if !p.persistent {
			for _, s := range o.slots {
				for _, slot := range s.storage() {
					defs[slot] = true
				}
			}
		}
	}
	if ret, results, ok := p.returnInfo(node); ok {
		if _, exits := p.exits[node]; exits {
			for _, v := range results {
				if v != nil {
					uses[v] = true
				}
			}
		}
		if ret != nil && len(ret.Results) != 0 && !p.returns[node].completionOnly {
			for i, v := range results {
				if v == nil {
					continue
				}
				defs[v], defs[d.companion(v)] = true, true
				if slot := d.aliases[v]; slot != nil {
					defs[slot] = true
				}
				if i < len(ret.Results) {
					dependencies[v] = d.s.value(ret.Results[i], symbolic)
				}
			}
		}
	}
	if exit, ok := p.exits[node]; ok {
		o := d.owner(exit.owner)
		uses[o.top] = true
		if !p.persistent {
			for _, s := range o.slots {
				for _, slot := range p.storage(s) {
					uses[slot] = true
				}
				for v := range d.s.index.captures[p.static[s].literal] {
					uses[v] = true // free cells are loaded now, never hoisted values
				}
			}
		}
	}
	if stmt, ok := node.(*ast.DeferStmt); ok {
		s := d.slot(stmt)
		o := d.owner(s.owner)
		uses[o.top], defs[o.top] = true, true
		dependencies[s.callee] = d.s.value(stmt.Call.Fun, symbolic)
		// Origins are metadata. Keep their source companion live directly;
		// they must not pollute the saved callee's executable capture modes.
		originUse := func(expr ast.Expr) {
			if id, ok := loopUnparen(expr).(*ast.Ident); ok {
				if v := d.s.index.ids[id]; v != nil {
					uses[d.companion(v)] = true
				}
			}
		}
		originUse(stmt.Call.Fun)
		for i, arg := range stmt.Call.Args {
			dependencies[s.args[i]] = d.s.value(arg, symbolic)
			if aliases != nil {
				dependencies[s.addresses[i]] = aliases(arg, symbolic)
			}
			originUse(arg)
		}
		if p.persistent {
			// The tree keeps operand VALUES independently of source-cell
			// liveness. Its reference dependencies still load EXIT-time cells.
			// Use one owner root, not one liveness root per saved operand.
			value := loopValue{}
			for _, contents := range dependencies {
				loopMerge(value, contents)
			}
			dependencies = map[*loopVariable]loopValue{o.top: value}
		}
	}
	return
}

// companionDependencies must run before transitive liveness closure. Also pass
// d.companions as another map to loopPruneBindings, alongside address aliases.
func (d *loopDeferredFlow) companionDependencies(deps map[*loopVariable]map[*loopVariable]bool, roots ...[]map[*loopVariable]bool) {
	relevant := map[*loopVariable]bool{}
	for v, targets := range deps {
		relevant[v] = true
		for target := range targets {
			relevant[target] = true
		}
	}
	for _, blocks := range roots {
		for _, vars := range blocks {
			for v := range vars {
				relevant[v] = true
			}
		}
	}
	for v := range relevant {
		if slot := d.companions[v]; slot != nil {
			if deps[v] == nil {
				deps[v] = map[*loopVariable]bool{}
			}
			deps[v][slot] = true
		}
	}
}

// invocations uses registration-time values and exit-time captured bindings.
// A parameter's private address slot must use the same aliasSlot allocator as
// ordinary flow. The companion slot follows the same rule for callable origins.
func (p *loopDeferredPlan) invocations(s *loopDeferredSlot, state map[*loopVariable]loopValue, aliasSlot func(*loopVariable) *loopVariable) []loopDeferredInvocation {
	return p.savedInvocations(s, state, state, aliasSlot)
}

// Saved operands are immutable throughout an exit. Keep them outside the
// per-event binding fixed point: a callee can update source cells, never the
// caller's private registration storage (including enclosing iterator owners).
func (p *loopDeferredPlan) savedInvocations(s *loopDeferredSlot, state, saved map[*loopVariable]loopValue, aliasSlot func(*loopVariable) *loopVariable) []loopDeferredInvocation {
	d := p.flow
	base := loopDeferredInvocation{stmt: s.stmt, owner: s.owner, callee: saved[s.callee]}
	for i := range s.args {
		base.args = append(base.args, saved[s.args[i]])
		base.addresses = append(base.addresses, saved[s.addresses[i]])
	}
	makeInvocation := func(lit *ast.FuncLit) loopDeferredInvocation {
		inv := base
		inv.literal = lit
		inv.seed = loopCloneValues(state)
		for v, reference := range base.callee {
			if reference&loopPriorRef != 0 && loopEpochCurrentMode(reference) == 0 {
				// Current bindings are not the contents of an old allocation.
				delete(inv.seed, v)
				delete(inv.seed, d.companion(v))
				delete(inv.seed, d.aliases[v])
			}
		}
		if lit != nil && lit.Type.Params != nil {
			i := 0
			for _, field := range lit.Type.Params.List {
				for _, name := range field.Names {
					value, address, origins := loopValue{}, loopValue{}, loopValue{}
					if _, variadic := field.Type.(*ast.Ellipsis); variadic {
						for j := i; j < len(base.args); j++ {
							loopMerge(value, base.args[j])
							loopMerge(origins, saved[s.argOrigins[j]])
						}
						// The variadic parameter is a fresh slice, not an alias to
						// the storage addressed by its first argument.
					} else if i < len(base.args) {
						loopMerge(value, base.args[i])
						loopMerge(address, base.addresses[i])
						loopMerge(origins, saved[s.argOrigins[i]])
					}
					if v := d.s.index.ids[name]; v != nil {
						inv.seed[v], inv.seed[d.companion(v)] = value, origins
						if aliasSlot != nil {
							inv.seed[aliasSlot(v)] = address
						}
					}
					i++
				}
			}
		}
		active := loopValue{}
		for v, mode := range base.callee {
			active[v] = loopActivateCallableMode(mode)
		}
		inv.effects = d.s.expand(active, &loopWalk{values: inv.seed, aliases: d.aliases})
		return inv
	}
	var out []loopDeferredInvocation
	for token := range saved[s.origins] {
		if lit := d.literals[token]; lit != nil {
			out = append(out, makeInvocation(lit))
		} else if token == d.opaque {
			out = append(out, makeInvocation(nil))
		}
	}
	if len(out) == 0 {
		out = append(out, makeInvocation(nil))
	}
	return out
}

// loopDeferredEvents is the physical execution order to install at an exit.
// Allocate a point per registration, connect entries from the return-assignment
// phase, next edges from a completed invocation to older invocations, and done
// edges to the result-copy phase. empty also permits a direct result-copy edge.
// A registration with multiple callee origins branches at its point, not into
// an artificial sequence of invocations. Keep each stmt as its activation guard.
type loopDeferredEvents struct {
	entries []*ast.DeferStmt
	empty   bool
	next    map[*ast.DeferStmt][]*ast.DeferStmt
	done    map[*ast.DeferStmt]bool
}

func (p *loopDeferredPlan) events(node ast.Node, w *loopWalk) loopDeferredEvents {
	events := loopDeferredEvents{next: map[*ast.DeferStmt][]*ast.DeferStmt{}, done: map[*ast.DeferStmt]bool{}}
	exit, ok := p.exits[node]
	if !ok {
		return events
	}
	o := p.flow.owner(exit.owner)
	byToken := map[*loopVariable]*loopDeferredSlot{}
	for _, s := range o.slots {
		byToken[s.token] = s
	}
	var queue []*loopDeferredSlot
	for token := range w.values[o.top] {
		if s := byToken[token]; s != nil {
			events.entries = append(events.entries, s.stmt)
			queue = append(queue, s)
		} else if token == o.bottom {
			events.empty = true
		}
	}
	if len(w.values[o.top]) == 0 {
		events.empty = true
	}
	seen := map[*loopDeferredSlot]bool{}
	for head := 0; head < len(queue); head++ {
		s := queue[head]
		if seen[s] {
			continue
		}
		seen[s] = true
		previous := p.previous(s, w.values)
		if p.static[s].previous == nil {
			previous = p.flow.savedValue(w, s.previous)
		}
		if len(previous) == 0 {
			events.done[s.stmt] = true
		}
		for token := range previous {
			if older := byToken[token]; older != nil {
				events.next[s.stmt] = append(events.next[s.stmt], older.stmt)
				queue = append(queue, older)
			} else if token == o.bottom {
				events.done[s.stmt] = true
			}
		}
	}
	return events
}

// executeExit solves the finite saved-stack graph in LIFO order. A backedge
// represents repeated registrations at one syntax site; it is iterated to a
// binding fixed point, NOT truncated after one invocation. Branch alternatives
// join only at their actual predecessor event. The returned state is the state
// after all defers, suitable for the subsequent named-result copy.
//
// Registration/path correlations beyond the base CFG's may-analysis are not
// invented here. To avoid branch-exclusive false conflicts, the caller's event
// projection must retain the registration guard in addition to exit reachability.
// The stmt on every invocation is the guard's exact original identity.
func (p *loopDeferredPlan) executeExit(node ast.Node, w *loopWalk, aliasSlot func(*loopVariable) *loopVariable, run loopDeferredRunner, replay bool) map[*loopVariable]loopValue {
	exit, ok := p.exits[node]
	if !ok {
		return w.values
	}
	d := p.flow
	o := d.owner(exit.owner)
	byToken := map[*loopVariable]*loopDeferredSlot{}
	for _, s := range o.slots {
		byToken[s.token] = s
	}
	// Factor ALL owners' hidden storage, not just this owner's: iterator exits
	// may execute while a large enclosing stack is pending. Source-cell updates
	// are the only runner writebacks, so this table remains shared and immutable.
	saved := p.savedOperands(w, o)
	bindings := make(map[*loopVariable]loopValue)
	for v, value := range w.values {
		if !d.savedSlots[v] {
			bindings[v] = value
		}
	}
	inputs := map[*loopVariable]map[*loopVariable]loopValue{}
	queue := []*loopVariable{}
	queued := map[*loopVariable]bool{}
	push := func(token *loopVariable, state map[*loopVariable]loopValue) {
		first := inputs[token] == nil
		changed := false
		if first {
			// Event inputs are immutable. A linear stack with no binding
			// changes can share ONE source-cell map across every event too.
			inputs[token] = state
		} else {
			inputs[token], changed = loopDeferredJoinBindings(inputs[token], state)
		}
		if (first || changed) && !queued[token] {
			queue = append(queue, token)
			queued[token] = true
		}
	}
	if len(w.values[o.top]) == 0 {
		push(o.bottom, bindings)
	}
	for token := range w.values[o.top] {
		push(token, bindings)
	}
	for len(queue) != 0 {
		token := queue[0]
		queue = queue[1:]
		queued[token] = false
		s := byToken[token]
		if s == nil {
			continue
		}
		input := inputs[token]
		var output map[*loopVariable]loopValue
		completed := false
		for _, inv := range p.savedInvocations(s, input, saved, aliasSlot) {
			next := input
			if run != nil {
				if result := run(inv, loopCloneValues(input), false); result != nil {
					next = result
				}
			}
			if len(next[d.blocked]) != 0 {
				continue
			}
			if loopContextBindingsEqual(input, next) {
				next = input
			}
			if !completed {
				output = next
			} else {
				output, _ = loopDeferredJoinBindings(output, next)
			}
			completed = true
		}
		if !completed {
			continue
		}
		previous := p.previous(s, saved)
		if len(previous) == 0 {
			push(o.bottom, output)
		}
		for older := range previous {
			push(older, output)
		}
	}
	if replay && run != nil {
		for token, input := range inputs {
			if s := byToken[token]; s != nil {
				for _, inv := range p.savedInvocations(s, input, saved, aliasSlot) {
					run(inv, loopCloneValues(input), true)
				}
			}
		}
	}
	if result := inputs[o.bottom]; result != nil {
		completed := &loopWalk{values: loopCloneValues(result), persistentDeferred: w.persistentDeferred, deferredBindings: w.deferredBindings}
		for v, value := range w.values {
			if d.savedSlots[v] {
				_, operand := d.savedIDs[v]
				if !w.persistentDeferred || !operand {
					completed.values[v] = value
				}
			}
		}
		d.enter(exit.owner, completed) // this invocation's registrations are consumed
		w.deferredBindings = completed.deferredBindings
		return completed.values
	}
	// A pure cycle denotes no completed exit (e.g. an unbounded registration
	// loop). Do not manufacture a result-copy access after a non-returning path.
	return nil
}

// returnInfo also works for iterator markers whose original ReturnStmt was
// removed from the physical CFG. A nested void implicit return never inherits
// its enclosing declaration's named result variables.
func (p *loopDeferredPlan) returnInfo(node ast.Node) (*ast.ReturnStmt, []*loopVariable, bool) {
	exit, ok := p.returns[node]
	if !ok {
		return nil, nil, false
	}
	var fields *ast.FieldList
	if exit.owner != nil {
		fields = exit.owner.Type.Results
	} else if decl := p.flow.s.env.ctx.decl; decl != nil {
		fields = decl.Type.Results
	}
	var results []*loopVariable
	if fields != nil {
		for _, field := range fields.List {
			if len(field.Names) == 0 {
				results = append(results, nil)
			}
			for _, name := range field.Names {
				// Blank results have no storage but still occupy a tuple slot.
				results = append(results, p.flow.s.index.ids[name])
			}
		}
	}
	return exit.result, results, true
}

// transferDeferredReturn is the fixed-point/replay return-assignment hook.
// It evaluates all RHS values/address proofs against the same pre-return state.
// Ordinary multi-result calls remain opaque, as elsewhere in loopFlow.transfer.
func (f *loopFlow) transferDeferredReturn(p *loopDeferredPlan, node ast.Node, w *loopWalk) {
	ret, results, ok := p.returnInfo(node)
	if !ok || ret == nil || len(ret.Results) == 0 || p.returns[node].completionOnly {
		return
	}
	values, addresses, origins := make([]loopValue, len(results)), make([]loopValue, len(results)), make([]loopValue, len(results))
	for i := range results {
		if i < len(ret.Results) {
			values[i] = f.s.value(ret.Results[i], w)
			addresses[i] = f.addressAliases(ret.Results[i], w)
			origins[i] = p.flow.origins(ret.Results[i], w)
		}
	}
	for i, v := range results {
		if v == nil {
			continue
		}
		w.values[v] = values[i]
		w.values[f.aliasSlot(v)] = addresses[i]
		w.values[p.flow.companion(v)] = origins[i]
	}
}

// recordReturnBefore replaces recordFlowEffects(ReturnStmt) in replay. The
// caller must refine ordinary synchronous operand calls at their normal sites.
func (p *loopDeferredPlan) recordReturnBefore(node ast.Node, w *loopWalk, e *loopFlowEffects) {
	ret, results, ok := p.returnInfo(node)
	if !ok || ret == nil || e == nil || p.returns[node].completionOnly {
		return
	}
	for _, result := range ret.Results {
		e.recordFlowEffects(result, w)
	}
	if len(ret.Results) != 0 {
		next := e.context(ret, w)
		next.owner = p.returns[node].owner
		for _, v := range results {
			if v != nil {
				e.access(node, v, loopWrite, ret.End(), &next)
			}
		}
	}
}

// recordReturnAfter belongs AFTER executeExit, and only on completed exits.
func (p *loopDeferredPlan) recordReturnAfter(node ast.Node, w *loopWalk, e *loopFlowEffects) {
	ret, results, ok := p.returnInfo(node)
	if _, exits := p.exits[node]; !exits || !ok || ret == nil || e == nil {
		return
	}
	next := e.context(ret, w)
	next.owner = p.exits[node].owner
	for _, v := range results {
		if v != nil {
			e.access(node, v, loopRead, ret.End(), &next)
		}
	}
}

// recordOpaqueDeferredInvocation records execution at the matching exit. Bind
// the physical sink to this defer event, not its registration operand nodes.
func (e *loopFlowEffects) recordOpaqueDeferredInvocation(inv loopDeferredInvocation, exit ast.Node, pos token.Pos, w *loopWalk) {
	if e == nil {
		return
	}
	next := *w
	next.owner = inv.owner
	if loc, ok := e.s.index.locations[exit]; ok {
		next.loops = loc.loops
	}
	for v, mode := range inv.effects {
		e.access(exit, v, mode, pos, &next)
	}
}

// loopDeferredGuardGraph builds a two-state product for ONE registration guard.
// Use on an already context-projected frame during conflict collection; don't
// publish it as another constructor/invocation context. Install that defer's
// delayed accesses/escaping launches ONLY at mapping[exit][1], never in the
// original graph. Normal accesses/launches remain in both reachable states.
//
// registrations are physical registration-complete points; resets are this
// owner's fresh-entry points (particularly iterator entries, NOT yield/resume).
// This prevents a defer on one branch conflicting with a Go on an exclusive
// branch just because their states join at a shared exit. Cost is two points
// per original point, not a product of all registrations' active bitsets.
func loopDeferredGuardGraph(original loopSyncEscapeFrame, registrations, resets []int) (loopSyncEscapeFrame, [][2]int) {
	source := original.graph
	mapping := make([][2]int, len(source.points))
	for i := range mapping {
		mapping[i] = [2]int{-1, -1}
	}
	set, reset := map[int]bool{}, map[int]bool{}
	for _, point := range registrations {
		set[point] = true
	}
	for _, point := range resets {
		reset[point] = true
	}
	g := &loopConflictFrame{
		nodes: map[ast.Node][]int{}, gos: map[*ast.GoStmt]int{}, effects: map[int][]loopDirectAccess{},
		copies: map[*loopVariable][]int{}, births: map[*loopVariable][]int{}, iterations: map[*loopVariable][]int{},
	}
	out := loopSyncEscapeFrame{graph: g}
	if len(source.points) == 0 {
		return out, mapping
	}
	type state struct{ point, active int }
	queue := []state{}
	add := func(point, active int) int {
		if mapping[point][active] < 0 {
			mapping[point][active] = g.point()
			queue = append(queue, state{point, active})
		}
		return mapping[point][active]
	}
	add(0, 0)
	for head := 0; head < len(queue); head++ {
		current := queue[head]
		active := current.active
		if reset[current.point] {
			active = 0
		}
		if set[current.point] {
			active = 1
		}
		for _, next := range source.points[current.point].next {
			to := add(next, active)
			g.edge(mapping[current.point][current.active], to)
		}
	}
	points := func(old int) []int {
		var out []int
		for _, point := range mapping[old] {
			if point >= 0 {
				out = append(out, point)
			}
		}
		return out
	}
	for node, oldPoints := range source.nodes {
		for _, old := range oldPoints {
			g.nodes[node] = append(g.nodes[node], points(old)...)
		}
	}
	for stmt, old := range source.gos {
		if ps := points(old); len(ps) != 0 {
			g.gos[stmt] = ps[0]
		}
	}
	for old, accesses := range source.effects {
		for _, point := range points(old) {
			g.effects[point] = append(g.effects[point], accesses...)
		}
	}
	for v, oldPoints := range source.copies {
		for _, old := range oldPoints {
			g.copies[v] = append(g.copies[v], points(old)...)
		}
	}
	for v, oldPoints := range source.births {
		for _, old := range oldPoints {
			g.births[v] = append(g.births[v], points(old)...)
		}
	}
	for v, oldPoints := range source.iterations {
		for _, old := range oldPoints {
			g.iterations[v] = append(g.iterations[v], points(old)...)
		}
	}
	for _, launch := range original.launches {
		for _, point := range points(launch.point) {
			copy := launch
			copy.point = point
			out.launches = append(out.launches, copy)
		}
	}
	return out, mapping
}
