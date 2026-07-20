package diago

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

func TestMissingContextContract(t *testing.T) {
	tests := []struct {
		name            string
		source          string
		allowTypeErrors bool
		want            []string
		wantNot         []string
	}{
		{
			name: "database sql legacy and context APIs",
			source: `package sample
import ("context"; "database/sql")
func LegacyQuery(db *sql.DB) { _, _ = db.Query("select 1") }
func LegacyBegin(db *sql.DB) { _, _ = db.Begin() }
func ContextQuery(ctx context.Context, db *sql.DB) { _, _ = db.QueryContext(ctx, "select 1") }
func DatabaseHandle() { _, _ = sql.Open("driver", "source") }
`,
			want:    []string{"LegacyQuery", "LegacyBegin"},
			wantNot: []string{"ContextQuery", "DatabaseHandle"},
		},
		{
			name: "HTTP client and server operations",
			source: `package sample
import ("context"; "net/http")
func ClientCall(client *http.Client, request *http.Request) { _, _ = client.Do(request) }
func PackageCall() { _, _ = http.Get("https://example.com") }
func ServerCall(server *http.Server) { _ = server.ListenAndServe() }
func RequestCall(ctx context.Context, client *http.Client, request *http.Request) {
	_, _ = client.Do(request.WithContext(ctx))
}
`,
			want:    []string{"ClientCall", "PackageCall", "ServerCall"},
			wantNot: []string{"RequestCall"},
		},
		{
			name: "network dial alternatives",
			source: `package sample
import ("context"; "net")
func Dial() { _, _ = net.Dial("tcp", "localhost:80") }
func DialWithContext(ctx context.Context, dialer *net.Dialer) {
	_, _ = dialer.DialContext(ctx, "tcp", "localhost:80")
}
`,
			want:    []string{"Dial"},
			wantNot: []string{"DialWithContext"},
		},
		{
			name: "exec operations",
			source: `package sample
import ("context"; "os/exec")
func Run(command *exec.Cmd) { _ = command.Run() }
func Output(command *exec.Cmd) { _, _ = command.Output() }
func Command() *exec.Cmd { return exec.Command("tool") }
func CommandWithBackground() *exec.Cmd { return exec.CommandContext(context.Background(), "tool") }
func RunWithContext(ctx context.Context) { _ = exec.CommandContext(ctx, "tool").Run() }
`,
			want:    []string{"Run", "Output"},
			wantNot: []string{"Command", "CommandWithBackground", "RunWithContext"},
		},
		{
			name: "local and embedded filesystems cannot consume context",
			source: `package sample
import ("embed"; "io"; "io/fs"; "os")
func LocalFiles() { _, _ = os.ReadFile("settings.json"); _, _ = os.Open("settings.json") }
func GenericFiles(files fs.FS) { _, _ = files.Open("index.html"); _, _ = fs.ReadFile(files, "index.html") }
func EmbeddedFiles(files embed.FS) { _, _ = files.Open("index.html"); _, _ = files.ReadFile("index.html") }
func ReadStream(reader io.Reader) { _, _ = io.ReadAll(reader) }
`,
			wantNot: []string{"LocalFiles", "GenericFiles", "EmbeddedFiles", "ReadStream"},
		},
		{
			name: "custom context signature and sibling",
			source: `package sample
import "context"
type Loader interface { Load(context.Context, string) error }
type Client struct{}
func (*Client) Fetch(string) error { return nil }
func (*Client) FetchContext(context.Context, string) error { return nil }
func Direct(loader Loader) { _ = loader.Load(context.Background(), "key") }
func Alternative(client *Client) { _ = client.Fetch("key") }
func Propagated(ctx context.Context, client *Client) { _ = client.FetchContext(ctx, "key") }
`,
			want:    []string{"Direct"},
			wantNot: []string{"Alternative", "Propagated"},
		},
		{
			name: "arbitrary method names and constructors",
			source: `package sample
type Registry struct{}
func (*Registry) Get(string) (string, bool) { return "", false }
func (*Registry) Open(string) error { return nil }
func (*Registry) Do() {}
type Service struct{ registry *Registry }
func Lookup(registry *Registry) { _, _ = registry.Get("key"); _ = registry.Open("key"); registry.Do() }
func NewService(registry *Registry) *Service { return &Service{registry: registry} }
`,
			wantNot: []string{"Lookup", "NewService"},
		},
		{
			name: "incomplete type information stays conservative",
			source: `package sample
func Unresolved(client MissingClient) { _, _ = client.Get("key"); _ = client.Open("file"); client.Do() }
`,
			allowTypeErrors: true,
			wantNot:         []string{"Unresolved"},
		},
		{
			name: "context convenience wrapper",
			source: `package sample
import ("context"; "net/http")
type Runner struct{}
func (*Runner) Execute() error { return (&Runner{}).ExecuteContext(context.Background()) }
func (*Runner) ExecuteContext(context.Context) error { return nil }
func (*Runner) ExecuteAndFetch() error {
	if err := (&Runner{}).ExecuteContext(context.Background()); err != nil { return err }
	_, err := http.Get("https://example.com")
	return err
}
func (*Runner) ExecuteAndFetchContext(context.Context) error { return nil }
`,
			want:    []string{"*Runner.ExecuteAndFetch"},
			wantNot: []string{"*Runner.Execute"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := contextFindings(t, tc.source, tc.allowTypeErrors)
			missing := findingsByRule(findings, "missing-context-param")
			for _, symbol := range tc.want {
				if !missing[symbol] {
					t.Errorf("missing finding for %s: %+v", symbol, findings)
				}
			}
			for _, symbol := range tc.wantNot {
				if missing[symbol] {
					t.Errorf("unexpected finding for %s: %+v", symbol, findings)
				}
			}
		})
	}
}

func TestBackgroundContextRecognizesBoundedAndOwnedRoots(t *testing.T) {
	findings := contextFindings(t, `package sample
import (
	"context"
	"time"
)
type Owner struct { cancel context.CancelFunc }
type Lifecycle struct{}
func (*Lifecycle) Own(cancel context.CancelFunc) { func() { cancel() }() }
func (*Lifecycle) MaybeOwn(cancel context.CancelFunc, enabled bool) { if enabled { cancel() } }
type CallbackLifecycle struct { callbacks []func() }
func (l *CallbackLifecycle) OnStop(callback func()) { l.callbacks = append(l.callbacks, callback) }
func (*CallbackLifecycle) Ignore(func()) {}
func (l *CallbackLifecycle) OnStopCancel(cancel context.CancelFunc) { l.OnStop(func() { cancel() }) }
func ReturnCallback(callback func()) func() { return callback }
func Unbounded() context.Context { return context.Background() }
func BoundedTimeout() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = ctx
}
func BoundedDeadline() {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	_ = ctx
}
func ClosedCancel() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
}
func ReturnedOwner() *Owner {
	_, cancel := context.WithCancel(context.Background())
	return &Owner{cancel: cancel}
}
func RegisteredOwner(lifecycle *Lifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.Own(cancel)
	_ = ctx
}
func ConditionalOwner(lifecycle *Lifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.MaybeOwn(cancel, false)
	_ = ctx
}
func CallbackOwner(lifecycle *CallbackLifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.OnStop(func() { cancel() })
	_ = ctx
}
func TransitiveCallbackOwner(lifecycle *CallbackLifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.OnStopCancel(cancel)
	_ = ctx
}
func NonOwningCallback(lifecycle *CallbackLifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.Ignore(func() { cancel() })
	_ = ctx
}
func DiscardedReturnedCallback() {
	ctx, cancel := context.WithCancel(context.Background())
	_ = ReturnCallback(func() { cancel() })
	_ = ctx
}
func DeferredCallback() {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel() }()
	_ = ctx
}
func ReturnedCallback() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	return ctx, func() { cancel() }
}
func DetachedCallback() {
	ctx, cancel := context.WithCancel(context.Background())
	callback := func() { cancel() }
	_, _ = ctx, callback
}
func CallbackScope(register func(func())) {
	register(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_ = ctx
	})
}
func DiscardedCancel() {
	ctx, _ := context.WithCancel(context.Background())
	_ = ctx
}
func UnclosedCancel() {
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = ctx, cancel
}
`, false)
	background := findingsByRule(findings, "background-context")
	for _, symbol := range []string{
		"Unbounded", "ConditionalOwner", "NonOwningCallback", "DiscardedReturnedCallback", "DetachedCallback",
		"DiscardedCancel", "UnclosedCancel",
	} {
		if !background[symbol] {
			t.Errorf("missing background-context finding for %s: %+v", symbol, findings)
		}
	}
	for _, symbol := range []string{
		"BoundedTimeout", "BoundedDeadline", "ClosedCancel", "ReturnedOwner", "RegisteredOwner",
		"CallbackOwner", "TransitiveCallbackOwner", "DeferredCallback", "ReturnedCallback", "CallbackScope",
	} {
		if background[symbol] {
			t.Errorf("bounded or owned root reported for %s: %+v", symbol, findings)
		}
	}
}

func TestBackgroundContextMainAndRuleDirectives(t *testing.T) {
	findings := contextFindings(t, `package main
import "context"
func main() {
	_ = context.Background()
	callback := func() { _ = context.Background() }
	_ = callback
}
func helper() { _ = context.Background() }
func suppressed() {
	//diago:ignore background-context process-owned root
	_ = context.Background()
}
func wrongRule() {
	//diago:ignore ignored-call-result wrong rule
	_ = context.Background()
}
func detached() {
	//diago:ignore background-context not adjacent

	_ = context.Background()
}
	`, false)
	background := findingsByRule(findings, "background-context")
	if background["suppressed"] {
		t.Fatalf("explicitly ignored root was reported: %+v", findings)
	}
	for _, symbol := range []string{"main", "helper", "wrongRule", "detached"} {
		if !background[symbol] {
			t.Errorf("missing background-context finding for %s: %+v", symbol, findings)
		}
	}
	mainFindings := 0
	for _, finding := range findings {
		if finding.Rule == "background-context" && finding.Symbol == "main" {
			mainFindings++
		}
	}
	if mainFindings != 1 {
		t.Fatalf("main findings = %d, want only the nested callback root: %+v", mainFindings, findings)
	}
}

func TestBackgroundContextConvenienceWrapperScope(t *testing.T) {
	findings := contextFindings(t, `package sample
import "context"
type Runner struct{}
func (*Runner) Execute() error { return (&Runner{}).ExecuteContext(context.Background()) }
func (*Runner) ExecuteContext(context.Context) error { return nil }
func (*Runner) ExecuteWithCallback() func() error {
	return func() error { return (&Runner{}).ExecuteWithCallbackContext(context.Background()) }
}
func (*Runner) ExecuteWithCallbackContext(context.Context) error { return nil }
`, false)
	background := findingsByRule(findings, "background-context")
	if background["*Runner.Execute"] {
		t.Fatalf("direct convenience wrapper root was reported: %+v", findings)
	}
	if !background["*Runner.ExecuteWithCallback"] {
		t.Fatalf("nested callback root inherited the wrapper exemption: %+v", findings)
	}
}

func TestBackgroundOwnershipCases(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   bool
	}{
		{
			name: "discarded cancel is not ownership",
			source: `package sample
import "context"
func Check() { ctx, _ := context.WithCancel(context.Background()); _ = ctx }
`,
			want: true,
		},
		{
			name: "unused closure is not ownership",
			source: `package sample
import "context"
func Check() {
	ctx, cancel := context.WithCancel(context.Background())
	callback := func() { cancel() }
	_, _ = ctx, callback
}
`,
			want: true,
		},
		{
			name: "non owning callback consumer is not ownership",
			source: `package sample
import "context"
func consume(func()) {}
func Check() {
	ctx, cancel := context.WithCancel(context.Background())
	consume(func() { cancel() })
	_ = ctx
}
`,
			want: true,
		},
		{
			name: "stored callback transfers ownership",
			source: `package sample
import "context"
type Lifecycle struct { callbacks []func() }
func (l *Lifecycle) Own(callback func()) { l.callbacks = append(l.callbacks, callback) }
func Check(lifecycle *Lifecycle) {
	ctx, cancel := context.WithCancel(context.Background())
	lifecycle.Own(func() { cancel() })
	_ = ctx
}
`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := contextFindings(t, tc.source, false)
			got := findingsByRule(findings, "background-context")["Check"]
			if got != tc.want {
				t.Fatalf("background-context finding = %v, want %v: %+v", got, tc.want, findings)
			}
		})
	}
}

func contextFindings(t *testing.T, source string, allowTypeErrors bool) []ASTFinding {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	conf := types.Config{Importer: importer.Default(), Error: func(error) {}}
	_, checkErr := conf.Check("example.com/sample", fset, []*ast.File{file}, info)
	if checkErr != nil && !allowTypeErrors {
		t.Fatal(checkErr)
	}
	files := map[string]*ast.File{"sample.go": file}
	ctx := astContext{
		pkg:       goListPackage{ImportPath: "example.com/sample", Name: file.Name.Name},
		path:      "sample.go",
		fset:      fset,
		file:      file,
		types:     info,
		cancelers: collectCancelParams("example.com/sample", files, []string{"sample.go"}, info),
	}
	var findings []ASTFinding
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		findContextAndTimeoutSignals(&findings, ctx, fn, funcName(fn))
	}
	return findings
}

func findingsByRule(findings []ASTFinding, rule string) map[string]bool {
	symbols := map[string]bool{}
	for _, finding := range findings {
		if finding.Rule == rule {
			symbols[finding.Symbol] = true
		}
	}
	return symbols
}
