package diago

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeTypeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, source := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTestPackageTypes(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/testinfo\n\ngo 1.22\n",
		"p.go": `package p
var Shared int
type Resource struct{}
func (*Resource) Close() error { return nil }
func Acquire() *Resource { return new(Resource) }
`,
		"p_test.go": `package p
import "testing"
func TestResource(t *testing.T) { r := Acquire(); _ = r.Close(); t.Log(Shared) }
`,
		"external_test.go": `package p_test
import (
 "testing"
 "context"
 "example.com/testinfo"
)
func TestExternal(t *testing.T) { _ = p.Acquire(); t.Log(context.Background()) }
`,
	})
	pkgs, err := listPackages(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 {
		t.Fatalf("got %d packages, want 1", len(pkgs))
	}
	pkg := pkgs[0]
	if pkg.GoVersion != "1.22" || pkg.exports["testing"] == "" || pkg.exports["context"] == "" {
		t.Fatalf("missing language version or test-only export data: %+v", pkg)
	}
	if pkg.testExports["testing"] != "" {
		t.Fatal("test exports duplicated the shared base map")
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, name := range []string{"p.go", "p_test.go", "external_test.go"} {
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		parsed[name] = file
	}
	infos := checkTestTypes(pkg, fset, parsed)
	for _, name := range []string{"p_test.go", "external_test.go"} {
		info := infos[name]
		if info == nil {
			t.Fatalf("missing complete type information for %s", name)
		}
		found := map[string]bool{}
		ast.Inspect(parsed[name], func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && info.Uses[id] != nil {
				found[id.Name] = true
			}
			return true
		})
		for _, want := range []string{"Acquire", "Log", "T"} {
			if !found[want] {
				t.Errorf("%s: %s did not resolve", name, want)
			}
		}
		if name == "p_test.go" && (!found["Shared"] || !found["Close"]) {
			t.Errorf("internal tests cannot resolve production declarations: %v", found)
		}
		if name == "external_test.go" && !found["Background"] {
			t.Errorf("external tests cannot resolve test-only imports: %v", found)
		}
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Errorf("valid test packages should type-check: %+v", finding)
		}
	}
}

func TestSharedExportMaps(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/sharedexports\n\ngo 1.22\n",
		"a/a.go": "package a\nfunc A(){}\n",
		"b/b.go": "package b\nfunc B(){}\n",
	})
	pkgs, err := listPackages(dir, "./...")
	if err != nil || len(pkgs) != 2 {
		t.Fatalf("listing packages: %v, count=%d", err, len(pkgs))
	}
	pkgs[0].exports["test-only-sentinel"] = "shared"
	if pkgs[1].exports["test-only-sentinel"] != "shared" {
		t.Fatal("packages have separate copies of the base export map")
	}
	for _, pkg := range pkgs {
		if len(pkg.testExports) != 0 {
			t.Fatal("test-free package has test-variant overrides")
		}
	}
}

func TestExportTestTypes(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod":         "module example.com/testexports\n\ngo 1.22\n",
		"p.go":           "package p\ntype Value struct{}\nfunc private() int { return 1 }\n",
		"q/q.go":         "package q\nimport \"example.com/testexports\"\nfunc Get() p.Value { return p.Value{} }\n",
		"export_test.go": "package p\nvar ExportedForTest = private\n",
		"external_test.go": `package p_test
import (
 "testing"
 "example.com/testexports"
 "example.com/testexports/q"
)
func TestExternal(t *testing.T) {
 var v p.Value = q.Get()
 t.Log(p.ExportedForTest(), v)
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("external tests should see export_test.go declarations: %+v", finding)
		}
	}
}

func TestCgoPackageTypes(t *testing.T) {
	out, err := exec.Command("go", "env", "CGO_ENABLED").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "1" {
		t.Skip("cgo is disabled")
	}
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/cgotypes\n\ngo 1.22\n",
		"cgo.go": "package p\nimport \"C\"\ntype cgoThing struct{}\n",
		"p.go": `package p
import "net/http"
var _ cgoThing
func Fetch(url string) error {
 resp, err := http.Get(url)
 if err != nil { return err }
 defer resp.Body.Close()
 return nil
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := countRules(findings)
	if counts["type-info-unavailable"] != 0 || counts["missing-context-param"] != 1 {
		t.Fatalf("cgo declarations should preserve regular-file checks: %+v", findings)
	}
}

func TestTargetTypeSizes(t *testing.T) {
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "386")
	t.Setenv("CGO_ENABLED", "0")
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/targetsize\n\ngo 1.22\n",
		"p.go":   "package p\nimport \"unsafe\"\nvar _ [4 - unsafe.Sizeof(uintptr(0))]byte\n",
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			t.Fatalf("valid target-sized declaration failed to type-check: %+v", finding)
		}
	}
}

func TestInternalTestCapture(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod": "module example.com/testcapture\n\ngo 1.22\n",
		"p.go":   "package p\nvar shared int\n",
		"p_test.go": `package p
func launch(xs []int) {
 n := len(xs)
 var k int
 for k, shared = range xs {
  go func() { _ = shared; _ = n }()
 }
 _ = k
}
`,
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	counts := countRules(findings)
	if counts["type-info-unavailable"] != 0 || counts["goroutine-in-loop"] != 1 {
		t.Fatalf("expected a capture with complete internal-test types: %+v", findings)
	}
}

func TestPartialTypesRejected(t *testing.T) {
	fset, files := parseTypeCheckFixture(t, "package p\nfunc F() { _ = missing }\n")
	conf := types.Config{Error: func(error) {}}
	if detail := tryTypeCheck(&conf, "example.com/p", fset, files, newTypesInfo()); detail == nil {
		t.Fatal("type error was ignored")
	}
	pkg := goListPackage{ImportPath: "example.com/p", Name: "p", GoFiles: []string{"p.go"}}
	if info := checkPackageTypes(pkg, fset, map[string]*ast.File{"p.go": files[0]}); info != nil {
		t.Fatal("partial type information was returned as complete")
	}
}

func TestTestTypeFailureWarning(t *testing.T) {
	dir := writeTypeModule(t, map[string]string{
		"go.mod":    "module example.com/typefailure\n\ngo 1.22\n",
		"p.go":      "package p\nfunc F() {}\n",
		"p_test.go": "package p\nfunc testHelper() { F(); missing() }\n",
	})
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	var warnings []ASTFinding
	for _, finding := range findings {
		if finding.Rule == "type-info-unavailable" {
			warnings = append(warnings, finding)
		}
	}
	if len(warnings) != 1 || filepath.Base(warnings[0].File) != "p_test.go" {
		t.Fatalf("expected one warning for failed test type check: %+v", warnings)
	}
}

func TestSourceGoVersion(t *testing.T) {
	for _, tc := range []struct{ module, file, want string }{
		{"1.22", "", "1.22"},
		{"1.22", "go1.21", "go1.21"},
		{"1.21", "go1.22", "go1.22"},
		{"1.22", "go1.20", "1.21"},
	} {
		ctx := astContext{pkg: goListPackage{GoVersion: tc.module}, file: &ast.File{GoVersion: tc.file}}
		if got := sourceGoVersion(ctx); got != tc.want {
			t.Errorf("module=%s file=%s: got %s, want %s", tc.module, tc.file, got, tc.want)
		}
	}
}

func TestLoopGOPATHVersion(t *testing.T) {
	t.Setenv("GO111MODULE", "off")
	dir := writeTypeModule(t, map[string]string{
		"p.go": "package p\nfunc work(){ for i:=0;i<3;i++{go func(){_=i}()} }\n",
	})
	pkgs, err := listPackages(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || !goVersionAtLeast(pkgs[0].GoVersion, "1.22") {
		t.Fatalf("missing toolchain language version for GOPATH package: %+v", pkgs)
	}
	findings, err := AnalyzeAST(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if counts := countRules(findings); counts["goroutine-in-loop"] != 0 || counts["type-info-unavailable"] != 0 {
		t.Fatalf("GOPATH packages should use fresh iteration semantics: %+v", findings)
	}
}

func TestLoopModuleVersion(t *testing.T) {
	for _, tc := range []struct {
		module, tag string
		want        int
	}{
		{"1.21", "", 1},
		{"1.22", "", 0},
		{"1.22", "//go:build go1.21\n\n", 1},
		{"1.21", "//go:build go1.22\n\n", 0},
	} {
		t.Run(tc.module+"/"+tc.tag, func(t *testing.T) {
			dir := writeTypeModule(t, map[string]string{
				"go.mod": "module example.com/loopversion\n\ngo " + tc.module + "\n",
				"p.go": tc.tag + `package p
func work() {
 for i := 0; i < 10; i++ { go func() { _ = i }() }
}
`,
			})
			findings, err := AnalyzeAST(dir, "./...")
			if err != nil {
				t.Fatal(err)
			}
			if got := countRules(findings)["goroutine-in-loop"]; got != tc.want {
				t.Fatalf("got %d loop findings, want %d: %+v", got, tc.want, findings)
			}
		})
	}
}
