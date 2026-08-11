package connector

import (
	"context"
	"os/exec"
)

// execLookPath is a seam over exec.LookPath so tests can stub the git binary.
var execLookPath = exec.LookPath

// execCommandContext is a seam over exec.CommandContext so tests can capture
// git invocations without running a real git process.
var execCommandContext = exec.CommandContext

// SetExecLookPath replaces the LookPath implementation (tests only).
func SetExecLookPath(f func(string) (string, error)) func() {
	prev := execLookPath
	execLookPath = f
	return func() { execLookPath = prev }
}

// SetExecCommand replaces the CommandContext implementation (tests only).
func SetExecCommand(f func(ctx context.Context, name string, args ...string) *exec.Cmd) func() {
	prev := execCommandContext
	execCommandContext = f
	return func() { execCommandContext = prev }
}

// fakeCmd is a minimal exec.Cmd-like for tests that want a captured command.
type fakeCmd struct {
	ctx  context.Context
	name string
	args []string
}

// (fakeCmd intentionally does NOT implement exec.Cmd; tests supply a custom
// execCommandContext that returns a real *exec.Cmd driven by a script.)
