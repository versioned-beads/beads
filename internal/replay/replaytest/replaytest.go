// Package replaytest is the shared test support for the replay harness:
// tool requirements that fail loudly in the lanes that demand them, an
// isolated environment for every fixture, a bd binary (the job's prebuilt one,
// or one built once per test binary from this tree), and the small helpers the
// fixtures share.
//
// It is an ordinary package, not a _test package, so other packages can import
// it; only tests do.
package replaytest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/testutil/bazeltest"
)

// Need names the external tools a test requires.
type Need uint

const (
	// NeedDolt requires the dolt CLI on PATH.
	NeedDolt Need = 1 << iota
	// NeedBd requires bd, which is built from this tree: it needs the Go toolchain
	// and the source, so it is never available inside a Bazel sandbox.
	NeedBd
)

// requireEnv, set to "1" by the lanes meant to exercise these tests, turns a
// missing tool from a skip into a failure.
const requireEnv = "REPLAY_REQUIRE"

// Require makes sure the tools in needs are available. When one is missing it
// fails the test if REPLAY_REQUIRE=1 and skips it otherwise.
//
// BEADS_TEST_SKIP is deliberately not consulted. It skips only the
// container-backed tests, and the dolt CLI still runs in a lane that sets it,
// so honoring it here would let a green run silently skip the harness's tests.
func Require(t testing.TB, needs Need) {
	t.Helper()
	var missing []string
	if needs&NeedDolt != 0 {
		if _, err := doltcli.Path(); err != nil {
			missing = append(missing, "dolt")
		}
	}
	if needs&NeedBd != 0 {
		if bazeltest.IsBazel() {
			missing = append(missing, "bd (it is built from this tree, and the Bazel sandbox holds no source to build it from)")
		} else if _, err := exec.LookPath("go"); err != nil {
			missing = append(missing, "go (bd is built from this tree)")
		}
	}
	if len(missing) == 0 {
		return
	}
	msg := "replay test needs " + strings.Join(missing, ", ") + " on PATH"
	if os.Getenv(requireEnv) == "1" {
		t.Fatalf("%s, and %s=1 says this lane requires it", msg, requireEnv)
	}
	t.Skipf("%s; set %s=1 to fail instead of skipping", msg, requireEnv)
}

// baseEnv is the process environment as it was before any test changed it. Go
// builds run under it, so an isolated HOME never sends the toolchain to a cold
// cache.
var baseEnv = os.Environ()

var goEnv struct {
	once sync.Once
	vars map[string]string
}

// pinnedGoEnv returns the toolchain cache locations as the untouched
// environment resolves them.
func pinnedGoEnv() map[string]string {
	goEnv.once.Do(func() {
		goEnv.vars = map[string]string{}
		cmd := exec.Command("go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH")
		cmd.Env = baseEnv
		out, err := cmd.Output()
		if err != nil {
			return
		}
		_ = json.Unmarshal(out, &goEnv.vars)
	})
	return goEnv.vars
}

// Isolate gives the calling test its own HOME, XDG_CONFIG_HOME and dolt root,
// and a dolt identity inside that root, so nothing depends on the machine the
// test runs on. It also turns off dolt's and bd's telemetry senders. The Go
// toolchain caches keep their real locations.
func Isolate(t testing.TB) {
	t.Helper()
	caches := pinnedGoEnv()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	doltRoot := filepath.Join(root, "dolt-root")
	for _, dir := range []string{home, doltRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	for name, value := range caches {
		if value != "" {
			t.Setenv(name, value)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("DOLT_ROOT_PATH", doltRoot)
	// A test has no business emitting telemetry, and both tools start a detached
	// sender after a command that can write into the dolt root after the test's
	// temporary directory has been removed, which fails that cleanup with
	// "directory not empty". Turn both off: dolt's event flush and bd's metrics.
	t.Setenv("DOLT_DISABLE_EVENT_FLUSH", "1")
	t.Setenv("BD_DISABLE_METRICS", "1")
	for _, kv := range [][2]string{{"user.name", "replay-test"}, {"user.email", "replay-test@example.invalid"}} {
		if _, err := doltcli.Run(context.Background(), root, "config", "--global", "--add", kv[0], kv[1]); err != nil {
			t.Fatalf("setting dolt %s: %v", kv[0], err)
		}
	}
}

// NewDoltDB creates an empty dolt database in a fresh directory, in an
// isolated environment, and returns its path.
func NewDoltDB(t testing.TB, name string) string {
	t.Helper()
	Require(t, NeedDolt)
	Isolate(t)
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	RunDolt(t, dir, "init")
	return dir
}

// RunDolt runs a dolt subcommand in dir and returns its combined output; a
// failure fails the test.
func RunDolt(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := doltcli.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return string(out)
}

// HeadCommit returns the newest commit hash in the dolt database at dir.
func HeadCommit(t testing.TB, dir string) string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dir, "SELECT commit_hash FROM dolt_log ORDER BY commit_order DESC LIMIT 1")
	if err != nil {
		t.Fatalf("reading the head commit of %s: %v", dir, err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		t.Fatalf("dolt_log returned no rows in %s", dir)
	}
	return rows[0][0].Text
}

var bdBuild struct {
	once sync.Once
	dir  string
	path string
	err  error
}

// bdBuildTags is the build tag every bd in this repo is built with. It keeps
// go-mysql-server on Go's own regexp instead of the ICU-backed package, whose C
// headers a macOS runner does not have.
const bdBuildTags = "gms_pure_go"

// BdBin returns the bd binary the tests run: the prebuilt one a job provides
// through BEADS_TEST_BD_BINARY, otherwise one built from this tree with the
// suite's tags. A build happens once per test binary; call Main from TestMain to
// remove it when the tests finish. bd is never taken from PATH.
func BdBin(t testing.TB) string {
	t.Helper()
	Require(t, NeedBd)
	bdBuild.once.Do(func() {
		prebuilt, err := bazeltest.PrebuiltBD()
		if err != nil {
			bdBuild.err = err
			return
		}
		if prebuilt != "" {
			bdBuild.path = prebuilt
			return
		}
		dir, err := os.MkdirTemp("", "replaytest-bd-")
		if err != nil {
			bdBuild.err = err
			return
		}
		bdBuild.dir = dir
		root := bazeltest.RepoRoot(t)
		out := filepath.Join(dir, "bd")
		cmd := exec.Command("go", "build", "-tags", bdBuildTags, "-o", out, "./cmd/bd")
		cmd.Dir = root
		cmd.Env = baseEnv
		if b, err := cmd.CombinedOutput(); err != nil {
			bdBuild.err = fmt.Errorf("building bd from %s: %w\n%s", root, err, b)
			return
		}
		bdBuild.path = out
	})
	if bdBuild.err != nil {
		t.Fatalf("%v", bdBuild.err)
	}
	return bdBuild.path
}

// Main runs a test binary's tests and then removes anything BdBin built. Use
// it as: func TestMain(m *testing.M) { os.Exit(replaytest.Main(m)) }
func Main(m *testing.M) int {
	code := m.Run()
	if bdBuild.dir != "" {
		_ = os.RemoveAll(bdBuild.dir)
	}
	removeOldBd()
	return code
}

// RunBd runs the given bd binary with args in dir, under the sanitized
// environment, and returns its combined output; a failure fails the test.
func RunBd(t testing.TB, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = doltcli.SanitizedEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s (dir=%s) failed: %v\n%s", filepath.Base(bin), strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// InitBdProject creates a bd project named name in a fresh directory, using
// the given bd binary, and returns the project directory.
func InitBdProject(t testing.TB, name, bin string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	RunBd(t, bin, dir, "init", "--non-interactive", "--role=maintainer")
	return dir
}

// DataDir returns the embedded dolt data directory bd init created under the
// project dir. A glob of "*" also matches a sibling lock file, so matches are
// filtered to directories.
func DataDir(t testing.TB, projectDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(projectDir, ".beads", "embeddeddolt", "*"))
	if err != nil {
		t.Fatalf("could not find the embedded dolt data dir under %s: %v", projectDir, err)
	}
	for _, m := range matches {
		if info, statErr := os.Stat(m); statErr == nil && info.IsDir() {
			return m
		}
	}
	t.Fatalf("could not find the embedded dolt data dir under %s: no directory among %v", projectDir, matches)
	return ""
}

// JSONID extracts the id field from the JSON bd create prints. It looks for
// the key rather than decoding the whole document, so it tolerates whatever
// else bd prints around it.
func JSONID(t testing.TB, out string) string {
	t.Helper()
	out = strings.TrimSpace(out)
	const key = `"id"`
	idx := strings.Index(out, key)
	if idx < 0 {
		t.Fatalf("no %q field in bd create output: %s", key, out)
	}
	rest := out[idx+len(key):]
	q1 := strings.Index(rest, `"`)
	if q1 < 0 {
		t.Fatalf("malformed id field in bd create output: %s", out)
	}
	rest = rest[q1+1:]
	q2 := strings.Index(rest, `"`)
	if q2 < 0 {
		t.Fatalf("malformed id field in bd create output: %s", out)
	}
	id := rest[:q2]
	if id == "" {
		t.Fatalf("empty id parsed from bd create output: %s", out)
	}
	return id
}

// ParseCSV parses CSV text into rows, header included.
func ParseCSV(t testing.TB, out string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("parsing CSV: %v\noutput:\n%s", err, out)
	}
	return rows
}
