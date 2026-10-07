package doltcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrDeniedVerb is what a Runner reports for a command on the outbound deny-lists:
// one that sends a store's state somewhere else or takes someone else's in. No
// child is started for it.
var ErrDeniedVerb = errors.New("doltcli: the command is on the outbound deny-list")

var errRunnerNotBuilt = errors.New("doltcli: the runner is not built")

// Tool labels the program a child ran. A label is never an executable name: a
// Runner starts only the paths it was given.
type Tool string

// The two programs a seeding runs.
const (
	ToolDolt Tool = "dolt"
	ToolBd   Tool = "bd"
)

// Child is one process a Runner started: what it ran, where, and with which
// environment, exactly as it was handed to the process.
type Child struct {
	Tool Tool
	Args []string
	Dir  string
	Env  []string
}

// ChildLog records, in order, the children a Runner starts. The zero value is
// ready to use and a nil *ChildLog records nothing.
type ChildLog struct {
	mu       sync.Mutex
	children []Child
}

// Record appends c to the log.
func (l *ChildLog) Record(c Child) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.children = append(l.children, c)
}

// Children returns a copy of the log, oldest first.
func (l *ChildLog) Children() []Child {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Child(nil), l.children...)
}

// Result is what a finished child printed.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// ExitError is a child that ran and exited with a status above zero. What it
// printed is kept apart, standard output as data and standard error as the
// diagnostic it is.
type ExitError struct {
	Tool     Tool
	Args     []string
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("%s %s: exit status %d\n%s", e.Tool, strings.Join(e.Args, " "), e.ExitCode, e.Stderr)
}

// Runner starts the dolt and bd children of a seeding. It is the one place the
// harness starts a child on a seed (H2): every child gets the environment the
// Runner was built with (H19), none gets a verb on the deny-lists, and every one
// started is recorded in Log.
type Runner struct {
	// DoltBin and BdBin are the programs, named by path and never looked up.
	DoltBin string
	BdBin   string
	// Env is the whole environment of every child. It is built from an allow-list
	// (see ChildEnv) and nothing of the ambient environment is added to it.
	Env []string
	// Log, when set, records every child.
	Log *ChildLog
}

// Dolt runs the dolt CLI in dir.
func (*Runner) Dolt(_ context.Context, _ string, _ ...string) (Result, error) {
	return Result{}, errRunnerNotBuilt
}

// Bd runs bd in dir, with automatic backups explicitly off.
func (*Runner) Bd(_ context.Context, _ string, _ ...string) (Result, error) {
	return Result{}, errRunnerNotBuilt
}

// Query runs one read-only statement in the database at dir and returns its rows,
// the way the package-level Query does, but as a child of this Runner.
func (*Runner) Query(_ context.Context, _, _ string) (header []string, rows [][]Cell, err error) {
	return nil, nil, errRunnerNotBuilt
}

// ChildEnv is the environment every dolt and bd child that touches a seed runs
// under (H19). It is built from nothing, not from the ambient environment by
// deleting names from it, so a variable nobody listed cannot reach a child. HOME,
// XDG_CONFIG_HOME, DOLT_ROOT_PATH and TMPDIR all lie inside envRoot, which the
// seeding owns and which holds its own dolt identity, so neither the real home nor
// the real dolt configuration is read or written.
func ChildEnv(_, _ string) []string {
	return nil
}
