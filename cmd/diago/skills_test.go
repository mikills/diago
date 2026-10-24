package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeInstall(t *testing.T) {
	dir := t.TempDir()
	if err := installClaude(dir, false, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "diago", "SKILL.md")
	want, err := skillsFS.ReadFile(skillDoc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("installed skill differs: error=%v", err)
	}
	custom := []byte("custom skill\n")
	if err := os.WriteFile(path, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installClaude(dir, false, false); err == nil {
		t.Fatal("existing file was not rejected")
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, custom) {
		t.Fatalf("existing skill was changed without force: error=%v", err)
	}
	if err := installClaude(dir, false, true); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("forced install did not restore bundled skill: error=%v", err)
	}
}

func TestClonerErrors(t *testing.T) {
	c := cloner{dest: t.TempDir()}
	want := errors.New("walk failed")
	if err := c.walk("unreadable", nil, want); !errors.Is(err, want) {
		t.Fatalf("walk returned %v, want %v", err, want)
	}
	if wrote, err := copyEntry(c.dest, "nonexistent", "missing/file", false); wrote || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing source: wrote=%v error=%v", wrote, err)
	}
	if c.n != 0 {
		t.Fatalf("failed copy incremented count to %d", c.n)
	}
}

func TestDispatchFallback(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"-target", "./..."}, {"--perf"}, {"not-a-command"}} {
		if dispatchCommand(args) {
			t.Fatalf("unexpected dispatch for %v", args)
		}
	}
}
