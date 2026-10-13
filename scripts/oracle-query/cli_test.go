package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// These tests drive the real command line. A stand-in dolt is placed first on
// PATH: it records the arguments and the environment it was started with and
// prints canned CSV, so the tests need no external tool and pin exactly what
// the command sends to dolt and what it prints back.
//
//   - TestB1OneDenyList: every name on the deny-list is absent from the
//     environment of the dolt process the command actually spawns.
//   - TestB1MainsCharacterised_OracleCLI: the command's stdout, exit status
//     and dolt invocation equal a golden transcript captured before the
//     command's logic moved into a library.

var updateGolden = flag.Bool("update", false, "rewrite golden transcripts from the current output")

// cliRef is a syntactically valid commit ref used wherever the value only
// needs to pass validation.
const cliRef = "0123456789abcdefghijklmnopqrstuv"

// deniedEnvNames is the union deny-list, written out independently of the
// implementation so the test cannot pass by importing the same mistake.
var deniedEnvNames = []string{
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_PORT",
	"BEADS_ACTOR",
	"BD_ACTOR",
	"GT_ROOT",
	"BEADS_DIR",
	"BEADS_HOLDER_TOKEN",
	"GC_BEADS_SCOPE_ROOT",
	"BEADS_DOLT_AUTO_START",
	"BEADS_DOLT_SYNC_CLI_REMOTES",
	"BEADS_BACKUP_ENABLED",
}

// buildCLI returns this package's command. Under Bazel the sandbox holds no Go
// toolchain, so the go_test names the go_binary as data and sets
// ORACLE_QUERY_BIN to its runfile; elsewhere the command is compiled into a
// temporary directory.
func buildCLI(t *testing.T) string {
	t.Helper()
	if bazeltest.IsBazel() {
		bin, err := bazeltest.RunfileEnv("ORACLE_QUERY_BIN")
		if err != nil {
			t.Fatalf("locating the prebuilt command: %v", err)
		}
		return bin
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found: %v", err)
	}
	out := filepath.Join(t.TempDir(), "oracle-query")
	if b, err := exec.Command(goBin, "build", "-o", out, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}

// doltShim is a stand-in dolt that records how it was started.
type doltShim struct {
	dir     string // put first on PATH
	argvLog string // one argument per line, one invocation after another
	envLog  string // the environment of the last invocation
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newDoltShim installs a dolt that prints csv on every invocation.
func newDoltShim(t *testing.T, csv string) doltShim {
	t.Helper()
	dir := t.TempDir()
	s := doltShim{dir: dir, argvLog: filepath.Join(dir, "argv.log"), envLog: filepath.Join(dir, "env.log")}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %s\nenv > %s\ncat <<'CSV'\n%sCSV\n",
		shellQuote(s.argvLog), shellQuote(s.envLog), csv)
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the dolt stand-in: %v", err)
	}
	return s
}

// cliEnv is the whole environment the command is started with; nothing is
// inherited, so the only names present are the ones a test put there.
func cliEnv(t *testing.T, shim doltShim, extra ...string) []string {
	t.Helper()
	env := []string{"PATH=" + shim.dir + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
	return append(env, extra...)
}

type cliResult struct {
	exit           int
	stdout, stderr string
}

func runCLI(t *testing.T, bin string, env []string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	res := cliResult{}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %s: %v", bin, err)
		}
		res.exit = ee.ExitCode()
	}
	res.stdout, res.stderr = so.String(), se.String()
	return res
}

// readLog returns a shim log's contents, or "" when the shim never ran.
func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// envNames returns the set of variable names in an `env` dump.
func envNames(dump string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(dump, "\n") {
		if name, _, ok := strings.Cut(line, "="); ok && name != "" {
			names[name] = true
		}
	}
	return names
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("creating testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s (run with -update to create it): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("output differs from %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

// B1.OneDenyList: BEADS_DIR and the other names the oracle path used to let
// through never reach the dolt process it spawns.
func TestB1OneDenyList(t *testing.T) {
	bin := buildCLI(t)
	shim := newDoltShim(t, "id,title\n")

	extra := []string{"REPLAY_TEST_KEEP=kept"}
	for _, name := range deniedEnvNames {
		extra = append(extra, name+"=leaked-"+strings.ToLower(name))
	}
	res := runCLI(t, bin, cliEnv(t, shim, extra...), "-dir", t.TempDir(), "-ref", cliRef, "-issue", "x-1")
	if res.exit != 0 {
		t.Fatalf("command exited %d\nstderr: %s", res.exit, res.stderr)
	}

	got := envNames(readLog(t, shim.envLog))
	if len(got) == 0 {
		t.Fatal("the dolt stand-in never recorded an environment: the command did not spawn it")
	}
	for _, name := range deniedEnvNames {
		if got[name] {
			t.Errorf("%s reached the spawned dolt process", name)
		}
	}
	if !got["REPLAY_TEST_KEEP"] {
		t.Error("an unrelated variable was stripped from the spawned dolt process")
	}
}

// B1.MainsCharacterised (oracle): stdout, exit status and the dolt invocation
// for a row that exists, a row that does not, bad input and missing flags.
func TestB1MainsCharacterised_OracleCLI(t *testing.T) {
	bin := buildCLI(t)

	var transcript strings.Builder
	scenario := func(name, csv string, args ...string) cliResult {
		shim := newDoltShim(t, csv)
		res := runCLI(t, bin, cliEnv(t, shim), args...)
		fmt.Fprintf(&transcript, "== %s\nexit: %d\nstdout:\n%s", name, res.exit, res.stdout)
		if name == "missing flags" {
			fmt.Fprintf(&transcript, "stderr:\n%s", res.stderr)
		}
		fmt.Fprintf(&transcript, "dolt argv:\n%s\n", readLog(t, shim.argvLog))
		return res
	}

	dir := t.TempDir()
	scenario("row exists", "id,status,title\nx-1,open,\"Widget, deluxe\"\n", "-dir", dir, "-ref", cliRef, "-issue", "x-1")
	scenario("row missing", "id,status,title\n", "-dir", dir, "-ref", cliRef, "-issue", "x-1")
	scenario("missing flags", "")
	if res := scenario("invalid ref", "", "-dir", dir, "-ref", "not a ref", "-issue", "x-1"); !strings.Contains(res.stderr, "invalid ref") {
		t.Errorf("invalid ref: stderr does not mention it: %q", res.stderr)
	}

	checkGolden(t, "cli.golden", transcript.String())
}
