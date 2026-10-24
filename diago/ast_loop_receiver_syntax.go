package diago

import (
	"go/ast"
	"go/token"
	"go/types"
)

type loopReceiverSyntax struct {
	types   map[string]*ast.TypeSpec
	methods map[string]map[string]bool // method presence -> pointer receiver
	hints   map[loopReceiverSyntaxKey]loopReceiverSyntaxHint
	locals  []loopReceiverLocalType
}

type loopReceiverLocalType struct {
	decl *ast.TypeSpec
	end  token.Pos
}

type loopReceiverSyntaxKey struct {
	typ    ast.Expr
	method string
}

type loopReceiverSyntaxHint struct {
	known, pointerReceiver, throughPointer bool
	receiverPointer                        bool
	field                                  bool
	// Fields traversed before the selected method (or including a selected
	// field). indirect describes the containing value, not the field's type.
	path []loopReceiverField
}

type loopReceiverField struct {
	name     string
	indirect bool
	pointer  bool     // the selected field itself is a pointer
	typ      ast.Expr // syntax-only field type, for fallback receiver chains
}

// Index probes are short lived. Share declaration and method-path caches with
// the scanner rather than rescanning every file declaration for each selector.
func (s *loopScanner) receiverDeclarations() *loopReceiverSyntax {
	if s.receiverSyntax != nil {
		return s.receiverSyntax
	}
	if s.index != nil && s.index.receiverSyntax != nil {
		s.receiverSyntax = s.index.receiverSyntax
		return s.receiverSyntax
	}
	r := &loopReceiverSyntax{
		types: map[string]*ast.TypeSpec{}, methods: map[string]map[string]bool{},
		hints: map[loopReceiverSyntaxKey]loopReceiverSyntaxHint{},
	}
	if s.env.ctx.file != nil {
		for _, decl := range s.env.ctx.file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if spec, ok := spec.(*ast.TypeSpec); ok {
						r.types[spec.Name.Name] = spec
					}
				}
			case *ast.FuncDecl:
				if decl.Recv == nil || len(decl.Recv.List) == 0 {
					continue
				}
				typ := loopUnparen(decl.Recv.List[0].Type)
				ptr, pointer := typ.(*ast.StarExpr)
				if pointer {
					typ = loopUnparen(ptr.X)
				}
				if id, ok := loopReceiverBaseType(typ).(*ast.Ident); ok {
					if r.methods[id.Name] == nil {
						r.methods[id.Name] = map[string]bool{}
					}
					r.methods[id.Name][decl.Name.Name] = pointer
				}
			}
		}
		var stack []ast.Node
		ast.Inspect(s.env.ctx.file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if spec, ok := node.(*ast.TypeSpec); ok {
				for i := len(stack) - 1; i >= 0; i-- {
					if block, ok := stack[i].(*ast.BlockStmt); ok {
						r.locals = append(r.locals, loopReceiverLocalType{spec, block.End()})
						break
					}
				}
			}
			stack = append(stack, node)
			return true
		})
	}
	s.receiverSyntax = r
	if s.index != nil {
		s.index.receiverSyntax = r
	}
	return r
}

func (r *loopReceiverSyntax) declaration(id *ast.Ident) *ast.TypeSpec {
	if id.Obj != nil {
		if decl, ok := id.Obj.Decl.(*ast.TypeSpec); ok {
			return decl
		}
	}
	decl := r.types[id.Name]
	for _, local := range r.locals {
		if local.decl.Name.Name == id.Name && local.decl.Pos() <= id.Pos() && id.Pos() < local.end &&
			(decl == nil || local.decl.Pos() > decl.Pos()) {
			decl = local.decl
		}
	}
	return decl
}

func loopReceiverBaseType(typ ast.Expr) ast.Expr {
	switch typ := loopUnparen(typ).(type) {
	case *ast.IndexExpr:
		return loopReceiverBaseType(typ.X)
	case *ast.IndexListExpr:
		return loopReceiverBaseType(typ.X)
	default:
		return typ
	}
}

// Keep the declared name: syntaxType deliberately erases named structs, which
// would lose the receiver method set. Initializers cover short declarations.
func (s *loopScanner) receiverDeclaredType(expr ast.Expr, seen map[*loopVariable]bool) ast.Expr {
	switch expr := loopUnparen(expr).(type) {
	case *ast.CompositeLit:
		return expr.Type
	case *ast.Ident:
		if v := s.index.ids[expr]; v != nil {
			if v.typeExpr != nil {
				return v.typeExpr
			}
			if seen[v] {
				return nil
			}
			seen[v] = true
			if init := s.syntaxInitializer(v); init != nil {
				return s.receiverDeclaredType(init, seen)
			}
		}
		if expr.Obj != nil {
			switch decl := expr.Obj.Decl.(type) {
			case *ast.ValueSpec:
				return decl.Type
			case *ast.Field:
				return decl.Type
			}
		}
	case *ast.SelectorExpr:
		hint := s.receiverHint(expr.X, expr.Sel.Name)
		if hint.field && len(hint.path) != 0 {
			return hint.path[len(hint.path)-1].typ
		}
	case *ast.StarExpr:
		if typ, ok := loopUnparen(s.receiverDeclaredType(expr.X, seen)).(*ast.StarExpr); ok {
			return typ.X
		}
	case *ast.UnaryExpr:
		if expr.Op == token.AND {
			if typ := s.receiverDeclaredType(expr.X, seen); typ != nil {
				return &ast.StarExpr{X: typ}
			}
		}
	}
	return s.syntaxType(expr)
}

func (s *loopScanner) receiverHint(expr ast.Expr, method string) loopReceiverSyntaxHint {
	r := s.receiverDeclarations()
	declared := s.receiverDeclaredType(expr, map[*loopVariable]bool{})
	key := loopReceiverSyntaxKey{declared, method}
	if hint, ok := r.hints[key]; ok {
		return hint
	}
	hint := r.lookup(declared, method)
	r.hints[key] = hint
	return hint
}

func (r *loopReceiverSyntax) lookup(declared ast.Expr, method string) loopReceiverSyntaxHint {
	type candidate struct {
		typ     ast.Expr
		pointer bool
		path    []loopReceiverField
		seen    map[ast.Expr]bool
	}
	level := []candidate{{typ: declared}}
	for len(level) != 0 {
		var next []candidate
		matches := 0
		var hint loopReceiverSyntaxHint
		for _, c := range level {
			typ := loopUnparen(c.typ)
			_, indirect := typ.(*ast.StarExpr)
			if p, ok := typ.(*ast.StarExpr); ok {
				typ, c.pointer = loopUnparen(p.X), true
			}
			typ = loopReceiverBaseType(typ)
			// Resolve aliases without consuming a promotion level. In particular
			// *Alias must keep its containing-pointer edge when Alias = Inner.
			aliases := map[ast.Expr]bool{}
			for {
				id, ok := typ.(*ast.Ident)
				if !ok || aliases[typ] {
					break
				}
				aliases[typ] = true
				decl := r.declaration(id)
				if decl == nil || !decl.Assign.IsValid() {
					break
				}
				typ = loopReceiverBaseType(decl.Type)
				if p, ok := typ.(*ast.StarExpr); ok {
					typ, indirect, c.pointer = loopReceiverBaseType(p.X), true, true
				}
			}
			if typ == nil || c.seen[typ] {
				continue
			}
			seen := make(map[ast.Expr]bool, len(c.seen)+1)
			for ancestor := range c.seen {
				seen[ancestor] = true
			}
			seen[typ] = true
			if id, ok := typ.(*ast.Ident); ok {
				decl := r.declaration(id)
				if pointer, ok := r.methods[id.Name][method]; ok && decl == r.types[id.Name] {
					matches++
					hint = loopReceiverSyntaxHint{known: true, pointerReceiver: pointer, throughPointer: c.pointer,
						receiverPointer: indirect, path: c.path}
					continue
				}
				if decl == nil {
					continue
				}
				typ = loopUnparen(decl.Type)
			}
			if st, ok := typ.(*ast.StructType); ok {
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if name.Name == method {
							matches++ // A field hides deeper promoted methods.
							hint = loopReceiverSyntaxHint{field: true, throughPointer: c.pointer,
								path: r.appendField(c.path, name.Name, indirect, field.Type)}
						}
					}
					if len(field.Names) == 0 {
						name := loopReceiverEmbeddedName(field.Type)
						path := r.appendField(c.path, name, indirect, field.Type)
						if name == method {
							matches++
							hint = loopReceiverSyntaxHint{field: true, throughPointer: c.pointer, path: path}
						} else {
							next = append(next, candidate{typ: field.Type, pointer: c.pointer, path: path, seen: seen})
						}
					}
				}
			}
		}
		if matches != 0 {
			if matches == 1 {
				return hint
			}
			return loopReceiverSyntaxHint{}
		}
		level = next
	}
	return loopReceiverSyntaxHint{}
}

// Positive syntax evidence is sufficient to avoid inventing an address of a
// wrapper when a promoted method binds an embedded pointer. Unknown/imported
// method sets remain conservative when type checking is unavailable.
func (s *loopScanner) promotedThroughPointer(expr ast.Expr, method string) bool {
	hint := s.receiverHint(expr, method)
	return hint.known && hint.throughPointer
}

// methodReceiverHint uses positive selection evidence when available, falling
// back to the shared local method-set cache for incomplete type information.
func (s *loopScanner) methodReceiverHint(expr *ast.SelectorExpr) loopReceiverSyntaxHint {
	if info := s.env.ctx.types; info != nil {
		if sel := info.Selections[expr]; sel != nil {
			if sel.Kind() != types.MethodVal {
				return loopReceiverSyntaxHint{}
			}
			sig, ok := sel.Obj().Type().(*types.Signature)
			if !ok || sig.Recv() == nil {
				return loopReceiverSyntaxHint{}
			}
			_, pointer := sig.Recv().Type().Underlying().(*types.Pointer)
			through := false
			typ := sel.Recv()
			for _, index := range sel.Index()[:len(sel.Index())-1] {
				if p, ok := typ.Underlying().(*types.Pointer); ok {
					through, typ = true, p.Elem()
				}
				st, ok := typ.Underlying().(*types.Struct)
				if !ok {
					break
				}
				typ = st.Field(index).Type()
			}
			_, receiverPointer := typ.Underlying().(*types.Pointer)
			if receiverPointer {
				through = true
			}
			return loopReceiverSyntaxHint{known: true, pointerReceiver: pointer, throughPointer: through,
				receiverPointer: receiverPointer, path: loopTypedReceiverPath(sel)}
		}
	}
	return s.receiverHint(expr.X, expr.Sel.Name)
}

func (s *loopScanner) methodCopiesPointee(expr *ast.SelectorExpr) bool {
	hint := s.methodReceiverHint(expr)
	return hint.known && !hint.pointerReceiver && (hint.throughPointer || s.pointerValue(expr.X))
}

// Unlike implicitAddress's conservative retention fallback, suppressing an
// actual read requires positive evidence of an implicitly addressed receiver.
func (s *loopScanner) receiverImplicitAddress(expr *ast.SelectorExpr) bool {
	if info := s.env.ctx.types; info != nil && info.Selections[expr] != nil {
		return s.implicitAddress(expr)
	}
	hint := s.receiverHint(expr.X, expr.Sel.Name)
	return hint.known && hint.pointerReceiver && !hint.throughPointer && !s.pointerValue(expr.X)
}

func loopReceiverEmbeddedName(typ ast.Expr) string {
	if pointer, ok := loopUnparen(typ).(*ast.StarExpr); ok {
		typ = pointer.X
	}
	switch typ := loopReceiverBaseType(typ).(type) {
	case *ast.Ident:
		return typ.Name
	case *ast.SelectorExpr:
		return typ.Sel.Name
	}
	return ""
}

func (r *loopReceiverSyntax) appendField(path []loopReceiverField, name string, indirect bool, typ ast.Expr) []loopReceiverField {
	next := append([]loopReceiverField(nil), path...)
	resolved := loopReceiverBaseType(typ)
	seen := map[ast.Expr]bool{}
	for !seen[resolved] {
		seen[resolved] = true
		id, ok := resolved.(*ast.Ident)
		if !ok {
			break
		}
		decl := r.declaration(id)
		if decl == nil {
			break
		}
		resolved = loopReceiverBaseType(decl.Type)
	}
	_, pointer := resolved.(*ast.StarExpr)
	return append(next, loopReceiverField{name: name, indirect: indirect, pointer: pointer, typ: typ})
}

// A selection's final index names the method, not another storage load. For a
// field selection it does name storage; only dereferences BEFORE that field
// matter to the address of the selected field (&s.Inner still addresses s).
func loopTypedReceiverPath(sel *types.Selection) []loopReceiverField {
	indices := sel.Index()
	if sel.Kind() != types.FieldVal {
		indices = indices[:len(indices)-1]
	}
	typ := sel.Recv()
	var path []loopReceiverField
	for _, index := range indices {
		pointer, indirect := typ.Underlying().(*types.Pointer)
		if indirect {
			typ = pointer.Elem()
		}
		st, ok := typ.Underlying().(*types.Struct)
		if !ok || index >= st.NumFields() {
			return nil
		}
		field := st.Field(index)
		_, fieldPointer := field.Type().Underlying().(*types.Pointer)
		path = append(path, loopReceiverField{name: field.Name(), indirect: indirect, pointer: fieldPointer})
		typ = field.Type()
	}
	return path
}

func (s *loopScanner) fieldReceiverHint(expr *ast.SelectorExpr) loopReceiverSyntaxHint {
	if info := s.env.ctx.types; info != nil {
		if sel := info.Selections[expr]; sel != nil {
			if sel.Kind() != types.FieldVal {
				return loopReceiverSyntaxHint{}
			}
			hint := loopReceiverSyntaxHint{field: true, path: loopTypedReceiverPath(sel)}
			for _, field := range hint.path {
				hint.throughPointer = hint.throughPointer || field.indirect
			}
			return hint
		}
	}
	return s.receiverHint(expr.X, expr.Sel.Name)
}

func loopReceiverFieldExpr(base ast.Expr, field loopReceiverField, pos token.Pos) ast.Expr {
	return &ast.SelectorExpr{X: base, Sel: &ast.Ident{NamePos: pos, Name: field.name}}
}

// fieldPathOperand keeps the selected storage separate from the wrapper's
// header. Only fields BEFORE the selected field can redirect a store: assigning
// s.Inner replaces the wrapper's pointer slot, while assigning s.N follows it.
func (x *loopIndex) fieldPathOperand(expr *ast.SelectorExpr, hint loopReceiverSyntaxHint, mode loopAccess) {
	indirect := loopRead
	if mode&loopWrite != 0 {
		indirect |= loopIndirectWrite
	}
	if mode&loopRead != 0 {
		indirect |= loopIndirectRead
	}
	if len(hint.path) > 1 && hint.path[0].indirect && loopReceiverHasPointerField(hint.path[:len(hint.path)-1]) {
		indirect |= loopEmbeddedPointer
	}
	x.lhs(expr.X, indirect)
}

// When p points to a local wrapper, p.N follows TWO storage edges: p -> wrapper
// -> embedded pointee. The first edge never writes the wrapper. Unknown heap
// contents are not a proof of either local storage edge; p's own data channel
// still permits same-binding comparisons without inventing aliases.
func embeddedFieldPathMode(access, mode loopAccess, knownLocal bool) loopAccess {
	if access&loopEmbeddedPointer == 0 {
		return mode
	}
	if knownLocal && mode&loopRead != 0 {
		return mode&^(loopWrite|loopIndirectRead|loopIndirectWrite|loopEmbeddedPointer) |
			loopRead | access&(loopIndirectRead|loopIndirectWrite)
	}
	return mode & (loopRetain | loopPriorRef | loopEpochMixedRef)
}

// Binding a pointer method loads pointer fields along the promotion path, but
// never the final receiver's data. Header-only reads retain their physical-slot
// semantics; only crossing a pointer to obtain a field adds an indirect read.
func (x *loopIndex) methodReceiverPathOperand(expr *ast.SelectorExpr, hint loopReceiverSyntaxHint) {
	x.methodReceiverHeaderOperand(expr.X)
	base := expr.X
	for _, field := range hint.path {
		if field.indirect {
			// The pointee's FIELD HEADER is read, not the data addressed by
			// that field. Keep loopRead clear so expansion can stop there.
			x.lhs(base, loopHeaderRead|loopIndirectRead)
		}
		base = loopReceiverFieldExpr(base, field, expr.Sel.Pos())
	}
}

// An explicit p.Inner receiver needs the same physical field-header loads as
// the implicit path in p.Method. The ordinary field reader would also follow
// Inner's retained contents, which method binding must leave latent.
func (x *loopIndex) methodReceiverHeaderOperand(expr ast.Expr) {
	if field, ok := loopUnparen(expr).(*ast.SelectorExpr); ok {
		probe := &loopScanner{env: loopEnv{ctx: x.ctx}, index: x}
		if hint := probe.fieldReceiverHint(field); hint.field {
			x.methodReceiverPathOperand(field, hint)
			return
		}
	}
	x.headerOperand(expr)
}

// methodPathHeaderMode is an expansion hook for a physical header read through
// a pointer. The explicit address edge reaches the containing local's field
// slot, never that field's retained pointee. Ordinary data reads keep loopRead
// and therefore retain their existing expansion behavior. knownLocal must come
// from knownInvocationAlias, not from the ordinary retained-content summary.
func methodPathHeaderMode(access, mode loopAccess, knownLocal bool) loopAccess {
	if access&loopRead == 0 && access&(loopHeaderRead|loopIndirectRead) == loopHeaderRead|loopIndirectRead {
		if knownLocal && mode&loopRead != 0 {
			return loopHeaderRead | mode&(loopPriorRef|loopEpochMixedRef)
		}
		return mode & (loopRetain | loopPriorRef | loopEpochMixedRef)
	}
	return mode
}

// promotedReceiverValue is a value-selector hook. A method reached through an
// embedded pointer binds that pointer's value, not the wrapper's address. The
// only load here follows an explicit local address proof; field contents stay
// coarse, just as for the equivalent explicit p.Inner.Method selection.
func (s *loopScanner) promotedReceiverValue(expr *ast.SelectorExpr, w *loopWalk) (loopValue, bool) {
	hint := s.methodReceiverHint(expr)
	if !hint.known || !loopReceiverHasPointerField(hint.path) {
		return nil, false
	}
	return loopBindMethodReceiver(s.promotedPathValue(expr.X, w)), true
}

// promotedFieldAddressValue is the corresponding addressValue-selector hook.
// Exclude the last field: &s.Inner addresses a pointer FIELD in s, whereas
// &s.N crosses Inner to address storage beyond s.
func (s *loopScanner) promotedFieldAddressValue(expr *ast.SelectorExpr, w *loopWalk) (loopValue, bool) {
	hint := s.fieldReceiverHint(expr)
	if !hint.field || len(hint.path) < 2 || !loopReceiverHasPointerField(hint.path[:len(hint.path)-1]) {
		return nil, false
	}
	return s.promotedPathValue(expr.X, w), true
}

func loopReceiverHasPointerField(path []loopReceiverField) bool {
	for _, field := range path {
		if field.pointer {
			return true
		}
	}
	return false
}

// promotedFieldAddressRoots is the address-provenance adapter for flow-values.
// Call it before loopAddressRoot's syntactic-root fallback. A member address
// stays partial even if the member happens to have the root variable's type.
func (s *loopScanner) promotedFieldAddressRoots(expr ast.Expr, w *loopWalk) (loopValue, bool) {
	field, ok := loopUnparen(expr).(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	hint := s.fieldReceiverHint(field)
	if !hint.field || !hint.throughPointer {
		return nil, false
	}
	e := &loopFlowEffects{s: s, aliases: w.aliases}
	return loopPartialAddress(e.fieldPathTargets(field, hint, w)), true
}

// fieldPathTargets is the physical-access counterpart of promotedPathValue.
// Only a proven local container may supply pointer-field contents. A heap
// pointer's broad retained summary is not evidence that it addresses a local.
// Contents remain field-insensitive, like the scanner's existing value lattice.
func (e *loopFlowEffects) fieldPathTargets(expr *ast.SelectorExpr, hint loopReceiverSyntaxHint, w *loopWalk) loopValue {
	if !loopReceiverHasPointerField(hint.path[:len(hint.path)-1]) {
		if targets := e.addressAliases(expr.X, w); len(targets) != 0 {
			return targets
		}
		// Explicit s.Inner.N crosses the same pointer as promoted s.N.
		if field, ok := loopUnparen(expr.X).(*ast.SelectorExpr); ok {
			if h := e.s.fieldReceiverHint(field); h.field {
				return e.fieldContainerContents(field.X, w)
			}
		}
		return nil
	}
	return e.fieldContainerContents(expr.X, w)
}

func (e *loopFlowEffects) fieldContainerContents(base ast.Expr, w *loopWalk) loopValue {
	var value loopValue
	if e.s.pointerValue(base) {
		loaded, known := e.s.loadKnownPointee(base, w)
		if !known {
			return nil
		}
		value = loaded
	} else if e.s.root(base) != nil {
		value = e.s.value(base, w)
	} else {
		return nil
	}
	out := loopValue{}
	for v, mode := range value {
		if mode&loopRead != 0 {
			out[v] = mode
		}
	}
	return out
}

func (s *loopScanner) promotedPathValue(base ast.Expr, w *loopWalk) loopValue {
	if s.pointerValue(base) {
		if loaded, known := s.loadKnownPointee(base, w); known {
			return loaded
		}
	}
	return s.value(base, w)
}
