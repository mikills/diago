package diago

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoredCallSeverity(t *testing.T) {
	t.Run("cleanup calls are low severity", func(t *testing.T) {
		findings := ignoredCallFindings(t, `package sample

import "os"

func cleanup(f *os.File, timer interface{ Stop() bool }) {
	_ = f.Close()
	_ = os.Remove("tmp")
	_ = timer.Stop()
}
`)
		if len(findings) != 3 {
			t.Fatalf("got %d findings, want 3", len(findings))
		}
		for _, finding := range findings {
			if finding.Severity != "low" {
				t.Fatalf("%s severity = %s, want low", finding.Message, finding.Severity)
			}
		}
	})

	t.Run("non cleanup calls stay medium severity", func(t *testing.T) {
		findings := ignoredCallFindings(t, `package sample

func save() error { return nil }
func run() { _ = save() }
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Severity != "medium" {
			t.Fatalf("severity = %s, want medium", findings[0].Severity)
		}
	})
}

func ignoredCallFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample.go"}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findErrorHandlingSignals(&findings, ctx, fn, fn.Name.Name)
	}
	return findings
}

func TestCollectLiteralSignals(t *testing.T) {
	t.Run("skips imports and struct tags", func(t *testing.T) {
		file := parseLiteralTestFile(t, `package sample

import (
	"context"
	"errors"
	"log/slog"
	"github.com/mikills/minnow/kb"
)

type record struct {
	ID string `+"`json:\"kb_id\"`"+`
}

const a = "real duplicate"
const b = "real duplicate"
const c = "real duplicate"
const d = "real duplicate"
const e = "real duplicate"
const f = "real duplicate"
`)
		signals := newPackageSignals(goListPackage{})
		collectLiteralSignals(signals, astContext{fset: token.NewFileSet(), path: "sample.go"}, file)

		for _, literal := range []string{"context", "errors", "log/slog", "github.com/mikills/minnow/kb", `json:"kb_id"`} {
			if got := len(signals.strings[literal]); got != 0 {
				t.Fatalf("literal %q tracked %d times, want 0", literal, got)
			}
		}
		if got := len(signals.strings["real duplicate"]); got != 6 {
			t.Fatalf("real duplicate tracked %d times, want 6", got)
		}
	})
}

func parseLiteralTestFile(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLongTestNameFinding(t *testing.T) {
	t.Run("flags long test names", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestParseEscapeOutputValidOutputWithMultipleHeapEscapes(t *testing.T) {}
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Rule != "long-test-name" || findings[0].Severity != "low" {
			t.Fatalf("unexpected finding: %#v", findings[0])
		}
	})

	t.Run("ignores short test names", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestParseEscapeOutput(t *testing.T) {}
`)
		if len(findings) != 0 {
			t.Fatalf("got %d findings, want 0", len(findings))
		}
	})

	t.Run("matches the requested rg threshold", func(t *testing.T) {
		findings := longTestNameFindings(t, `package sample

import "testing"

func TestABCDEFGHIJKLMNOPQRSTUVWXY1234567(t *testing.T) {}
func TestABCDEFGHIJKLMNOPQRSTUVWXY123456(t *testing.T) {}
`)
		if len(findings) != 1 {
			t.Fatalf("got %d findings, want 1", len(findings))
		}
		if findings[0].Symbol != "TestABCDEFGHIJKLMNOPQRSTUVWXY1234567" {
			t.Fatalf("symbol = %q", findings[0].Symbol)
		}
	})
}

func longTestNameFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample_test.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample_test.go", isTest: true}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		analyzeFunc(&findings, ctx, fn)
	}
	return findings
}

func resourceCloseFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Types: make(map[ast.Expr]types.TypeAndValue)}
	conf := types.Config{Importer: importer.Default()}
	if _, err := conf.Check("example.com/sample", fset, []*ast.File{file}, info); err != nil {
		t.Fatal(err)
	}
	ctx := astContext{
		pkg: goListPackage{
			ImportPath:  "example.com/sample",
			ownedFields: collectLifecycleOwnedFieldsFromFiles("example.com/sample", map[string]*ast.File{"sample.go": file}),
		},
		fset: fset, path: "sample.go", types: info,
		releases: collectReleaseParams(map[string]*ast.File{"sample.go": file}, []string{"sample.go"}),
	}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findResourceCloseSignals(&findings, ctx, fn, fn.Name.Name)
	}
	return findings
}

func TestResourceCloseSignals(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   int
	}{
		{
			name: "defer close direct",
			source: `package sample
import "os"
func read(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return nil
}`,
			want: 0,
		},
		{
			name: "returned ownership is not a leak",
			source: `package sample
import "os"
func openThing(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}`,
			want: 0,
		},
		{
			name: "returned wrapped ownership is not a leak",
			source: `package sample
import "os"
type holder struct{ f *os.File }
func openWrapped(p string) (*holder, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &holder{f: f}, nil
}`,
			want: 0,
		},
		{
			name: "http body close resolves the response",
			source: `package sample
import ("io"; "net/http")
func fetch(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}`,
			want: 0,
		},
		{
			name: "genuine leak still reported",
			source: `package sample
import "os"
func leak(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	_ = f
	return nil
}`,
			want: 1,
		},
		{
			name: "sql Rows leak is reported",
			source: `package sample
import "database/sql"
func leak(db *sql.DB) error {
	rows, err := db.Query("select 1")
	if err != nil { return err }
	_ = rows
	return nil
}`,
			want: 1,
		},
		{
			name: "http Response leak from client is reported",
			source: `package sample
import "net/http"
func leak(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil { return err }
	_ = resp
	return nil
}`,
			want: 1,
		},
		{
			name: "custom closer factory name does not hide leak",
			source: `package sample
import "io"
func acquire() (io.Closer, error) { return nil, nil }
func leak() error {
	resource, err := acquire()
	if err != nil { return err }
	_ = resource
	return nil
}`,
			want: 1,
		},
		{
			name: "non io Closer Close method is not a resource",
			source: `package sample
type Dialog struct{}
func (*Dialog) Close() {}
func NewDialog() *Dialog { return &Dialog{} }
func build() { dialog := NewDialog(); _ = dialog }`,
			want: 0,
		},
		{
			name: "URL query values are not resources",
			source: `package sample
import "net/http"
func values(r *http.Request) string {
	query := r.URL.Query()
	return query.Get("key")
}`,
			want: 0,
		},
		{
			name: "ordinary Do and Get results are not resources",
			source: `package sample
type Group struct{}
func (Group) Do(string, func() (any, error)) (any, error, bool) { return nil, nil, false }
type Cache struct{}
func (Cache) Get(string) (string, bool) { return "", false }
func load(group Group, cache Cache) string {
	result, _, _ := group.Do("key", func() (any, error) { return "value", nil })
	value, _ := cache.Get("key")
	_ = result
	return value
}`,
			want: 0,
		},
		{
			name: "typed closable Query result is reported",
			source: `package sample
type Rows struct{}
func (*Rows) Close() error { return nil }
type DB struct{}
func (DB) Query(string) (*Rows, error) { return nil, nil }
func leak(db DB) error {
	rows, err := db.Query("select 1")
	if err != nil { return err }
	_ = rows
	return nil
}`,
			want: 1,
		},
		{
			name: "one returned one leaked",
			source: `package sample
import "os"
func mixed(p, q string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	g, err := os.Open(q)
	if err != nil {
		return nil, err
	}
	_ = g
	return f, nil
}`,
			want: 1,
		},
		{
			name: "resource assigned to returned owner field",
			source: `package sample
import "io"
type Owner struct{ resource io.Closer }
func acquire() (io.Closer, error) { return nil, nil }
func build() (*Owner, error) {
	resource, err := acquire()
	if err != nil { return nil, err }
	owner := &Owner{}
	owner.resource = resource
	return owner, nil
}`,
			want: 0,
		},
		{
			name: "resource embedded in returned struct variable",
			source: `package sample
import "io"
type Owner struct{ resource io.Closer }
func acquire() (io.Closer, error) { return nil, nil }
func build() (*Owner, error) {
	resource, err := acquire()
	if err != nil { return nil, err }
	owner := &Owner{resource: resource}
	return owner, nil
}`,
			want: 0,
		},
		{
			name: "resource assigned to parameter owner field",
			source: `package sample
import "io"
type Owner struct{ resource io.Closer }
func acquire() (io.Closer, error) { return nil, nil }
func install(owner *Owner) error {
	resource, err := acquire()
	if err != nil { return err }
	owner.resource = resource
	return nil
}`,
			want: 0,
		},
		{
			name: "interface backend transferred to lifecycle owner",
			source: `package sample
type Backend interface{ Close() error }
type backend struct{}
func (*backend) Close() error { return nil }
func acquire() (Backend, error) { return &backend{}, nil }
type Service struct{ backend Backend }
func NewService(backend Backend) *Service { return &Service{backend: backend} }
func (s *Service) Close() error { return s.backend.Close() }
type Lifecycle struct{}
func (*Lifecycle) OnShutdown(func()) {}
func install(lifecycle *Lifecycle) error {
	backend, err := acquire()
	if err != nil { return err }
	service := NewService(backend)
	lifecycle.OnShutdown(func() { _ = service.Close() })
	return nil
}`,
			want: 0,
		},
		{
			name: "Shutdown releases resource",
			source: `package sample
import "context"
type Server struct{}
func NewServer() *Server { return &Server{} }
func (*Server) Close() error { return nil }
func (*Server) Shutdown(context.Context) error { return nil }
func run(ctx context.Context) { server := NewServer(); _ = server.Shutdown(ctx) }`,
			want: 0,
		},
		{
			name: "borrowed concrete file remains caller owned",
			source: `package sample
import "os"
type Wrapper struct{ file *os.File }
func NewWrapper(file *os.File) *Wrapper { return &Wrapper{file: file} }
func (*Wrapper) Close() error { return nil }
func leak() {
	file, _ := os.Open("file")
	wrapper := NewWrapper(file)
	_ = wrapper.Close()
}`,
			want: 1,
		},
		{
			name: "composite field name does not transfer same named resource",
			source: `package sample
import "io"
type Owner struct{ resource io.Closer }
func acquire() (io.Closer, error) { return nil, nil }
func leak() (*Owner, error) {
	resource, err := acquire()
	if err != nil { return nil, err }
	owner := &Owner{resource: nil}
	_ = resource
	return owner, nil
}`,
			want: 1,
		},
		{
			name: "resource mentioned in returned call is not transferred",
			source: `package sample
import ("fmt"; "io")
func acquire() (io.Closer, error) { return nil, nil }
func leak() error {
	resource, err := acquire()
	if err != nil { return err }
	return fmt.Errorf("resource: %v", resource)
}`,
			want: 1,
		},
		{
			name: "resource reassigned to parameter remains owned locally",
			source: `package sample
import "io"
func acquire() (io.Closer, error) { return nil, nil }
func leak(resource io.Closer) {
	resource, _ = acquire()
	_ = resource
}`,
			want: 1,
		},
		{
			name: "local cleanup helper releases resource",
			source: `package sample
import "io"
func acquire() (io.Closer, error) { return nil, nil }
func release(resource io.Closer) { _ = resource.Close() }
func use() {
	resource, _ := acquire()
	defer release(resource)
}`,
			want: 0,
		},
		{
			name: "borrowed interface field remains caller owned",
			source: `package sample
import "io"
type Wrapper struct{ resource io.Closer }
func NewWrapper(resource io.Closer) *Wrapper { return &Wrapper{resource: resource} }
func (*Wrapper) Close() error { return nil }
func acquire() (io.Closer, error) { return nil, nil }
func leak() {
	resource, _ := acquire()
	wrapper := NewWrapper(resource)
	_ = wrapper.Close()
}`,
			want: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := resourceCloseFindings(t, tc.source)
			if len(findings) != tc.want {
				t.Fatalf("got %d resource-not-closed findings, want %d: %+v", len(findings), tc.want, findings)
			}
		})
	}
}

func TestAnalyzeASTResourceTypes(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module example.com/resources\n\ngo 1.22\n",
		"resources.go": `package resources
import (
	"database/sql"
	"io"
	"net/http"
	"os"
)
func acquire() (io.Closer, error) { return nil, nil }
func leakFile() { resource, _ := os.Open("file"); _ = resource }
func leakRows(db *sql.DB) { resource, _ := db.Query("select 1"); _ = resource }
func leakResponse(client *http.Client, req *http.Request) { resource, _ := client.Do(req); _ = resource }
func leakCustom() { resource, _ := acquire(); _ = resource }
func closeCustom() { resource, _ := acquire(); _ = resource.Close() }
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	findings, err := AnalyzeAST(dir, ".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, finding := range findings {
		if finding.Rule == "resource-not-closed" {
			got[finding.Symbol] = true
		}
	}
	for _, symbol := range []string{"leakFile", "leakRows", "leakResponse", "leakCustom"} {
		if !got[symbol] {
			t.Errorf("missing resource finding for %s: %+v", symbol, findings)
		}
	}
	if got["closeCustom"] {
		t.Errorf("closed custom resource was reported: %+v", findings)
	}
}

func TestGeneratedExportsDoNotCountAsHandWrittenSurface(t *testing.T) {
	generated := parseLiteralTestFile(t, `package sample
type GeneratedOne struct{}
func GeneratedTwo() {}`)
	handWritten := parseLiteralTestFile(t, `package sample
type PublicOne struct{}
func PublicTwo() {}`)
	signals := newPackageSignals(goListPackage{})
	ctx := astContext{generated: true, fset: token.NewFileSet()}
	var sink []ASTFinding
	analyzeExtraFile(&sink, signals, ctx, generated)
	ctx.generated = false
	analyzeExtraFile(&sink, signals, ctx, handWritten)
	if signals.exported != 2 {
		t.Fatalf("exported surface = %d, want 2 hand-written declarations", signals.exported)
	}

	generatedOnly := newPackageSignals(goListPackage{ImportPath: "example.com/generated"})
	var findings []ASTFinding
	appendPackageSignalFindings(&findings, goListPackage{ImportPath: "example.com/generated"}, generatedOnly)
	for _, finding := range findings {
		if finding.Rule == "untested-exported-surface" {
			t.Fatalf("generated-only package produced finding: %+v", finding)
		}
	}
}

func TestPanicOutsideMainIntent(t *testing.T) {
	findings := dangerousCallFindings(t, `package sample
func MustBuild() int { panic("invalid invariant") }
func MustBuildCallback() func() { return func() { panic("callback failure") } }
func MustServe() error { panic("returns an error") }
func WithRollback() {
	defer func() {
		if recovered := recover(); recovered != nil {
			rollback()
			panic(recovered)
		}
	}()
}
func NestedRecoverIsNotRepanic() {
	var recovered any
	defer func() {
		func() { recovered = recover() }()
		panic(recovered)
	}()
}
func ConditionalRecoverIsNotRepanic(condition bool) {
	var recovered any
	defer func() {
		if condition { recovered = recover() }
		panic(recovered)
	}()
}
func ordinary() { panic("unexpected") }
func MustCrash() { panic("still unexpected") }
func Mustard() { panic("still unexpected") }
func rollback() {}
`)
	want := map[string]bool{
		"ConditionalRecoverIsNotRepanic": true,
		"MustBuildCallback":              true,
		"MustServe":                      true,
		"NestedRecoverIsNotRepanic":      true,
		"ordinary":                       true,
		"MustCrash":                      true,
		"Mustard":                        true,
	}
	if len(findings) != len(want) {
		t.Fatalf("got %d panic findings, want %d: %+v", len(findings), len(want), findings)
	}
	for _, finding := range findings {
		if !want[finding.Symbol] {
			t.Fatalf("unexpected panic finding: %+v", finding)
		}
	}
}

func dangerousCallFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{pkg: goListPackage{ImportPath: "example.com/sample", Name: "sample"}, fset: fset, path: "sample.go"}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			findDangerousCalls(&findings, ctx, fn, fn.Name.Name)
		}
	}
	return findings
}

func TestSwallowedErrorRequiresExplicitRuleDirective(t *testing.T) {
	source := `package sample
func lookup() error { return nil }
func unsuppressed() error {
	err := lookup()
	if err != nil { return nil }
	return nil
}
func proseOnly() error {
	err := lookup()
	// Best effort lookup.
	if err != nil { return nil }
	return nil
}
func suppressed() error {
	err := lookup()
	//diago:ignore swallowed-error optional lookup failure
	if err != nil { return nil }
	return nil
}
func wrongRule() error {
	err := lookup()
	//diago:ignore ignored-call-result
	if err != nil { return nil }
	return nil
}
func detached() error {
	err := lookup()
	//diago:ignore swallowed-error

	if err != nil { return nil }
	return nil
}`
	findings := errorHandlingFindings(t, source)
	var swallowed []ASTFinding
	for _, finding := range findings {
		if finding.Rule == "swallowed-error" {
			swallowed = append(swallowed, finding)
		}
	}
	if len(swallowed) != 4 {
		t.Fatalf("got %d swallowed-error findings, want 4 unsuppressed branches: %+v", len(swallowed), swallowed)
	}
}

func errorHandlingFindings(t *testing.T, source string) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	ctx := astContext{fset: fset, path: "sample.go", file: file}
	var findings []ASTFinding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			findErrorHandlingSignals(&findings, ctx, fn, fn.Name.Name)
		}
	}
	return findings
}

func TestExternalTestsCountAsTests(t *testing.T) {
	exported := make([]ASTFinding, 0)
	signals := newPackageSignals(goListPackage{
		ImportPath:   "example.com/pkg",
		XTestGoFiles: []string{"pkg_test.go"},
	})
	signals.exported = 25
	appendPackageSignalFindings(&exported, goListPackage{ImportPath: "example.com/pkg"}, signals)
	for _, f := range exported {
		if f.Rule == "untested-exported-surface" {
			t.Fatalf("external test files (XTestGoFiles) should count as tests, got finding: %s", f.Message)
		}
	}

	// Sanity check: with no test files at all the finding still fires.
	noTests := make([]ASTFinding, 0)
	bare := newPackageSignals(goListPackage{ImportPath: "example.com/pkg"})
	bare.exported = 25
	appendPackageSignalFindings(&noTests, goListPackage{ImportPath: "example.com/pkg"}, bare)
	found := false
	for _, f := range noTests {
		if f.Rule == "untested-exported-surface" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected untested-exported-surface finding when no test files exist")
	}
}
