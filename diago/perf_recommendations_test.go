package diago

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceSignals(t *testing.T) {
	t.Run("detects allocation expressions on a hot line", func(t *testing.T) {
		path := writeTempGoFile(t, `package sample

func collect(values []string) []item {
	out := make([]item, 0, len(values))
	for _, value := range values {
		out = append(out, item{Name: value})
	}
	return out
}

type item struct{ Name string }
`)

		signals := sourceSignals(path, 6)
		want := []string{
			"append growth: append(out, item{Name: value})",
			"composite literal: item",
		}
		assertContainsAll(t, signals, want)
	})

	t.Run("detects multiline call from continuation line", func(t *testing.T) {
		path := writeTempGoFile(t, `package sample

func collect(values []string) []item {
	return append(
		nil,
		item{Name: values[0]},
	)
}

type item struct{ Name string }
`)

		signals := sourceSignals(path, 6)
		assertContainsAll(t, signals, []string{"append growth: append(nil, item{Name: values[0]})"})
	})
}

func TestSourceSymbols(t *testing.T) {
	t.Run("extracts vars calls args allocs and append target", func(t *testing.T) {
		path := writeTempGoFile(t, `package sample

func collect(values []string) []item {
	out := make([]item, 0, len(values))
	for _, value := range values {
		out = append(out, item{Name: value})
	}
	return out
}

type item struct{ Name string }
`)

		symbols := sourceSymbols(path, 6)
		assertContainsAll(t, symbols.AssignedVars, []string{"out"})
		assertContainsAll(t, symbols.CalledFuncs, []string{"append"})
		assertContainsAll(t, symbols.Args, []string{"out", "item{Name: value}"})
		assertContainsAll(t, symbols.AllocatedTypes, []string{"item"})
		assertContainsAll(t, symbols.AppendTargets, []string{"out"})
	})
}

func writeTempGoFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.go")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := map[string]bool{}
	for _, item := range got {
		set[item] = true
	}
	for _, item := range want {
		if !set[item] {
			t.Fatalf("missing signal %q in %#v", item, got)
		}
	}
}
