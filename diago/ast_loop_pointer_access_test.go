package diago

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"reflect"
	"strings"
	"testing"
)

var loopPointerAccessCases = []struct {
	name, body string
	want       int
}{
	{"heap_callback_field_store", `for range xs {v:=0; p:=&struct{F func(); N int}{F:func(){v++}}; go func(){use(v)}(); p.N=1}`, 0},
	{"heap_pointer_field_store", `for range xs {v:=0; p:=&struct{P *int; N int}{P:&v}; go func(){use(v)}(); p.N=1}`, 0},
	{"heap_callback_increment", `for range xs {v:=0; p:=&struct{F func(); N int}{F:func(){v++}}; go func(){use(v)}(); p.N++}`, 0},
	{"heap_callback_field_read", `for range xs {v:=0; p:=&struct{F func(); N int}{F:func(){v++}}; go func(){use(v)}(); use(p.N)}`, 0},
	{"heap_pointer_field_read", `for range xs {v:=0; p:=&struct{P *int; N int}{P:&v}; go func(){v=1}(); use(p.N)}`, 0},
	{"heap_callback_whole_store", `for range xs {v:=0; p:=&struct{F func()}{F:func(){v++}}; go func(){use(v)}(); *p=struct{F func()}{}}`, 0},
	{"heap_callback_array_store", `for range xs {v:=0; p:=&[1]func(){func(){v++}}; go func(){use(v)}(); p[0]=func(){}}`, 0},
	{"addressed_callback_struct_store", `for range xs {v:=0; s:=struct{F func(); N int}{F:func(){v++}}; p:=&s; go func(){use(v)}(); p.N=1}`, 0},
	{"addressed_pointer_struct_store", `for range xs {v:=0; s:=struct{P *int; N int}{P:&v}; p:=&s; go func(){use(v)}(); p.N=1}`, 0},
	{"explicit_scalar_store", `for range xs {v:=0; p:=&v; go func(){use(v)}(); *p=1}`, 1},
	{"explicit_scalar_increment", `for range xs {v:=0; p:=&v; go func(){use(v)}(); (*p)++}`, 1},
	{"explicit_scalar_read", `for range xs {v:=0; p:=&v; go func(){v=1}(); use(*p)}`, 1},
	{"explicit_field_read", `for range xs {v:=item{}; p:=&v; go func(){v.N=1}(); use(p.N)}`, 1},
	{"explicit_nested_field_read", `for range xs {v:=struct{S item}{}; p:=&v; go func(){v.S.N=1}(); use(p.S.N)}`, 1},
	{"explicit_array_read", `for range xs {v:=[1]int{}; p:=&v; go func(){v[0]=1}(); use(p[0])}`, 1},
	{"explicit_array_field_read", `for range xs {v:=[1]item{}; p:=&v; go func(){v[0].N=1}(); use(p[0].N)}`, 1},
	{"explicit_field_store", `for range xs {v:=item{}; p:=&v; go func(){use(v.N)}(); p.N=1}`, 1},
	{"explicit_array_store", `for range xs {v:=[1]int{}; p:=&v; go func(){use(v[0])}(); p[0]=1}`, 1},
	{"immediate_address_read", `for range xs {v:=0; go func(){v=1}(); use(*(&v))}`, 1},
	{"converted_address_read", `for range xs {v:=0; p:=(*int)(&v); go func(){v=1}(); use(*p)}`, 1},
	{"copied_address_read", `for range xs {v:=0; p:=&v; q:=p; go func(){v=1}(); use(*q)}`, 1},
	{"field_address_read", `for range xs {v:=item{}; p:=&v.N; go func(){v.N=1}(); use(*p)}`, 1},
	{"array_element_address_read", `for range xs {v:=[1]int{}; p:=&v[0]; go func(){v[0]=1}(); use(*p)}`, 1},
	{"pointer_copy_is_not_pointee_read", `for range xs {v:=0; p:=&v; go func(){v=1}(); _=p}`, 0},
	{"overwritten_address_read", `for range xs {v,u:=0,0; p:=&v; p=&u; go func(){v=1}(); use(*p)}`, 0},
	{"joined_address_read", `for _,n:=range xs {v,u:=0,0; p:=&u; if n==0 {p=&v}; go func(){v=1}(); use(*p)}`, 1},
	{"goto_skips_address_overwrite", `for range xs {v,u:=0,0; p:=&v; goto load; p=&u; load: go func(){v=1}(); use(*p)}`, 1},
	{"unreachable_address_read", `for range xs {v:=0; p:=&v; go func(){v=1}(); break; use(*p)}`, 0},
	{"read_before_launch", `for range xs {v:=0; p:=&v; use(*p); go func(){v=1}()}`, 0},
	{"read_only_pair", `for range xs {v:=0; p:=&v; go func(){use(v)}(); use(*p)}`, 0},
	{"address_is_not_read", `for range xs {v:=0; go func(){v=1}(); _=&v}`, 0},
	{"field_address_is_not_read", `for range xs {v:=item{}; p:=&v; go func(){v.N=1}(); _=&p.N}`, 0},
	{"array_address_is_not_read", `for range xs {v:=[1]int{}; p:=&v; go func(){v[0]=1}(); _=&p[0]}`, 0},
	{"dereference_address_is_not_read", `for range xs {v:=0; p:=&v; go func(){v=1}(); _=&*p}`, 0},
	{"address_loads_pointer_slot", `for range xs {v:=item{}; p:=&v; pp:=&p; go func(){p=nil}(); _=&(*pp).N}`, 1},
	{"address_does_not_load_final_pointee", `for range xs {v:=item{}; p:=&v; pp:=&p; go func(){v.N=1}(); _=&(*pp).N}`, 0},
	{"address_loads_index", `for range xs {i:=0; p:=&i; a:=[1]int{}; go func(){i=1}(); _=&a[*p]}`, 1},
	{"slice_element_store_not_header_write", `for range xs {v:=[]int{0}; p:=&v; go func(){use(v)}(); (*p)[0]=1}`, 0},
	{"slice_element_address_loads_header", `for range xs {v:=[]int{0}; p:=&v; go func(){v=nil}(); _=&(*p)[0]}`, 1},
	{"function_slot_read", `for range xs {f:=func(){}; p:=&f; go func(){f=func(){}}(); _=*p}`, 1},
	{"function_slot_call_load", `for range xs {f:=func(){}; p:=&f; go func(){f=func(){}}(); (*p)()}`, 1},
	{"function_slot_go_load", `for range xs {f:=func(){}; p:=&f; go func(){f=func(){}}(); go (*p)()}`, 1},
	{"function_slot_defer_load", `for range xs {f:=func(){}; p:=&f; go func(){f=func(){}}(); defer (*p)()}`, 1},
	{"function_field_call_load", `for range xs {v:=struct{F func()}{F:func(){}}; p:=&v; go func(){v.F=func(){}}(); p.F()}`, 1},
	{"function_array_call_load", `for range xs {v:=[1]func(){func(){}}; p:=&v; go func(){v[0]=func(){}}(); p[0]()}`, 1},
	{"stored_function_body_not_read", `for range xs {v:=0; f:=func(){v++}; p:=&f; go func(){use(v)}(); _=*p}`, 0},
	// The saved function executes at function exit, not registration; its write races.
	{"stored_function_body_not_invoked_by_defer", `for range xs {v:=0; f:=func(){v++}; p:=&f; go func(){use(v)}(); defer (*p)()}`, 1},
	{"function_body_invoked_separately", `for range xs {v:=0; f:=func(){v++}; p:=&f; go func(){use(v)}(); (*p)()}`, 1},
	{"go_argument_load_is_synchronous", `for range xs {v:=0; p:=&v; go func(){v=1}(); go use(*p)}`, 1},
	{"defer_argument_load_is_synchronous", `for range xs {v:=0; p:=&v; go func(){v=1}(); defer use(*p)}`, 1},
	{"pointer_method_binding_not_data_read", `for range xs {v:=item{}; p:=&v; go func(){v.N=1}(); _=p.Use}`, 0},
}

func TestLoopPointerAccess(t *testing.T) {
	for _, tc := range loopPointerAccessCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"typed", "nil", "partial", "lexical", "partial-lexical"} {
				t.Run(mode, func(t *testing.T) {
					got := analyzeLoopRegression(t, loopTestPrelude+"func work(xs []int){"+tc.body+"}", "go1.22", mode)
					if n := countRules(got)["goroutine-in-loop"]; n != tc.want {
						t.Fatalf("got %d goroutine findings, want %d: %+v", n, tc.want, got)
					}
				})
			}
		})
	}
}

// Exercise the production loader and require actual typed analysis, rather than
// accepting a silent fallback to syntax-only scanning.
func TestLoopPointerAccessAnalyzeAST(t *testing.T) {
	var source strings.Builder
	source.WriteString(loopTestPrelude)
	for i, tc := range loopPointerAccessCases {
		fmt.Fprintf(&source, "\nfunc pointerAccess%d(xs []int){%s}\n", i, tc.body)
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod":    "module example.com/pointeraccess\n\ngo 1.22\n",
		"access.go": source.String(),
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if countRules(findings)["type-info-unavailable"] != 0 {
		t.Fatalf("production scan must have complete type information: %+v", findings)
	}
	got := map[string]int{}
	for _, finding := range findings {
		if finding.Rule == "goroutine-in-loop" {
			got[finding.Symbol]++
		}
	}
	for i, tc := range loopPointerAccessCases {
		if n := got[fmt.Sprintf("pointerAccess%d", i)]; n != tc.want {
			t.Errorf("%s: got %d goroutine findings, want %d", tc.name, n, tc.want)
		}
	}
}

// The recorder consumes private address proofs without changing them. A broad
// retained callback entry is intentionally present beside the true alias slot.
func TestLoopPointerAccessRecorder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "access.go", `package sample
func work(){var p *int; use(*p); *p=1}`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	body := file.Decls[0].(*ast.FuncDecl).Body
	x := &loopIndex{
		ctx: loopContext{fset: fset}, ids: map[*ast.Ident]*loopVariable{},
		typed: map[types.Object]*loopVariable{}, parsed: map[*ast.Object]*loopVariable{},
		external: map[string]*loopVariable{}, regions: map[ast.Node]*loopRegion{},
		captures: map[*ast.FuncLit]map[*loopVariable]loopAccess{},
	}
	x.pushScope()
	x.node(body)
	p := x.ids[body.List[0].(*ast.DeclStmt).Decl.(*ast.GenDecl).Specs[0].(*ast.ValueSpec).Names[0]]
	target, callbackCapture, slot := &loopVariable{}, &loopVariable{}, &loopVariable{}
	aliases := map[*loopVariable]*loopVariable{p: slot}
	w := &loopWalk{values: map[*loopVariable]loopValue{
		p:    {callbackCapture: loopWrite | loopInvoke},
		slot: {target: loopRead | loopPriorRef},
	}}
	before := loopCloneValues(w.values)
	s := &loopScanner{index: x}
	e := s.beginFlowEffects()
	e.aliases = aliases
	e.recordFlowEffects(body.List[1], w)
	e.recordFlowEffects(body.List[2], w)
	e.recordFlowEffects(body.List[1], w)
	e.finishFlowEffects()
	var reads, writes int
	for _, access := range x.direct {
		if !access.effect {
			continue
		}
		if access.variable != target || access.mode&loopPriorRef == 0 {
			t.Fatalf("unexpected pointee or lost reference provenance: %+v", access)
		}
		if access.mode&loopRead != 0 {
			reads++
		}
		if access.mode&loopWrite != 0 {
			writes++
		}
	}
	if reads != 1 || writes != 1 {
		t.Fatalf("got %d pointee reads and %d writes, want one each", reads, writes)
	}
	if !reflect.DeepEqual(before, w.values) || len(aliases) != 1 {
		t.Fatal("effect replay mutated bindings or address metadata")
	}
}
