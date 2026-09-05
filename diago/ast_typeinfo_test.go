package diago

import (
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

type panicImporter struct {
	payload any
}

func (p panicImporter) Import(path string) (*types.Package, error) {
	panic(p.payload)
}

func parseTypeCheckFixture(t *testing.T, src string) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return fset, []*ast.File{file}
}

func newTypeInfo() *types.Info {
	return &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
}

func TestTryTypeCheckRecoversImporterSkew(t *testing.T) {
	skew := `cannot decode "context", export data version 4 is greater than maximum supported version 2`
	payloads := []any{skew, errors.New(skew)}
	for _, payload := range payloads {
		fset, files := parseTypeCheckFixture(t, "package p\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n")
		conf := &types.Config{Importer: panicImporter{payload: payload}}
		got := tryTypeCheck(conf, "example.com/p", fset, files, newTypeInfo())
		if got == nil {
			t.Fatalf("tryTypeCheck(%T) returned nil, want skew detail", payload)
		}
	}
}

func TestTryTypeCheckRepanicsUnknownPanic(t *testing.T) {
	fset, files := parseTypeCheckFixture(t, "package p\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n")
	conf := &types.Config{Importer: panicImporter{payload: "boom"}}
	defer func() {
		r := recover()
		if r != "boom" {
			t.Fatalf("got recovered=%v, want re-panicked boom", r)
		}
	}()
	tryTypeCheck(conf, "example.com/p", fset, files, newTypeInfo())
	t.Fatal("tryTypeCheck did not re-panic")
}

func TestTryTypeCheckCleanCheckReturnsNil(t *testing.T) {
	fset, files := parseTypeCheckFixture(t, "package p\n\nfunc F() int { return 1 }\n")
	conf := &types.Config{Importer: importer.Default()}
	if got := tryTypeCheck(conf, "example.com/p", fset, files, newTypeInfo()); got != nil {
		t.Fatalf("tryTypeCheck clean run returned %v, want nil", got)
	}
}

func TestCheckPackageTypesEmptyPackage(t *testing.T) {
	info := checkPackageTypes(goListPackage{}, token.NewFileSet(), map[string]*ast.File{})
	if info == nil {
		t.Fatal("checkPackageTypes with no files returned nil, want empty info")
	}
}

func TestNilTypeInfoGuards(t *testing.T) {
	t.Run("parameterIndexesInNode", func(t *testing.T) {
		_, files := parseTypeCheckFixture(t, "package p\n\nfunc f() { x := 1; _ = x }\n")
		var body *ast.BlockStmt
		ast.Inspect(files[0], func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				body = fn.Body
			}
			return body == nil
		})
		if got := parameterIndexesInNode(body, map[types.Object]int{}, nil); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
	t.Run("directlyCalledParameters", func(t *testing.T) {
		call := &ast.CallExpr{Fun: &ast.Ident{Name: "g"}}
		if got := directlyCalledParameters(call, map[types.Object]int{}, nil); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
	t.Run("isIteratorErrCall", func(t *testing.T) {
		call := &ast.CallExpr{Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "rows"},
			Sel: &ast.Ident{Name: "Err"},
		}}
		if isIteratorErrCall(astContext{}, call, iteratorLoop{}) {
			t.Fatal("isIteratorErrCall with nil types returned true, want false")
		}
	})
}
