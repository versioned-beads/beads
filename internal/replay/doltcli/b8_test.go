package doltcli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
)

// standIn writes a program that logs its arguments and its environment to files of
// its own, runs body, and exits. It stands for dolt or bd, so that what a Runner
// starts, and with what, can be read from outside the Runner.
func standIn(t *testing.T, name, body string) (bin, calls, env string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, name)
	calls = filepath.Join(dir, "calls")
	env = filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + calls + "'\n" +
		"env >> '" + env + "'\n" +
		"echo --- >> '" + env + "'\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 -- a test stand-in must be executable
		t.Fatalf("writing the stand-in: %v", err)
	}
	return bin, calls, env
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a file the test's own stand-in wrote
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// B8.RunnerRefusesOutboundVerbs: the Runner refuses every verb on the shared
// deny-lists, however it is spelled around them, and starts no child for it. The
// lists themselves are pinned first, so shrinking one cannot make this pass.
func TestB8RunnerRefusesOutboundVerbs(t *testing.T) {
	for _, want := range [][]string{
		{"push"}, {"fetch"}, {"pull"}, {"remote", "add"},
		{"backup", "add"}, {"backup", "sync"}, {"backup", "sync-url"}, {"backup", "restore"},
	} {
		if !slices.ContainsFunc(doltcli.DeniedDoltVerbs, func(v []string) bool { return slices.Equal(v, want) }) {
			t.Errorf("the dolt deny-list does not hold %v", want)
		}
	}
	for _, want := range [][]string{{"backup"}, {"dolt", "push"}, {"dolt", "pull"}} {
		if !slices.ContainsFunc(doltcli.DeniedBdVerbs, func(v []string) bool { return slices.Equal(v, want) }) {
			t.Errorf("the bd deny-list does not hold %v", want)
		}
	}
	for _, want := range []string{"dolt_push", "dolt_pull", "dolt_fetch", "dolt_remote", "dolt_backup"} {
		if !slices.Contains(doltcli.DeniedSQLProcedures, want) {
			t.Errorf("the procedure deny-list does not hold %s", want)
		}
	}

	var log doltcli.ChildLog
	dolt, doltCalls, _ := standIn(t, "dolt-stand-in", "exit 0")
	bd, bdCalls, _ := standIn(t, "bd-stand-in", "exit 0")
	r := &doltcli.Runner{DoltBin: dolt, BdBin: bd, Env: []string{"PATH=/usr/bin:/bin"}, Log: &log}
	ctx := context.Background()
	dir := t.TempDir()

	refuses := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, doltcli.ErrDeniedVerb) {
			t.Errorf("%s: err = %v, want ErrDeniedVerb", what, err)
		}
	}
	for _, verb := range doltcli.DeniedDoltVerbs {
		args := append(slices.Clone(verb), "origin", "main")
		_, err := r.Dolt(ctx, dir, args...)
		refuses("dolt "+strings.Join(args, " "), err)
	}
	for _, verb := range doltcli.DeniedBdVerbs {
		args := append(slices.Clone(verb), "--json")
		_, err := r.Bd(ctx, dir, args...)
		refuses("bd "+strings.Join(args, " "), err)
	}
	// A leading flag must not hide the verb behind it.
	_, err := r.Dolt(ctx, dir, "--data-dir", dir, "push")
	refuses("dolt --data-dir <dir> push", err)
	_, err = r.Bd(ctx, dir, "--json", "backup", "sync")
	refuses("bd --json backup sync", err)
	// The same verbs through SQL: a procedure call is the verb, spelled in a statement.
	for _, proc := range doltcli.DeniedSQLProcedures {
		for _, stmt := range []string{
			"CALL " + proc + "('origin', 'main')",
			"call " + strings.ToUpper(proc) + "()",
			"SELECT 1; CALL " + proc + "('x')",
		} {
			_, err := r.Dolt(ctx, dir, "sql", "-q", stmt)
			refuses("dolt sql -q "+stmt, err)
			_, _, err = r.Query(ctx, dir, stmt)
			refuses("Query "+stmt, err)
		}
	}

	if children := log.Children(); len(children) != 0 {
		t.Errorf("%d children were logged for refused commands: %+v", len(children), children)
	}
	if lines := readLines(t, doltCalls); len(lines) != 0 {
		t.Errorf("the dolt stand-in was started for refused commands: %q", lines)
	}
	if lines := readLines(t, bdCalls); len(lines) != 0 {
		t.Errorf("the bd stand-in was started for refused commands: %q", lines)
	}
}

// B8.RunnerAllowsRemovalAndReads is the other half of the refusal: taking a remote
// or a backup away is how a seeded copy is cut off, and reading the tables that
// list them is not a call, so none of those is refused. Each must start a child, so
// that the refusal test cannot pass by refusing everything.
func TestB8RunnerAllowsRemovalAndReads(t *testing.T) {
	var log doltcli.ChildLog
	dolt, doltCalls, _ := standIn(t, "dolt-stand-in", "exit 0")
	bd, bdCalls, _ := standIn(t, "bd-stand-in", "exit 0")
	r := &doltcli.Runner{DoltBin: dolt, BdBin: bd, Env: []string{"PATH=/usr/bin:/bin"}, Log: &log}
	ctx := context.Background()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"remote", "-v"}, {"remote", "remove", "origin"},
		{"backup", "-v"}, {"backup", "remove", "bk"},
		{"reset", "--hard", "abc"}, {"config", "--global", "--add", "user.name", "x"},
		{"sql", "-q", "SELECT * FROM dolt_remotes"},
		{"sql", "-q", "SELECT * FROM dolt_backups"},
	} {
		if _, err := r.Dolt(ctx, dir, args...); err != nil {
			t.Errorf("dolt %s: %v", strings.Join(args, " "), err)
		}
	}
	if _, _, err := r.Query(ctx, dir, "SELECT * FROM dolt_remotes"); err != nil {
		t.Errorf("Query of the remotes table: %v", err)
	}
	for _, args := range [][]string{
		{"migrate", "schema", "--json"}, {"count", "--json"}, {"create", "x", "--json"}, {"dolt", "show"},
	} {
		if _, err := r.Bd(ctx, dir, args...); err != nil {
			t.Errorf("bd %s: %v", strings.Join(args, " "), err)
		}
	}

	if got, want := len(readLines(t, doltCalls)), 9; got != want {
		t.Errorf("the dolt stand-in was started %d times, want %d", got, want)
	}
	if got, want := len(readLines(t, bdCalls)), 4; got != want {
		t.Errorf("the bd stand-in was started %d times, want %d", got, want)
	}
	children := log.Children()
	if len(children) != 13 {
		t.Fatalf("the log holds %d children, want 13: %+v", len(children), children)
	}
	if first := children[0]; first.Tool != doltcli.ToolDolt || first.Dir != dir || !slices.Equal(first.Args, []string{"remote", "-v"}) {
		t.Errorf("the first child is %+v, want dolt remote -v in %s", first, dir)
	}
	if last := children[len(children)-1]; last.Tool != doltcli.ToolBd || !slices.Equal(last.Args, []string{"dolt", "show"}) {
		t.Errorf("the last child is %+v, want bd dolt show", last)
	}
}

// B8.RunnerEnvironment (H19 at the runner): a child gets the environment the Runner
// was built with and nothing from the ambient one, and a bd child always has
// automatic backup switched off, whatever the Runner's environment said.
func TestB8RunnerEnvironment(t *testing.T) {
	t.Setenv("B8_AMBIENT_CANARY", "leaked")
	dolt, _, doltEnv := standIn(t, "dolt-stand-in", "exit 0")
	bd, _, bdEnv := standIn(t, "bd-stand-in", "exit 0")
	var log doltcli.ChildLog
	r := &doltcli.Runner{
		DoltBin: dolt, BdBin: bd, Log: &log,
		Env: []string{"PATH=/usr/bin:/bin", "B8_RUNNER_MARK=present", "BD_BACKUP_ENABLED=1"},
	}
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := r.Dolt(ctx, dir, "remote", "-v"); err != nil {
		t.Fatalf("dolt: %v", err)
	}
	if _, err := r.Bd(ctx, dir, "count", "--json"); err != nil {
		t.Fatalf("bd: %v", err)
	}

	for _, c := range []struct {
		what, dump string
	}{{"dolt", doltEnv}, {"bd", bdEnv}} {
		lines := readLines(t, c.dump)
		if !slices.Contains(lines, "B8_RUNNER_MARK=present") {
			t.Errorf("a %s child did not get the Runner's environment: %q", c.what, lines)
		}
		if slices.Contains(lines, "B8_AMBIENT_CANARY=leaked") {
			t.Errorf("a %s child saw the ambient environment: %q", c.what, lines)
		}
	}
	if lines := readLines(t, bdEnv); !slices.Contains(lines, "BD_BACKUP_ENABLED=0") || slices.Contains(lines, "BD_BACKUP_ENABLED=1") {
		t.Errorf("a bd child's backup setting is not an explicit off: %q", lines)
	}
	for _, child := range log.Children() {
		if child.Tool == doltcli.ToolBd && !slices.Contains(child.Env, "BD_BACKUP_ENABLED=0") {
			t.Errorf("the log does not show a bd child running with BD_BACKUP_ENABLED=0: %v", child.Env)
		}
	}
}

// B8.RunnerReportsExit: a child that exits non-zero is an ExitError carrying its
// status and its two streams apart; a child killed by a signal is not a refusal.
func TestB8RunnerReportsExit(t *testing.T) {
	bd, _, _ := standIn(t, "bd-stand-in", "echo to-stdout\necho to-stderr >&2\nexit 3")
	r := &doltcli.Runner{BdBin: bd, Env: []string{"PATH=/usr/bin:/bin"}}
	res, err := r.Bd(context.Background(), t.TempDir(), "create", "x")
	var exit *doltcli.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("err = %v, want an *ExitError", err)
	}
	if exit.ExitCode != 3 || exit.Tool != doltcli.ToolBd || !slices.Equal(exit.Args, []string{"create", "x"}) {
		t.Errorf("exit error = %+v, want bd create x with status 3", exit)
	}
	if string(res.Stdout) != "to-stdout\n" || string(res.Stderr) != "to-stderr\n" {
		t.Errorf("streams = %q and %q, want them apart", res.Stdout, res.Stderr)
	}
	if string(exit.Stdout) != string(res.Stdout) || string(exit.Stderr) != string(res.Stderr) {
		t.Errorf("the exit error's streams %q and %q differ from the result's", exit.Stdout, exit.Stderr)
	}

	killed, _, _ := standIn(t, "bd-killed", "kill -9 $$")
	r = &doltcli.Runner{BdBin: killed, Env: []string{"PATH=/usr/bin:/bin"}}
	if _, err := r.Bd(context.Background(), t.TempDir(), "count"); err == nil {
		t.Errorf("a killed child reported no error")
	} else if errors.As(err, &exit) {
		t.Errorf("a signal-killed child was reported as an exit status: %v", err)
	}
}

// B8.ChildEnvIsAllowList (H19, H14): the environment of a seeding's child is built
// from a list. Everything ambient, the override-class names included, is absent,
// the four locations lie inside the seeding's root, and no directory on the child's
// PATH holds a dolt.
func TestB8ChildEnvIsAllowList(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(t.TempDir(), "work")

	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "dolt"), []byte("#!/bin/sh\n"), 0o755); err != nil { // #nosec G306 -- a stand-in on PATH must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", filepath.Join(t.TempDir(), "ambient-home"))
	t.Setenv("B8_AMBIENT_CANARY", "leaked")
	t.Setenv("BD_ALLOW_REMOTE_MIGRATE", "1")
	t.Setenv("BEADS_SKIP_IDENTITY_CHECK", "1")
	t.Setenv("BD_IGNORE_SCHEMA_SKEW", "1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "1")

	got := map[string]string{}
	for _, kv := range doltcli.ChildEnv(root, work) {
		name, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Errorf("%q is not NAME=value", kv)
			continue
		}
		if _, dup := got[name]; dup {
			t.Errorf("%s is set twice", name)
		}
		got[name] = value
	}

	want := []string{
		"HOME", "XDG_CONFIG_HOME", "DOLT_ROOT_PATH", "TMPDIR", "LC_ALL", "PATH",
		"GIT_CONFIG_NOSYSTEM", "GIT_CEILING_DIRECTORIES", "BD_DISABLE_METRICS", "DO_NOT_TRACK",
		"BD_DISABLE_EVENT_FLUSH", "DOLT_DISABLE_EVENT_FLUSH", "BD_BACKUP_ENABLED",
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("%s is not in the child environment", name)
		}
	}
	for name := range got {
		if !slices.Contains(want, name) {
			t.Errorf("%s is in the child environment and not on the allow-list", name)
		}
	}

	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "DOLT_ROOT_PATH", "TMPDIR"} {
		rel, err := filepath.Rel(root, got[name])
		if got[name] == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a path inside the seeding's root %s", name, got[name], root)
		}
	}
	for name, value := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "BD_DISABLE_METRICS": "1", "DO_NOT_TRACK": "1",
		"BD_DISABLE_EVENT_FLUSH": "1", "DOLT_DISABLE_EVENT_FLUSH": "1", "BD_BACKUP_ENABLED": "0",
		"GIT_CEILING_DIRECTORIES": filepath.Dir(work),
	} {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if got["LC_ALL"] == "" {
		t.Errorf("LC_ALL is empty")
	}
	if got["PATH"] == os.Getenv("PATH") {
		t.Errorf("PATH is the ambient PATH")
	}
	for _, dir := range filepath.SplitList(got["PATH"]) {
		if _, err := os.Stat(filepath.Join(dir, "dolt")); err == nil {
			t.Errorf("%s is on the child's PATH and holds a dolt", dir)
		}
	}
}
