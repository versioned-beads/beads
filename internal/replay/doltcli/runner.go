package doltcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// ErrDeniedVerb is what a Runner reports for a command on the outbound deny-lists:
// one that sends a store's state somewhere else or takes someone else's in. No
// child is started for it.
var ErrDeniedVerb = errors.New("doltcli: the command is on the outbound deny-list")

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

// Dolt runs the dolt CLI in dir. A command on the dolt deny-lists, and a sql command
// whose statement calls a procedure on the procedure deny-list, is refused with
// ErrDeniedVerb: no child is started and nothing is logged for it.
func (r *Runner) Dolt(ctx context.Context, dir string, args ...string) (Result, error) {
	if verb, ok := startsVerb(args, DeniedDoltVerbs); ok {
		return Result{}, fmt.Errorf("%w: dolt %s", ErrDeniedVerb, strings.Join(verb, " "))
	}
	if _, ok := startsVerb(args, [][]string{{"sql"}}); ok {
		for _, arg := range args {
			if proc, ok := calledProcedure(arg); ok {
				return Result{}, fmt.Errorf("%w: a dolt sql statement calls %s", ErrDeniedVerb, proc)
			}
		}
	}
	return r.start(ctx, ToolDolt, r.DoltBin, dir, args, SanitizedEnv(r.Env))
}

// Bd runs bd in dir, with automatic backups explicitly off. A command on the bd
// deny-list is refused with ErrDeniedVerb: no child is started and nothing is
// logged for it.
func (r *Runner) Bd(ctx context.Context, dir string, args ...string) (Result, error) {
	if verb, ok := startsVerb(args, DeniedBdVerbs); ok {
		return Result{}, fmt.Errorf("%w: bd %s", ErrDeniedVerb, strings.Join(verb, " "))
	}
	return r.start(ctx, ToolBd, r.BdBin, dir, args, backupOff(SanitizedEnv(r.Env)))
}

// Query runs one read-only statement in the database at dir and returns its rows,
// the way the package-level Query does, but as a child of this Runner.
func (r *Runner) Query(ctx context.Context, dir, sql string) (header []string, rows [][]Cell, err error) {
	if proc, ok := calledProcedure(sql); ok {
		return nil, nil, fmt.Errorf("%w: the statement calls %s", ErrDeniedVerb, proc)
	}
	if err := refuseServed(dir); err != nil {
		return nil, nil, err
	}
	res, err := r.Dolt(ctx, dir, "sql", "-q", sql, "-r", "csv")
	if err != nil {
		return nil, nil, err
	}
	header, rows, err = parseCSV(res.Stdout)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing dolt sql csv output: %w", err)
	}
	return header, rows, nil
}

// start runs one child to the end and records it. The environment is recorded as it
// is handed to the process, so the log says what the child was given and not what
// the Runner was configured with.
func (r *Runner) start(ctx context.Context, tool Tool, bin, dir string, args, env []string) (Result, error) {
	if bin == "" {
		return Result{}, fmt.Errorf("doltcli: the runner was given no %s to run", tool)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	r.Log.Record(Child{Tool: tool, Args: slices.Clone(args), Dir: dir, Env: slices.Clone(env)})
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return res, &ExitError{Tool: tool, Args: slices.Clone(args), ExitCode: exit.ExitCode(), Stdout: res.Stdout, Stderr: res.Stderr}
	}
	return res, fmt.Errorf("%s %s: %w", tool, strings.Join(args, " "), err)
}

// backupOff returns env with automatic backup explicitly switched off, whatever it
// said before. An unset variable is not an off: the product's default is its own.
func backupOff(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name == "BD_BACKUP_ENABLED" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "BD_BACKUP_ENABLED=0")
}

// isOption reports whether an argument is an option and not a word of the command.
func isOption(arg string) bool { return len(arg) > 1 && arg[0] == '-' }

// startsVerb reports whether args begin a command on verbs, and which verb it is.
// The words of a verb are the arguments that are not options. They may follow
// leading options (`--data-dir DIR push`), and an option between two words of a verb
// does not separate them. An argument after the command's own words is not looked
// at, so a remote that happens to be named `push` is not the verb `push`.
func startsVerb(args []string, verbs [][]string) ([]string, bool) {
	for i, arg := range args {
		if !isOption(arg) {
			for _, verb := range verbs {
				if hasWords(args[i:], verb) {
					return verb, true
				}
			}
			// A word that does not follow an option can be nothing but the first word of
			// the command; one that does may have been that option's value.
			if i == 0 || !isOption(args[i-1]) {
				return nil, false
			}
		}
	}
	return nil, false
}

// hasWords reports whether the first word of args is verb[0], the next verb[1], and
// so on, skipping options between them.
func hasWords(args, verb []string) bool {
	k := 0
	for _, arg := range args {
		if isOption(arg) {
			continue
		}
		if arg != verb[k] {
			return false
		}
		k++
		if k == len(verb) {
			return true
		}
	}
	return false
}

// calledProcedure reports whether a SQL text mentions a procedure on the deny-list
// as a whole word, in any case. A table named for one of them (dolt_remotes) is not
// the procedure (dolt_remote), and a refusal that is too wide stops only the
// harness's own mistake.
func calledProcedure(sql string) (string, bool) {
	names := make([]string, len(DeniedSQLProcedures))
	for i, name := range DeniedSQLProcedures {
		names[i] = regexp.QuoteMeta(name)
	}
	re := regexp.MustCompile(`(?i)\b(?:` + strings.Join(names, "|") + `)\b`)
	if m := re.FindString(sql); m != "" {
		return strings.ToLower(m), true
	}
	return "", false
}

// ChildEnv is the environment every dolt and bd child that touches a seed runs
// under (H19). It is built from nothing, not from the ambient environment by
// deleting names from it, so a variable nobody listed cannot reach a child. HOME,
// XDG_CONFIG_HOME, DOLT_ROOT_PATH and TMPDIR all lie inside envRoot, which the
// seeding owns and which holds its own dolt identity, so neither the real home nor
// the real dolt configuration is read or written. PATH is one directory of the
// seeding's own, and no dolt is on it: a bd child can run git and nothing else, so
// the dolt it talks to is the engine it links and not a CLI it found.
//
// It only names the paths. The seeding creates them.
func ChildEnv(envRoot, workDir string) []string {
	home := filepath.Join(envRoot, "home")
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"DOLT_ROOT_PATH=" + filepath.Join(envRoot, "dolt-root"),
		"TMPDIR=" + filepath.Join(envRoot, "tmp"),
		"LC_ALL=C",
		"PATH=" + filepath.Join(envRoot, "bin"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(workDir),
		"BD_DISABLE_METRICS=1",
		"DO_NOT_TRACK=1",
		"BD_DISABLE_EVENT_FLUSH=1",
		"DOLT_DISABLE_EVENT_FLUSH=1",
		"BD_BACKUP_ENABLED=0",
	}
}
