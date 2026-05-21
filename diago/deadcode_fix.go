package diago

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
)

func removeDeadCodeFindings(findings []ASTFinding) AuditCheck {
	byFile := map[string][]ASTFinding{}
	for _, finding := range findings {
		if finding.Rule == "dead-code" && strings.HasPrefix(finding.Message, "unexported function ") {
			byFile[finding.File] = append(byFile[finding.File], finding)
		}
	}
	if len(byFile) == 0 {
		return AuditCheck{Name: "deadcode-fix", Command: "remove unexported dead functions", Passed: true, Output: "no removable dead functions found"}
	}

	var out bytes.Buffer
	for file, fileFindings := range byFile {
		removed, err := removeDeadFuncsFromFile(file, fileFindings)
		if err != nil {
			return AuditCheck{Name: "deadcode-fix", Command: "remove unexported dead functions", Passed: false, Output: err.Error()}
		}
		for _, name := range removed {
			fmt.Fprintf(&out, "removed %s from %s\n", name, file)
		}
	}
	return AuditCheck{Name: "deadcode-fix", Command: "remove unexported dead functions", Passed: true, Output: out.String()}
}

type removal struct {
	start int
	end   int
	name  string
}

func removeDeadFuncsFromFile(path string, findings []ASTFinding) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	wanted := deadCodeFindingLines(findings)
	removals := removableDeadFuncs(file, fset, data, wanted)
	if len(removals) == 0 {
		return nil, nil
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].start > removals[j].start })
	for _, r := range removals {
		data = append(data[:r.start], data[r.end:]...)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(removals))
	for i := len(removals) - 1; i >= 0; i-- {
		names = append(names, removals[i].name)
	}
	return names, nil
}

func deadCodeFindingLines(findings []ASTFinding) map[string]int {
	wanted := map[string]int{}
	for _, finding := range findings {
		wanted[finding.Symbol] = finding.Line
	}
	return wanted
}

func removableDeadFuncs(file *ast.File, fset *token.FileSet, data []byte, wanted map[string]int) []removal {
	var removals []removal
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !canRemoveDeadFunc(fn, fset, wanted) {
			continue
		}
		start := fset.Position(fn.Pos()).Offset
		end := extendRemovalEnd(data, fset.Position(fn.End()).Offset)
		removals = append(removals, removal{start: start, end: end, name: fn.Name.Name})
	}
	return removals
}

func canRemoveDeadFunc(fn *ast.FuncDecl, fset *token.FileSet, wanted map[string]int) bool {
	if fn.Recv != nil || fn.Doc != nil || !shouldTrackDeadFunc(fn) {
		return false
	}
	line, ok := wanted[fn.Name.Name]
	return ok && fset.Position(fn.Pos()).Line == line
}

func extendRemovalEnd(data []byte, end int) int {
	for end < len(data) && (data[end] == '\n' || data[end] == '\r') {
		end++
	}
	return end
}
