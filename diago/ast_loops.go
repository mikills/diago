package diago

import (
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

type loopContext struct {
	pkg, path string
	fset      *token.FileSet
	fn        string
	types     *types.Info
	file      *ast.File
	decl      *ast.FuncDecl
	// Effective language version of this file, not the analyzer's toolchain.
	// Empty/unknown versions deliberately retain pre-1.22 semantics.
	goVersion string
}

type loopEnv struct {
	findings *[]ASTFinding
	ctx      loopContext
	shade    map[string]bool // retained for the caller; resolution is identity-based
}

type loopAccess uint32

const (
	loopRead loopAccess = 1 << iota
	loopWrite
	// Writes through a pointer read its slot but mutate retained pointee storage.
	loopIndirectWrite
	// Retaining a variable's storage does not itself load or mutate its value.
	loopRetain
	// Calling a retained function value executes its contents; copying it does not.
	loopInvoke
	// This retained reference names storage from an earlier fresh allocation.
	loopPriorRef
	// Invoking a function field/index may first load a pointed-to container.
	loopFieldInvoke loopAccess = 1 << 7
	// Loading a header is a physical slot read, not a read of retained data.
	// Keep it outside both the ordinary read bit and existing latent modes.
	loopHeaderRead   loopAccess = 1 << 16
	loopIndirectRead loopAccess = 1 << 17
	// A pointer to a wrapper is crossed before an embedded pointer's data.
	loopEmbeddedPointer loopAccess = 1 << 19
	loopReadAccessMask             = loopRead | loopHeaderRead
)

type loopVariable struct {
	ident    *ast.Ident
	typeExpr ast.Expr // declared type, also available without go/types
	// Cached syntax only; retained values always come from loopWalk.values.
	valueInitializer      ast.Expr
	valueInitializerKnown bool
	owner                 *ast.FuncLit
	birth                 *loopRegion // storage is new on each iteration of this loop
	// Last direct access in each function frame. Literal bodies are not direct
	// accesses of the function that constructs the literal.
	reads, writes map[*ast.FuncLit]token.Pos
}

type loopRegion struct {
	node   ast.Node
	parent *loopRegion
	owner  *ast.FuncLit
	writes map[*loopVariable]bool
	// A three-clause loop copies its fresh header variables before the post.
	copies        map[*loopVariable]bool
	copyReachable bool // at least one CFG path reaches the per-iteration copy
}

type loopScopeIndex struct {
	parent *loopScopeIndex
	vars   map[string]*loopVariable
}

type loopLocation struct {
	owner *ast.FuncLit
	loops []*loopRegion
}

type loopIndex struct {
	ctx       loopContext
	fresh     bool
	ids       map[*ast.Ident]*loopVariable
	typed     map[types.Object]*loopVariable
	parsed    map[*ast.Object]*loopVariable
	external  map[string]*loopVariable
	regions   map[ast.Node]*loopRegion
	captures  map[*ast.FuncLit]map[*loopVariable]loopAccess
	direct    []loopDirectAccess
	locations map[ast.Node]loopLocation
	reachable map[ast.Node]bool
	scope     *loopScopeIndex
	loops     []*loopRegion
	lits      []*ast.FuncLit
	// Shared by short-lived selector probes and the scanner.
	receiverSyntax *loopReceiverSyntax
}

func scanLoops(env loopEnv, n ast.Node) {
	if n == nil {
		return
	}
	x := &loopIndex{
		ctx: env.ctx, fresh: freshLoopVariables(env.ctx.goVersion),
		ids: map[*ast.Ident]*loopVariable{}, typed: map[types.Object]*loopVariable{},
		parsed: map[*ast.Object]*loopVariable{}, external: map[string]*loopVariable{},
		regions: map[ast.Node]*loopRegion{}, captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	if env.ctx.decl != nil {
		x.fields(env.ctx.decl.Recv)
		x.fields(env.ctx.decl.Type.Params)
		x.fields(env.ctx.decl.Type.Results)
	}
	if body, ok := n.(*ast.BlockStmt); ok {
		// The function's parameters and outer body share the short-declaration
		// scope. A legal p, x := ... redeclares p rather than shadowing it.
		for _, stmt := range body.List {
			x.node(stmt)
		}
	} else {
		x.node(n)
	}
	s := &loopScanner{env: env, index: x, rootNode: n}
	s.discover(n, &loopWalk{})
	s.resetCaptureEffects()
	for {
		s.refineLaunchBindings(n)
		if !s.updateCaptureEffects() {
			break
		}
	}
	s.report()
}

func freshLoopVariables(v string) bool {
	v = strings.TrimPrefix(v, "go")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	major, e1 := strconv.Atoi(parts[0])
	minor, e2 := strconv.Atoi(parts[1])
	return e1 == nil && e2 == nil && (major > 1 || major == 1 && minor >= 22)
}

func (x *loopIndex) owner() *ast.FuncLit {
	if len(x.lits) == 0 {
		return nil
	}
	return x.lits[len(x.lits)-1]
}

func (x *loopIndex) innermost() *loopRegion {
	if len(x.loops) == 0 {
		return nil
	}
	return x.loops[len(x.loops)-1]
}

func (x *loopIndex) pushScope() {
	x.scope = &loopScopeIndex{parent: x.scope, vars: map[string]*loopVariable{}}
}

func (x *loopIndex) object(id *ast.Ident) types.Object {
	if x.ctx.types != nil {
		if obj := x.ctx.types.Defs[id]; obj != nil {
			return obj
		}
		return x.ctx.types.Uses[id]
	}
	return nil
}

func (x *loopIndex) remember(id *ast.Ident, v *loopVariable) {
	if v.ident == nil {
		v.ident = id
	}
	x.ids[id] = v
	if obj := x.object(id); obj != nil {
		x.typed[obj] = v
	}
	if id.Obj != nil {
		x.parsed[id.Obj] = v
	}
}

func (x *loopIndex) define(id *ast.Ident) *loopVariable {
	if id == nil || id.Name == "_" {
		return nil
	}
	v := &loopVariable{owner: x.owner(), birth: x.innermost(),
		reads: map[*ast.FuncLit]token.Pos{}, writes: map[*ast.FuncLit]token.Pos{}}
	x.scope.vars[id.Name] = v
	x.remember(id, v)
	return v
}

func (x *loopIndex) resolve(id *ast.Ident) *loopVariable {
	if id == nil || id.Name == "_" {
		return nil
	}
	if v := x.ids[id]; v != nil {
		return v
	}
	var v *loopVariable
	if obj := x.object(id); obj != nil {
		v = x.typed[obj]
	}
	if v == nil && id.Obj != nil {
		v = x.parsed[id.Obj]
	}
	// Missing Uses entries are not evidence that an identifier is unrelated.
	// Resolve each missing entry lexically, rather than trusting partial info.
	if v == nil {
		for scope := x.scope; scope != nil; scope = scope.parent {
			if v = scope.vars[id.Name]; v != nil {
				break
			}
		}
	}
	if v == nil {
		v = x.external[id.Name]
		if v == nil {
			v = &loopVariable{reads: map[*ast.FuncLit]token.Pos{}, writes: map[*ast.FuncLit]token.Pos{}}
			x.external[id.Name] = v
		}
	}
	x.remember(id, v)
	return v
}

func (x *loopIndex) access(id *ast.Ident, mode loopAccess) {
	v := x.resolve(id)
	if v == nil {
		return
	}
	owner := x.owner()
	x.recordLocation(id)
	x.direct = append(x.direct, loopDirectAccess{node: id, variable: v, owner: owner, mode: mode, pos: id.Pos()})
	if mode&loopReadAccessMask != 0 && id.Pos() > v.reads[owner] {
		v.reads[owner] = id.Pos()
	}
	if mode&loopWrite != 0 {
		if id.Pos() > v.writes[owner] {
			v.writes[owner] = id.Pos()
		}
		for _, l := range x.loops {
			if l.owner == owner {
				l.writes[v] = true
			}
		}
	}
	// Propagate free-variable summaries through nested literals once. A local
	// of an enclosing literal is not a capture of that enclosing literal.
	for i := len(x.lits) - 1; i >= 0; i-- {
		lit := x.lits[i]
		if v.owner == lit {
			break
		}
		x.captures[lit][v] |= mode
	}
}

func (x *loopIndex) fields(list *ast.FieldList) {
	if list == nil {
		return
	}
	for _, f := range list.List {
		for _, id := range f.Names {
			if v := x.define(id); v != nil {
				v.typeExpr = f.Type
			}
		}
	}
}

// children visits immediate children only. The recursive visitors below handle
// scope-bearing nodes explicitly, including a FuncLit passed as the root.
func loopChildren(n ast.Node, visit func(ast.Node)) {
	ast.Inspect(n, func(child ast.Node) bool {
		if child == n {
			return true
		}
		if child != nil {
			visit(child)
		}
		return false
	})
}

func (x *loopIndex) recordLocation(node ast.Node) {
	if x.locations == nil {
		x.locations = map[ast.Node]loopLocation{}
	}
	x.locations[node] = loopLocation{owner: x.owner(), loops: append([]*loopRegion{}, x.loops...)}
}

func (x *loopIndex) node(n ast.Node) {
	if n == nil {
		return
	}
	x.recordLocation(n)
	switch n := n.(type) {
	case ast.Expr:
		x.expressionNode(n)
	case ast.Stmt:
		x.statementNode(n)
	case *ast.ValueSpec:
		x.valueSpecNode(n)
	case *ast.TypeSpec:
		// Type names need identities for shadowing, but fields are not uses.
		if v := x.define(n.Name); v != nil {
			v.typeExpr = n.Type
		}
	default:
		loopChildren(n, x.node)
	}
}

func (x *loopIndex) expressionNode(n ast.Expr) {
	switch n := n.(type) {
	case *ast.Ident:
		x.access(n, loopRead)
	case *ast.StarExpr:
		x.readPointee(n.X)
	case *ast.IndexExpr:
		x.indexExprNode(n)
	case *ast.UnaryExpr:
		if n.Op == token.AND {
			x.addressOperand(n.X)
		} else {
			x.node(n.X)
		}
	case *ast.CallExpr:
		x.callOperands(n)
		x.markInvocation(n.Fun)
	case *ast.SelectorExpr:
		x.selectorExprNode(n)
	case *ast.CompositeLit:
		x.compositeLitNode(n)
	case *ast.StructType, *ast.InterfaceType, *ast.FuncType:
		// Field, method, and parameter names in types are not references.
	case *ast.FuncLit:
		x.funcLitNode(n)
	default:
		loopChildren(n, x.node)
	}
}

func (x *loopIndex) indexExprNode(n *ast.IndexExpr) {
	x.resolveOperand(n.X)
	probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
	if probe.referenceIndex(n.X) {
		x.readPointee(n.X)
	} else {
		x.node(n.X)
	}
	x.node(n.Index)
}

func (x *loopIndex) selectorExprNode(n *ast.SelectorExpr) {
	x.resolveOperand(n.X)
	probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
	if probe.receiverImplicitAddress(n) {
		// Binding s.M to a pointer receiver computes &s; it does not
		// copy s. Invocation activates the receiver's data read later.
		x.addressOperand(n.X)
	} else if hint := probe.methodReceiverHint(n); hint.known && hint.pointerReceiver {
		x.methodReceiverPathOperand(n, hint)
	} else if probe.methodCopiesPointee(n) {
		x.readPointee(n.X) // Value receivers copy the final receiver's data.
	} else if hint := probe.fieldReceiverHint(n); hint.field && hint.throughPointer {
		x.fieldPathOperand(n, hint, loopRead)
	} else {
		x.node(n.X)
	}
}

func (x *loopIndex) compositeLitNode(n *ast.CompositeLit) {
	isMap := x.mapLiteral(n)
	for _, elt := range n.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if isMap {
				x.node(kv.Key)
			}
			x.node(kv.Value)
		} else {
			x.node(elt)
		}
	}
}

func (x *loopIndex) funcLitNode(n *ast.FuncLit) {
	x.pushScope()
	x.lits = append(x.lits, n)
	x.captures[n] = map[*loopVariable]loopAccess{}
	x.fields(n.Type.Params)
	x.fields(n.Type.Results)
	// Parameters and the outermost body share a short-declaration scope.
	for _, stmt := range n.Body.List {
		x.node(stmt)
	}
	x.lits = x.lits[:len(x.lits)-1]
	x.scope = x.scope.parent
}

// statementNode handles simple statements and loops. Blocks, conditionals,
// switches, and their clauses go to blockNode.
func (x *loopIndex) statementNode(n ast.Stmt) {
	switch n := n.(type) {
	case *ast.GoStmt:
		x.callOperands(n.Call)
	case *ast.DeferStmt:
		x.callOperands(n.Call)
	case *ast.BranchStmt:
		// Labels live in a separate namespace.
	case *ast.LabeledStmt:
		x.node(n.Stmt)
	case *ast.AssignStmt:
		x.assignStmtNode(n)
	case *ast.IncDecStmt:
		x.lhs(n.X, loopRead|loopWrite)
	case *ast.ForStmt:
		x.loop(n, n.Init, nil, func() {
			x.node(n.Cond)
			x.node(n.Post)
			x.node(n.Body)
		})
	case *ast.RangeStmt:
		x.rangeStmtNode(n)
	default:
		x.blockNode(n)
	}
}

// blockNode visits explicit and implicit blocks, each in a new scope. Any
// other statement only visits its children.
func (x *loopIndex) blockNode(n ast.Stmt) {
	switch n := n.(type) {
	case *ast.BlockStmt:
		x.pushScope()
		for _, stmt := range n.List {
			x.node(stmt)
		}
		x.scope = x.scope.parent
	case *ast.IfStmt:
		x.pushScope()
		x.node(n.Init)
		x.node(n.Cond)
		x.node(n.Body)
		x.node(n.Else)
		x.scope = x.scope.parent
	case *ast.SwitchStmt:
		x.pushScope()
		x.node(n.Init)
		x.node(n.Tag)
		x.node(n.Body)
		x.scope = x.scope.parent
	case *ast.TypeSwitchStmt:
		x.pushScope()
		x.node(n.Init)
		x.node(n.Assign)
		x.node(n.Body)
		x.scope = x.scope.parent
	case *ast.CaseClause:
		x.pushScope()
		for _, expr := range n.List {
			x.node(expr)
		}
		for _, stmt := range n.Body {
			x.node(stmt)
		}
		x.scope = x.scope.parent
	case *ast.CommClause:
		x.pushScope()
		x.node(n.Comm)
		for _, stmt := range n.Body {
			x.node(stmt)
		}
		x.scope = x.scope.parent
	default:
		loopChildren(n, x.node)
	}
}

func (x *loopIndex) assignStmtNode(n *ast.AssignStmt) {
	for _, rhs := range n.Rhs {
		x.node(rhs)
	}
	for i, lhs := range n.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && n.Tok == token.DEFINE && x.scope.vars[id.Name] == nil {
			if v := x.define(id); v != nil && len(n.Rhs) == len(n.Lhs) {
				v.valueInitializer = n.Rhs[i]
				v.valueInitializerKnown = true
			}
		} else {
			mode := loopWrite
			if n.Tok != token.ASSIGN && n.Tok != token.DEFINE {
				mode |= loopRead
			}
			x.lhs(lhs, mode)
		}
	}
}

func (x *loopIndex) rangeStmtNode(n *ast.RangeStmt) {
	evaluated := x.rangeExpressionEvaluated(n)
	if evaluated {
		x.node(n.X) // evaluated outside the new iteration scope
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if probe.iteratorCallable(n.X) {
			x.markInvocation(n.X)
		}
	}
	x.loop(n, nil, n, func() {
		// A skipped array operand still declares a fresh integer key.
		if !evaluated && n.Tok == token.DEFINE {
			if id, ok := n.Key.(*ast.Ident); ok {
				if v := x.ids[id]; v != nil {
					v.typeExpr = ast.NewIdent("int")
				}
			}
		}
		x.node(n.Body)
	})
}

func (x *loopIndex) valueSpecNode(n *ast.ValueSpec) {
	for _, value := range n.Values {
		x.node(value)
	}
	for i, id := range n.Names {
		if v := x.define(id); v != nil {
			v.typeExpr = n.Type
			if i < len(n.Values) {
				v.valueInitializer = n.Values[i]
				v.valueInitializerKnown = true
			}
		}
	}
}

// Go/defer operands execute in the binding frame; only a synchronous call
// adds an invocation there. Launch summaries own asynchronous receiver reads.
func (x *loopIndex) callOperands(call *ast.CallExpr) {
	x.recordLocation(call)
	x.node(call.Fun)
	for _, arg := range call.Args {
		x.node(arg)
	}
}

func (x *loopIndex) mapLiteral(lit *ast.CompositeLit) bool {
	if info := x.ctx.types; info != nil {
		if typ := info.TypeOf(lit); typ != nil {
			_, ok := typ.Underlying().(*types.Map)
			return ok
		}
	}
	typ := lit.Type
	seen := map[*ast.Ident]bool{}
	for {
		switch t := loopUnparen(typ).(type) {
		case *ast.MapType:
			return true
		case *ast.Ident:
			if seen[t] {
				return false
			}
			seen[t] = true
			if t.Obj != nil {
				if spec, ok := t.Obj.Decl.(*ast.TypeSpec); ok {
					typ = spec.Type
					continue
				}
			}
			if v := x.resolve(t); v != nil && v.typeExpr != nil {
				typ = v.typeExpr
				continue
			}
		}
		return false
	}
}

// headerOperand loads only the slot used to compute an address or bind a
// pointer receiver. Indirect fields/indices still evaluate their data loads;
// suppressing those would hide reads needed to obtain the pointer itself.
func (x *loopIndex) headerOperand(expr ast.Expr) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		x.access(expr, loopHeaderRead)
	case *ast.SelectorExpr:
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if !probe.pointerValue(expr.X) {
			x.headerOperand(expr.X)
		} else {
			x.node(expr)
		}
	default:
		x.node(expr)
	}
}

// Computing an address retains storage without loading it. Pointer/slice
// headers and index expressions used to find that address are still evaluated.
func (x *loopIndex) addressOperand(expr ast.Expr) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		x.access(expr, loopRetain)
	case *ast.SelectorExpr:
		x.resolveOperand(expr.X)
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if hint := probe.fieldReceiverHint(expr); hint.field && hint.throughPointer {
			// The last pointer locates the field; its pointee is not loaded.
			hint.path = hint.path[:len(hint.path)-1]
			x.methodReceiverPathOperand(expr, hint)
		} else if probe.pointerValue(expr.X) {
			x.headerOperand(expr.X)
		} else {
			x.addressOperand(expr.X)
		}
	case *ast.IndexExpr:
		x.resolveOperand(expr.X)
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if probe.referenceIndex(expr.X) {
			x.headerOperand(expr.X)
		} else {
			x.addressOperand(expr.X)
		}
		x.node(expr.Index)
	case *ast.StarExpr:
		x.headerOperand(expr.X)
	default:
		x.node(expr)
	}
}

func (x *loopIndex) markInvocation(expr ast.Expr) {
	x.invocationRoot(expr, false)
}

func (x *loopIndex) invocationRoot(expr ast.Expr, field bool) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		mode := loopRead | loopInvoke
		if field {
			mode |= loopFieldInvoke
		}
		x.access(expr, mode)
	case *ast.SelectorExpr:
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		isField := probe.syntaxField(expr) || probe.receiverHint(expr.X, expr.Sel.Name).field
		if info := x.ctx.types; info != nil && info.Selections[expr] != nil {
			isField = info.Selections[expr].Kind() == types.FieldVal
		}
		if isField {
			x.invocationRoot(expr.X, true)
		} else if hint := probe.methodReceiverHint(expr); hint.known {
			// Opaque method calls may read receiver data, but are not calls
			// to any function values stored inside that receiver.
			if probe.pointerValue(expr.X) {
				x.readPointee(expr.X)
			} else {
				x.node(expr.X)
			}
		}
		// Methods bind/read receivers, not the callbacks stored in them.
		// Package selectors likewise never invoke a lexical receiver.
	case *ast.IndexExpr:
		x.invocationRoot(expr.X, true)
	case *ast.StarExpr:
		x.invocationRoot(expr.X, field)
	}
}

func (x *loopIndex) resolveOperand(expr ast.Expr) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		x.resolve(expr)
	case *ast.SelectorExpr:
		x.resolveOperand(expr.X)
	case *ast.IndexExpr:
		x.resolveOperand(expr.X)
	case *ast.StarExpr:
		x.resolveOperand(expr.X)
	case *ast.CallExpr:
		x.resolveOperand(expr.Fun)
	}
}

// readPointee records both the header load and the retained-data read. Selector
// indexing can call this for pointer field/value-receiver reads as well; binding
// a pointer-receiver method without invoking it must keep using headerOperand.
// Preserve nested pointer loads and evaluation of index operands as well.
func (x *loopIndex) readPointee(expr ast.Expr) {
	switch expr := loopUnparen(expr).(type) {
	case *ast.Ident:
		x.access(expr, loopRead|loopIndirectRead)
	case *ast.SelectorExpr:
		x.readPointee(expr.X)
	case *ast.IndexExpr:
		x.readPointee(expr.X)
		x.node(expr.Index)
	case *ast.StarExpr:
		x.readPointee(expr.X)
	default:
		x.node(expr)
	}
}

func (x *loopIndex) lhs(expr ast.Expr, mode loopAccess) {
	switch e := loopUnparen(expr).(type) {
	case *ast.Ident:
		x.access(e, mode)
	case *ast.SelectorExpr:
		x.resolveOperand(e.X)
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		hint := probe.fieldReceiverHint(e)
		if hint.field && hint.throughPointer && mode&(loopIndirectRead|loopIndirectWrite) != 0 {
			// e is itself a pointer field used by an enclosing selector. Its
			// containing pointee is read, not overwritten by that selector.
			x.lhs(e.X, mode|loopEmbeddedPointer)
		} else if hint.field && hint.throughPointer && mode&(loopRead|loopWrite) != 0 {
			x.fieldPathOperand(e, hint, mode)
		} else if mode&loopWrite != 0 && probe.pointerValue(e.X) {
			x.lhs(e.X, loopRead|loopIndirectWrite)
		} else {
			x.lhs(e.X, mode)
		}
	case *ast.IndexExpr:
		x.resolveOperand(e.X)
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if mode&loopWrite != 0 && probe.referenceIndex(e.X) {
			x.lhs(e.X, loopRead|loopIndirectWrite)
		} else {
			x.lhs(e.X, mode)
		}
		x.node(e.Index)
	case *ast.StarExpr:
		if mode&loopWrite != 0 {
			x.lhs(e.X, loopRead|loopIndirectWrite)
		} else {
			x.node(e.X)
		}
	default:
		x.node(expr)
	}
}

func (x *loopIndex) loop(n ast.Node, init ast.Stmt, rng *ast.RangeStmt, body func()) {
	l := &loopRegion{node: n, parent: x.innermost(), owner: x.owner(),
		writes: map[*loopVariable]bool{}, copies: map[*loopVariable]bool{}}
	x.regions[n] = l
	x.pushScope()
	// Init executes once; it is not a mutation on every iteration.
	x.node(init)
	if stmt, ok := init.(*ast.AssignStmt); ok {
		for _, lhs := range stmt.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				v := x.resolve(id)
				if v == nil {
					continue
				}
				if stmt.Tok == token.DEFINE && x.fresh {
					v.birth = l
					l.copies[v] = true
				}
			}
		}
	}
	x.loops = append(x.loops, l)
	if rng != nil {
		for _, expr := range []ast.Expr{rng.Key, rng.Value} {
			if expr == nil {
				continue
			}
			if id, ok := expr.(*ast.Ident); ok && rng.Tok == token.DEFINE {
				v := x.define(id)
				if v != nil {
					if !x.fresh {
						v.birth = l.parent
					}
					l.writes[v] = true
				}
			} else {
				x.lhs(expr, loopWrite)
			}
		}
	}
	body()
	x.loops = x.loops[:len(x.loops)-1]
	x.scope = x.scope.parent
}

type loopValue map[*loopVariable]loopAccess

type loopWalk struct {
	values             map[*loopVariable]loopValue
	aliases            map[*loopVariable]*loopVariable
	loops              []*loopRegion
	owner              *ast.FuncLit
	deferDepth         int
	reference          *loopDeferredInvocation // actual deferred execution's captured cells
	operands           map[*ast.CallExpr]*loopDeferredOperands
	deferredBindings   *loopDeferredBindings // immutable saved operands, shared by CFG inputs
	persistentDeferred bool
}

type loopLaunch struct {
	stmt   *ast.GoStmt
	owner  *ast.FuncLit
	loops  []*loopRegion
	values loopValue
}

type loopScanner struct {
	env             loopEnv
	index           *loopIndex
	rootNode        ast.Node
	receiverSyntax  *loopReceiverSyntax
	flowConflicts   *loopConflictHazards
	typeRetention   map[types.Type]bool
	syntaxRetention map[ast.Expr]bool
	launches        []loopLaunch
}

func loopUnparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func loopMerge(dst, src loopValue) {
	for v, mode := range src {
		dst[v] = loopEpochMergeMode(dst[v], mode)
	}
}

func loopCloneValues(src map[*loopVariable]loopValue) map[*loopVariable]loopValue {
	dst := make(map[*loopVariable]loopValue, len(src))
	for v, value := range src {
		dst[v] = value // values are immutable once assigned
	}
	return dst
}

// value describes only values which cross the go boundary. In particular, a
// CallExpr is a barrier: its arguments run synchronously, and its result is not
// assumed to retain every address or closure used to compute it.
func (s *loopScanner) value(e ast.Expr, w *loopWalk) loopValue {
	switch e := loopUnparen(e).(type) {
	case *ast.FuncLit:
		out := loopValue{}
		for v, mode := range s.index.captures[e] {
			out[v] = w.referenceMode(v, loopRetain|(mode&loopCallableCaptureMask)<<loopCallableShift)
		}
		return out
	case *ast.Ident:
		out := loopValue{}
		if v := s.index.ids[e]; v != nil {
			loopMerge(out, w.values[v])
		}
		return out
	case *ast.UnaryExpr:
		return s.unaryExprValue(e, w)
	case *ast.SelectorExpr:
		return s.selectorExprValue(e, w)
	case *ast.IndexExpr:
		return s.indexExprValue(e, w)
	case *ast.SliceExpr:
		return s.sliceExprValue(e, w)
	case *ast.TypeAssertExpr:
		out := loopValue{}
		if s.retains(e) {
			loopMerge(out, s.value(e.X, w))
		}
		return out
	case *ast.StarExpr:
		return s.starExprValue(e, w)
	case *ast.CompositeLit:
		return s.compositeLitValue(e, w)
	case *ast.CallExpr:
		return s.callExprValue(e, w)
	}
	return loopValue{}
}

func (s *loopScanner) unaryExprValue(e *ast.UnaryExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if e.Op == token.AND {
		if v := s.root(e.X); v != nil {
			out[v] = w.referenceMode(v, loopRead)
		} else {
			loopMerge(out, s.addressValue(e.X, w))
		}
	}
	return out
}

func (s *loopScanner) selectorExprValue(e *ast.SelectorExpr, w *loopWalk) loopValue {
	if value, handled := s.promotedReceiverValue(e, w); handled {
		return value
	}
	if projected, ok := s.projection(e); ok {
		return s.value(projected, w)
	}
	info := s.env.ctx.types
	field := s.fieldReceiverHint(e).field
	if info != nil && info.Selections[e] != nil {
		sel := info.Selections[e]
		field = sel.Kind() == types.FieldVal
		if sel.Kind() == types.MethodExpr {
			return loopValue{} // a method expression does not bind a receiver
		}
	}
	if field {
		return s.selectedFieldValue(e, w)
	}
	return s.boundMethodValue(e, w)
}

func (s *loopScanner) selectedFieldValue(e *ast.SelectorExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if s.retains(e) {
		// An implicit pointer dereference copies the selected contents,
		// not the address of the aggregate containing the field.
		if loaded, known := s.loadKnownPointee(e.X, w); known {
			return loaded
		}
		loopMerge(out, s.value(e.X, w))
	}
	return out
}

func (s *loopScanner) boundMethodValue(e *ast.SelectorExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if !s.retains(e) {
		return out
	}
	// A value receiver reached via p copies *p now, not its storage.
	// Retained fields in that copy remain opaque receiver data.
	if s.methodCopiesPointee(e) {
		if loaded, known := s.loadKnownPointee(e.X, w); known {
			return loopBindMethodReceiver(loaded)
		}
	}
	// Explicit pointer receivers retain their receiver just as implicit
	// addressing does. Field values are handled by selectedFieldValue.
	loopMerge(out, s.value(e.X, w))
	if s.implicitAddress(e) {
		if v := s.root(e.X); v != nil {
			out[v] = w.referenceMode(v, loopRead)
		}
	}
	return loopBindMethodReceiver(out)
}

func (s *loopScanner) indexExprValue(e *ast.IndexExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if !s.retains(e) {
		return out
	}
	if projected, ok := s.projection(e); ok {
		return s.value(projected, w)
	}
	// The index (including a map key) is evaluated synchronously.
	if lit := s.directLiteral(e.X); lit != nil {
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
			}
			loopMerge(out, s.value(elt, w))
		}
	} else if loaded, known := s.loadKnownPointee(e.X, w); known {
		// A pointer-to-array index loads the current element summary.
		// Unknown heap/backing-store references keep the coarse fallback.
		loopMerge(out, loaded)
	} else {
		loopMerge(out, s.value(e.X, w))
	}
	return out
}

func (s *loopScanner) sliceExprValue(e *ast.SliceExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if projected, ok := s.sliceValue(e, w); ok {
		loopMerge(out, projected)
	} else {
		loopMerge(out, s.value(e.X, w))
	}
	// Slicing an array also retains its storage, not just its contents.
	if s.arrayValue(e.X) {
		if v := s.root(e.X); v != nil {
			out[v] = loopEpochMergeMode(out[v], w.referenceMode(v, loopRead))
		}
	}
	return out
}

func (s *loopScanner) starExprValue(e *ast.StarExpr, w *loopWalk) loopValue {
	out := loopValue{}
	if s.retains(e) {
		if loaded, known := s.loadKnownPointee(e.X, w); known {
			return loaded
		}
	}
	if s.callablePointer(e.X) {
		return s.loadCallablePointer(e.X, w)
	}
	if s.retains(e) {
		loopMerge(out, s.value(e.X, w))
	}
	return out
}

func (s *loopScanner) compositeLitValue(e *ast.CompositeLit, w *loopWalk) loopValue {
	out := loopValue{}
	_, isMap := s.underlyingSyntax(e.Type).(*ast.MapType)
	if info := s.env.ctx.types; info != nil && info.TypeOf(e) != nil {
		_, isMap = info.TypeOf(e).Underlying().(*types.Map)
	}
	for _, elt := range e.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if isMap {
				loopMerge(out, s.value(kv.Key, w))
			}
			loopMerge(out, s.value(kv.Value, w))
		} else {
			loopMerge(out, s.value(elt, w))
		}
	}
	return out
}

func (s *loopScanner) callExprValue(e *ast.CallExpr, w *loopWalk) loopValue {
	out := loopValue{}
	// A type conversion preserves its operand; an ordinary call does not
	// imply that any descendant expressions escape.
	if len(e.Args) == 1 && s.conversion(e.Fun) {
		loopMerge(out, s.value(e.Args[0], w))
	}
	return out
}

// A closure captures function/pointer variables by reference, not a snapshot
// of their old values. Follow current bindings, activating callable contents
// only at invocation edges. Latent modes remain latent through aggregate reads.
func (s *loopScanner) expand(value loopValue, w *loopWalk) loopValue {
	const follow = loopRead | loopIndirectRead | loopIndirectWrite | loopInvoke | loopFieldInvoke | loopEmbeddedPointer
	needsExpansion := false
	for v, mode := range value {
		if mode&follow != 0 && len(w.values[v]) != 0 {
			needsExpansion = true
			break
		}
	}
	if !needsExpansion {
		return value
	}
	out := loopValue{}
	loopMerge(out, value)
	var pending []*loopVariable
	for v, mode := range value {
		if mode&follow != 0 {
			pending = append(pending, v)
		}
	}
	// A read can reach a slot before a different path invokes it. Reprocess
	// newly added modes rather than letting traversal order decide its effects.
	seen := map[*loopVariable]loopAccess{}
	for len(pending) != 0 {
		v := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		access := loopEpochCurrentMode(out[v]) & follow
		if access&^seen[v] == 0 {
			continue
		}
		seen[v] |= access
		invokePointer := access&loopInvoke != 0 && v.ident != nil && s.callablePointer(v.ident)
		for captured, mode := range w.values[v] {
			// Field/index invocation follows only proven local address edges.
			// Closure captures and unknown aggregate contents are not aliases.
			if access&loopFieldInvoke != 0 && s.knownInvocationAlias(v, captured, w) {
				mode |= loopInvoke | loopFieldInvoke
			}
			// Only a statically known pointer-to-callable carries ordinary
			// invocation through its address chain. A function's captured
			// reads (including function comparisons) are not invocation edges.
			if invokePointer && mode&loopRead != 0 {
				mode |= loopInvoke
			}
			if access&loopInvoke != 0 {
				mode = loopActivateCallableMode(mode)
			}
			if access&loopIndirectWrite != 0 && mode&(loopRead|loopWrite|loopIndirectRead|loopIndirectWrite) != 0 {
				mode |= loopWrite
			}
			mode = embeddedFieldPathMode(loopEpochCurrentMode(out[v]), mode, s.knownInvocationAlias(v, captured, w))
			mode = methodPathHeaderMode(loopEpochCurrentMode(out[v]), mode, s.knownInvocationAlias(v, captured, w))
			before := out[captured]
			out[captured] = loopEpochMergeMode(out[captured], mode)
			if loopEpochCurrentMode(out[captured])&follow != loopEpochCurrentMode(before)&follow {
				pending = append(pending, captured)
			}
		}
	}
	return out
}

func (s *loopScanner) root(e ast.Expr) *loopVariable {
	switch e := loopUnparen(e).(type) {
	case *ast.Ident:
		return s.index.ids[e]
	case *ast.SelectorExpr:
		if hint := s.fieldReceiverHint(e); hint.field && hint.throughPointer {
			return nil // promoted field storage is beyond an embedded pointer
		}
		if info := s.env.ctx.types; info != nil {
			if typ := info.TypeOf(e.X); typ != nil {
				if _, pointer := typ.Underlying().(*types.Pointer); pointer {
					return nil // field storage is in the pointee, not the reused pointer slot
				}
			}
		}
		if _, pointer := loopUnparen(s.syntaxType(e.X)).(*ast.StarExpr); pointer {
			return nil
		}
		return s.root(e.X)
	case *ast.IndexExpr:
		// An array element lives in the variable; a slice element does not.
		if typ := s.syntaxType(e.X); typ != nil {
			if array, ok := typ.(*ast.ArrayType); !ok || array.Len == nil {
				return nil
			}
		}
		if info := s.env.ctx.types; info != nil {
			if t := info.TypeOf(e.X); t != nil {
				if _, ok := t.Underlying().(*types.Array); !ok {
					return nil
				}
			}
		}
		return s.root(e.X)
	}
	return nil
}

// Syntax recovery is used only for types, never to substitute a variable's
// initializer for its current binding summary (which may have been overwritten).
func (s *loopScanner) syntaxType(e ast.Expr) ast.Expr {
	return s.syntaxTypeSeen(e, map[*loopVariable]bool{})
}

func (s *loopScanner) syntaxTypeSeen(e ast.Expr, seen map[*loopVariable]bool) ast.Expr {
	switch e := loopUnparen(e).(type) {
	case *ast.Ident:
		return s.identSyntaxType(e, seen)
	case *ast.CompositeLit:
		return s.underlyingSyntax(e.Type)
	case *ast.FuncLit:
		return e.Type
	case *ast.UnaryExpr:
		return s.unaryExprSyntaxType(e, seen)
	case *ast.StarExpr:
		if typ, ok := s.syntaxTypeSeen(e.X, seen).(*ast.StarExpr); ok {
			return s.underlyingSyntax(typ.X)
		}
	case *ast.SelectorExpr:
		return s.selectorExprSyntaxType(e, seen)
	case *ast.IndexExpr:
		return s.indexExprSyntaxType(e, seen)
	case *ast.SliceExpr:
		if typ, ok := s.syntaxTypeSeen(e.X, seen).(*ast.ArrayType); ok {
			return &ast.ArrayType{Elt: typ.Elt}
		}
	case *ast.TypeAssertExpr:
		return s.underlyingSyntax(e.Type)
	case *ast.CallExpr:
		return s.callExprSyntaxType(e)
	case *ast.BasicLit:
		// All literal kinds are scalar for retention purposes.
		return ast.NewIdent("int")
	}
	return nil
}

func (s *loopScanner) identSyntaxType(e *ast.Ident, seen map[*loopVariable]bool) ast.Expr {
	v := s.index.ids[e]
	if v != nil {
		if v.typeExpr != nil {
			return s.underlyingSyntax(v.typeExpr)
		}
		if seen[v] {
			return nil
		}
		seen[v] = true
		defer delete(seen, v)
	}
	if e.Obj != nil {
		if typ, declared := s.objectSyntaxType(e.Obj, seen); declared {
			return typ
		}
	}
	// SkipObjectResolution removes declaration links. The indexed loop
	// regions still contain local declarations, with identity-resolved LHSs.
	if v != nil {
		if rhs := s.syntaxInitializer(v); rhs != nil {
			return s.syntaxTypeSeen(rhs, seen)
		}
	}
	return nil
}

// objectSyntaxType reads a type from the declaration the parser linked to obj.
// It reports false when that declaration neither states nor initializes obj.
func (s *loopScanner) objectSyntaxType(obj *ast.Object, seen map[*loopVariable]bool) (ast.Expr, bool) {
	switch d := obj.Decl.(type) {
	case *ast.ValueSpec:
		if d.Type != nil {
			return s.underlyingSyntax(d.Type), true
		}
		for i, id := range d.Names {
			if id.Obj == obj && i < len(d.Values) {
				return s.syntaxTypeSeen(d.Values[i], seen), true
			}
		}
	case *ast.Field:
		return s.underlyingSyntax(d.Type), true
	case *ast.AssignStmt:
		for i, lhs := range d.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Obj == obj && i < len(d.Rhs) {
				return s.syntaxTypeSeen(d.Rhs[i], seen), true
			}
		}
	}
	return nil, false
}

func (s *loopScanner) unaryExprSyntaxType(e *ast.UnaryExpr, seen map[*loopVariable]bool) ast.Expr {
	if e.Op == token.AND {
		if typ := s.syntaxTypeSeen(e.X, seen); typ != nil {
			return &ast.StarExpr{X: typ}
		}
	}
	return nil
}

func (s *loopScanner) selectorExprSyntaxType(e *ast.SelectorExpr, seen map[*loopVariable]bool) ast.Expr {
	typ, _, _ := s.fieldSyntax(s.syntaxTypeSeen(e.X, seen), e.Sel.Name)
	if typ == nil {
		if hint := s.receiverHint(e.X, e.Sel.Name); hint.field && len(hint.path) != 0 {
			typ = hint.path[len(hint.path)-1].typ
		}
	}
	return s.underlyingSyntax(typ)
}

func (s *loopScanner) indexExprSyntaxType(e *ast.IndexExpr, seen map[*loopVariable]bool) ast.Expr {
	typ := s.syntaxTypeSeen(e.X, seen)
	if p, ok := typ.(*ast.StarExpr); ok {
		typ = s.underlyingSyntax(p.X)
	}
	switch typ := typ.(type) {
	case *ast.ArrayType:
		return s.underlyingSyntax(typ.Elt)
	case *ast.MapType:
		return s.underlyingSyntax(typ.Value)
	}
	return nil
}

func (s *loopScanner) callExprSyntaxType(e *ast.CallExpr) ast.Expr {
	if id, ok := loopUnparen(e.Fun).(*ast.Ident); ok && len(e.Args) > 0 && s.index.external[id.Name] == s.index.ids[id] {
		switch id.Name {
		case "new":
			return &ast.StarExpr{X: s.underlyingSyntax(e.Args[0])}
		case "make":
			return s.underlyingSyntax(e.Args[0])
		}
	}
	if s.conversion(e.Fun) {
		return s.underlyingSyntax(e.Fun)
	}
	return nil
}

func (s *loopScanner) syntaxInitializer(v *loopVariable) ast.Expr {
	if v.valueInitializerKnown {
		return v.valueInitializer
	}
	v.valueInitializerKnown = true
	var rhs ast.Expr
	for _, region := range s.index.regions {
		ast.Inspect(region.node, func(n ast.Node) bool {
			if rhs != nil {
				return false
			}
			switch n := n.(type) {
			case *ast.AssignStmt:
				if n.Tok == token.DEFINE && len(n.Lhs) == len(n.Rhs) {
					for i, lhs := range n.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && s.index.ids[id] == v {
							rhs = n.Rhs[i]
						}
					}
				}
			case *ast.ValueSpec:
				for i, id := range n.Names {
					if s.index.ids[id] == v && i < len(n.Values) {
						rhs = n.Values[i]
					}
				}
			}
			return true
		})
		if rhs != nil {
			break
		}
	}
	v.valueInitializer = rhs
	return rhs
}

func (s *loopScanner) underlyingSyntax(typ ast.Expr) ast.Expr {
	seen := map[ast.Expr]bool{}
	for typ != nil && !seen[typ] {
		seen[typ] = true
		typ = loopUnparen(typ)
		id, ok := typ.(*ast.Ident)
		if !ok {
			return typ
		}
		if id.Obj != nil {
			if d, ok := id.Obj.Decl.(*ast.TypeSpec); ok {
				typ = d.Type
				continue
			}
		}
		if decl := s.receiverDeclarations().declaration(id); decl != nil {
			typ = decl.Type
			continue
		}
		var declared ast.Expr
		var pos token.Pos
		for name, v := range s.index.ids {
			if name.Name == id.Name && name.Pos() < id.Pos() && name.Pos() > pos && v.typeExpr != nil {
				declared, pos = v.typeExpr, name.Pos()
			}
		}
		if declared == nil {
			return typ
		}
		typ = declared
	}
	return nil
}

func (s *loopScanner) fieldSyntax(typ ast.Expr, name string) (ast.Expr, int, bool) {
	typ = s.underlyingSyntax(typ)
	if p, ok := typ.(*ast.StarExpr); ok {
		typ = s.underlyingSyntax(p.X)
	}
	if st, ok := typ.(*ast.StructType); ok {
		index := 0
		for _, field := range st.Fields.List {
			if len(field.Names) == 0 {
				index++
			}
			for _, id := range field.Names {
				if id.Name == name {
					return field.Type, index, true
				}
				index++
			}
		}
	}
	return nil, 0, false
}

func (s *loopScanner) syntaxField(e *ast.SelectorExpr) bool {
	_, _, ok := s.fieldSyntax(s.syntaxType(e.X), e.Sel.Name)
	return ok
}

// retains distinguishes copied scalars from values that can carry references.
// Unknown syntax remains conservative, but never turns a CallExpr into an
// argument traversal: value still enforces the synchronous-call barrier.
func (s *loopScanner) retains(e ast.Expr) bool {
	if info := s.env.ctx.types; info != nil {
		if typ := info.TypeOf(e); typ != nil {
			if s.typeRetention == nil {
				s.typeRetention = map[types.Type]bool{}
			}
			return loopRetainedTypeMemo(typ, s.typeRetention)
		}
	}
	return s.retainedSyntax(s.syntaxType(e), map[ast.Expr]bool{})
}

func loopRetainedType(typ types.Type) bool {
	return loopRetainedTypeMemo(typ, map[types.Type]bool{})
}

func loopRetainedTypeMemo(typ types.Type, memo map[types.Type]bool) (retained bool) {
	if value, ok := memo[typ]; ok {
		return value
	}
	defer func() { memo[typ] = retained }()
	switch typ := typ.Underlying().(type) {
	case *types.Basic:
		return typ.Kind() == types.UnsafePointer
	case *types.Array:
		return loopRetainedTypeMemo(typ.Elem(), memo)
	case *types.Struct:
		for i := 0; i < typ.NumFields(); i++ {
			if loopRetainedTypeMemo(typ.Field(i).Type(), memo) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (s *loopScanner) retainedSyntax(typ ast.Expr, seen map[ast.Expr]bool) (retained bool) {
	typ = s.underlyingSyntax(typ)
	if typ == nil || seen[typ] {
		return true
	}
	if value, ok := s.syntaxRetention[typ]; ok {
		return value
	}
	seen[typ] = true
	defer func() {
		delete(seen, typ)
		if s.syntaxRetention == nil {
			s.syntaxRetention = map[ast.Expr]bool{}
		}
		s.syntaxRetention[typ] = retained
	}()
	switch typ := typ.(type) {
	case *ast.Ident:
		switch typ.Name {
		case "bool", "string", "byte", "rune", "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128":
			return false
		}
	case *ast.ArrayType:
		return typ.Len == nil || s.retainedSyntax(typ.Elt, seen)
	case *ast.StructType:
		for _, field := range typ.Fields.List {
			if s.retainedSyntax(field.Type, seen) {
				return true
			}
		}
		return false
	}
	return true
}

func (s *loopScanner) arrayValue(e ast.Expr) bool {
	if info := s.env.ctx.types; info != nil && info.TypeOf(e) != nil {
		_, ok := info.TypeOf(e).Underlying().(*types.Array)
		return ok
	}
	typ, ok := s.syntaxType(e).(*ast.ArrayType)
	return ok && typ.Len != nil
}

func (s *loopScanner) conversion(fun ast.Expr) bool {
	if info := s.env.ctx.types; info != nil {
		if tv, ok := info.Types[fun]; ok {
			return tv.IsType()
		}
	}
	switch fun := loopUnparen(fun).(type) {
	case *ast.ArrayType, *ast.StructType, *ast.InterfaceType, *ast.MapType, *ast.ChanType, *ast.FuncType:
		return true
	case *ast.StarExpr:
		return s.conversion(fun.X) // (*f)(arg) may instead call a function value
	case *ast.Ident:
		if fun.Obj != nil {
			_, ok := fun.Obj.Decl.(*ast.TypeSpec)
			return ok
		}
		// Only recognize unshadowed predeclared conversions without type info.
		if s.index.external[fun.Name] == s.index.ids[fun] {
			switch fun.Name {
			case "any", "bool", "string", "byte", "rune", "int", "int8", "int16", "int32", "int64",
				"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "float32", "float64", "complex64", "complex128":
				return true
			}
		}
	}
	return false
}

// directLiteral follows only synchronous projections/conversions, not variable
// initializers. Named values deliberately use the current coarse binding map.
func (s *loopScanner) directLiteral(e ast.Expr) *ast.CompositeLit {
	switch e := loopUnparen(e).(type) {
	case *ast.CompositeLit:
		return e
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return s.directLiteral(e.X)
		}
	case *ast.StarExpr:
		return s.directLiteral(e.X)
	case *ast.TypeAssertExpr:
		return s.directLiteral(e.X)
	case *ast.SelectorExpr, *ast.IndexExpr:
		if projected, ok := s.projection(e); ok {
			return s.directLiteral(projected)
		}
	case *ast.CallExpr:
		if len(e.Args) == 1 && s.conversion(e.Fun) {
			return s.directLiteral(e.Args[0])
		}
	}
	return nil
}

// projection returns exactly the selected initializer when it is statically
// known. A nil expression with ok=true denotes an omitted (zero) field/element.
func (s *loopScanner) projection(e ast.Expr) (ast.Expr, bool) {
	switch e := loopUnparen(e).(type) {
	case *ast.SelectorExpr:
		lit := s.directLiteral(e.X)
		if lit == nil {
			return nil, false
		}
		_, index, field := s.fieldSyntax(s.syntaxType(e.X), e.Sel.Name)
		for i, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if name, ok := kv.Key.(*ast.Ident); ok && name.Name == e.Sel.Name {
					return kv.Value, true
				}
			} else if field && i == index {
				return elt, true
			}
		}
		return nil, field
	case *ast.IndexExpr:
		lit := s.directLiteral(e.X)
		key, known := s.projectionKey(e.Index)
		if lit == nil || !known {
			return nil, false
		}
		_, isMap := s.underlyingSyntax(lit.Type).(*ast.MapType)
		if info := s.env.ctx.types; info != nil && info.TypeOf(lit) != nil {
			_, isMap = info.TypeOf(lit).Underlying().(*types.Map)
		}
		next := int64(0)
		for _, elt := range lit.Elts {
			eltKey := "i:" + strconv.FormatInt(next, 10)
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				var known bool
				eltKey, known = s.projectionKey(kv.Key)
				if !known {
					return nil, false
				}
				if !isMap {
					n, err := strconv.ParseInt(strings.TrimPrefix(eltKey, "i:"), 10, 64)
					if err != nil {
						return nil, false
					}
					next = n
				}
				elt = kv.Value
			}
			if eltKey == key {
				return elt, true
			}
			next++
		}
		return nil, true
	}
	return nil, false
}

// A slice can be resliced to its capacity, but cannot expose a discarded
// prefix. Full slices can also exclude a suffix. Only statically known bounds
// on direct literals are refined; named backing stores use the binding summary.
func (s *loopScanner) sliceValue(e *ast.SliceExpr, w *loopWalk) (loopValue, bool) {
	lit := s.directLiteral(e.X)
	if lit == nil {
		return nil, false
	}
	low, limit := int64(0), int64(-1)
	bound := func(expr ast.Expr) (int64, bool) {
		key, ok := s.projectionKey(expr)
		if !ok || !strings.HasPrefix(key, "i:") {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(key, "i:"), 10, 64)
		return n, err == nil && n >= 0
	}
	if e.Low != nil {
		var ok bool
		low, ok = bound(e.Low)
		if !ok {
			return nil, false
		}
	}
	if e.Slice3 {
		var ok bool
		limit, ok = bound(e.Max)
		if !ok {
			return nil, false
		}
	}
	out := loopValue{}
	next := int64(0)
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			var known bool
			next, known = bound(kv.Key)
			if !known {
				return nil, false
			}
			elt = kv.Value
		}
		if next >= low && (limit < 0 || next < limit) {
			loopMerge(out, s.value(elt, w))
		}
		next++
	}
	return out, true
}

func (s *loopScanner) projectionKey(e ast.Expr) (string, bool) {
	if lit, ok := loopUnparen(e).(*ast.BasicLit); ok {
		switch lit.Kind {
		case token.INT:
			n, err := strconv.ParseInt(strings.ReplaceAll(lit.Value, "_", ""), 0, 64)
			return "i:" + strconv.FormatInt(n, 10), err == nil
		case token.STRING:
			v, err := strconv.Unquote(lit.Value)
			return "s:" + v, err == nil
		}
	}
	return "", false
}

func (s *loopScanner) implicitAddress(e *ast.SelectorExpr) bool {
	info := s.env.ctx.types
	if info != nil {
		if typ := info.TypeOf(e.X); typ != nil {
			if _, pointer := typ.Underlying().(*types.Pointer); pointer {
				return false
			}
		}
	}
	if _, pointer := loopUnparen(s.syntaxType(e.X)).(*ast.StarExpr); pointer {
		return false
	}
	if info == nil || info.Selections[e] == nil {
		if hint := s.receiverHint(e.X, e.Sel.Name); hint.known {
			return hint.pointerReceiver && !hint.throughPointer
		}
		if s.promotedThroughPointer(e.X, e.Sel.Name) {
			return false
		}
		// Without a selection we cannot distinguish a pointer receiver from
		// a value receiver. Be conservative, but only for this direct value.
		return true
	}
	sel := info.Selections[e]
	if sel.Kind() != types.MethodVal {
		return false
	}
	// A promoted method reached through an embedded pointer binds that
	// pointee, not the outer struct's reused storage.
	receiver := sel.Recv()
	path := sel.Index()
	for i := 0; i+1 < len(path); i++ {
		st, ok := receiver.Underlying().(*types.Struct)
		if !ok {
			return false
		}
		receiver = st.Field(path[i]).Type()
		if _, pointer := receiver.Underlying().(*types.Pointer); pointer {
			return false
		}
	}
	sig, ok := sel.Obj().Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	if _, ptr := sig.Recv().Type().(*types.Pointer); !ptr {
		return false
	}
	_, alreadyPointer := sel.Recv().Underlying().(*types.Pointer)
	return !alreadyPointer
}

// join preserves immutable value snapshots held by earlier launches and aliases.
func loopJoinBindings(dst, src map[*loopVariable]loopValue) bool {
	changed := false
	for v, value := range src {
		for captured, mode := range value {
			if loopEpochMergeMode(dst[v][captured], mode) != dst[v][captured] {
				merged := loopValue{}
				loopMerge(merged, dst[v])
				loopMerge(merged, value)
				dst[v] = merged
				changed = true
				break
			}
		}
	}
	return changed
}

func (s *loopScanner) walk(n ast.Node, w *loopWalk) {
	s.discover(n, w)
}

func (s *loopScanner) finding(n ast.Node, rule, message string) {
	line := 0
	if s.env.ctx.fset != nil {
		line = s.env.ctx.fset.Position(n.Pos()).Line
	}
	loc := astLocation{pkg: s.env.ctx.pkg, file: s.env.ctx.path, line: line}
	*s.env.findings = append(*s.env.findings, astFinding(rule, "high", loc, s.env.ctx.fn, message))
}

func loopShared(v *loopVariable, l *loopRegion) bool {
	// Parameters and locals of a literal created within this loop belong to
	// separate function invocations, not to the surrounding loop's frame.
	if v.owner != nil && v.owner.Pos() > l.node.Pos() && v.owner.End() <= l.node.End() {
		return false
	}
	for birth := v.birth; birth != nil; birth = birth.parent {
		if birth == l {
			return false
		}
	}
	return true
}

func (s *loopScanner) report() {
	if s.flowConflicts != nil {
		s.reportFlowConflicts()
		return
	}
	siblings := loopSiblingHazards(s.rootNode, s.launches)
	direct := loopDirectHazards(s.rootNode, s.launches, s.index.direct)
	sharedWriters := loopSharedWriters(s.launches)
	for _, launch := range s.launches {
		if len(launch.loops) == 0 {
			continue
		}
		if siblings[launch.stmt] || direct[launch.stmt] || loopLaunchHazard(launch, sharedWriters) {
			s.finding(launch.stmt, "goroutine-in-loop", "goroutine launched inside loop captures shared mutable variable")
		}
	}
}

func (s *loopScanner) reportFlowConflicts() {
	for _, launch := range s.launches {
		if len(launch.loops) == 0 && !s.flowConflicts.loopCalls[launch.stmt] {
			continue
		}
		stmt := launch.stmt
		if s.flowConflicts.direct[stmt] || s.flowConflicts.siblings[stmt] || s.flowConflicts.copies[stmt] {
			s.finding(stmt, "goroutine-in-loop", "goroutine launched inside loop captures shared mutable variable")
		}
	}
}

// loopSharedWriters indexes, for each loop, the shared variables written by a
// launch from that loop's own frame.
func loopSharedWriters(launches []loopLaunch) map[*loopRegion]map[*loopVariable]bool {
	sharedWriters := map[*loopRegion]map[*loopVariable]bool{}
	for _, launch := range launches {
		for v, mode := range launch.values {
			if mode&(loopWrite|loopIndirectWrite) == 0 {
				continue
			}
			for _, l := range launch.loops {
				if l.owner != launch.owner || !loopShared(v, l) {
					continue
				}
				if sharedWriters[l] == nil {
					sharedWriters[l] = map[*loopVariable]bool{}
				}
				sharedWriters[l][v] = true
			}
		}
	}
	return sharedWriters
}

func loopLaunchHazard(launch loopLaunch, sharedWriters map[*loopRegion]map[*loopVariable]bool) bool {
	for v, mode := range launch.values {
		if mode&(loopReadAccessMask|loopWrite|loopIndirectWrite) == 0 {
			continue
		}
		for _, l := range launch.loops {
			if loopCaptureHazard(v, mode, l, sharedWriters) {
				return true
			}
		}
	}
	return false
}

// loopCaptureHazard reports whether a launch's access to v conflicts with a
// write to storage that iterations of l share, or writes a header variable
// that l copies into its next iteration.
func loopCaptureHazard(v *loopVariable, mode loopAccess, l *loopRegion, sharedWriters map[*loopRegion]map[*loopVariable]bool) bool {
	if loopShared(v, l) && (l.writes[v] || sharedWriters[l][v] || mode&(loopWrite|loopIndirectWrite) != 0) {
		return true
	}
	return mode&loopWrite != 0 && l.copyReachable && l.copies[v]
}
