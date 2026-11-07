package main

import (
	"flag"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAuditTestFlags() (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	target := fs.String("target", "./...", "")
	race := fs.Bool("race", false, "")
	return fs, target, race
}

func TestApplyPositionalTarget(t *testing.T) {
	t.Run("a target before the flags keeps the flags", func(t *testing.T) {
		fs, target, race := newAuditTestFlags()
		require.NoError(t, fs.Parse([]string{"./pkg/...", "-race"}))
		require.NoError(t, applyPositionalTarget(fs, target))
		assert.Equal(t, "./pkg/...", *target)
		assert.True(t, *race)
	})
	t.Run("no positional target keeps the flag value", func(t *testing.T) {
		fs, target, _ := newAuditTestFlags()
		require.NoError(t, fs.Parse([]string{"-target", "./a/..."}))
		require.NoError(t, applyPositionalTarget(fs, target))
		assert.Equal(t, "./a/...", *target)
	})
	t.Run("two different targets fail", func(t *testing.T) {
		fs, target, _ := newAuditTestFlags()
		require.NoError(t, fs.Parse([]string{"-target", "./a/...", "./b/..."}))
		require.Error(t, applyPositionalTarget(fs, target))
	})
	t.Run("a second positional argument fails", func(t *testing.T) {
		fs, target, _ := newAuditTestFlags()
		require.NoError(t, fs.Parse([]string{"./a/...", "./b/..."}))
		require.Error(t, applyPositionalTarget(fs, target))
	})
}
