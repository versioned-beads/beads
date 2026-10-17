package replaytest_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// recorder stands in for *testing.T so a test can see whether a helper fails
// or skips. Embedding testing.TB satisfies its unexported method; only the
// methods the helpers call are implemented, so a helper that reaches for
// anything else panics loudly instead of passing quietly.
type recorder struct {
	testing.TB
	failed, skipped bool
	msg             string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failed, r.msg = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped, r.msg = true, fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// observe runs fn against a recorder on its own goroutine, because Fatalf and
// Skipf end the goroutine they are called on, as the real ones do.
func observe(fn func(testing.TB)) *recorder {
	r := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	return r
}

// withoutDolt makes dolt unfindable for the rest of the test.
func withoutDolt(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// Require fails, and does not skip, when the lane declares the tool required
// and it is absent.
func TestB1RequireFailsWhenRequiredToolIsAbsent(t *testing.T) {
	t.Setenv("REPLAY_REQUIRE", "1")
	withoutDolt(t)
	r := observe(func(tb testing.TB) { replaytest.Require(tb, replaytest.NeedDolt) })
	if !r.failed || r.skipped {
		t.Fatalf("Require with REPLAY_REQUIRE=1 and no dolt: failed=%v skipped=%v, want a failure", r.failed, r.skipped)
	}
}

// Without the lane flag a missing tool is a skip, so a developer machine with
// no dolt still runs everything else.
func TestB1RequireSkipsWhenToolIsAbsentAndNotRequired(t *testing.T) {
	t.Setenv("REPLAY_REQUIRE", "")
	withoutDolt(t)
	r := observe(func(tb testing.TB) { replaytest.Require(tb, replaytest.NeedDolt) })
	if !r.skipped || r.failed {
		t.Fatalf("Require without REPLAY_REQUIRE and no dolt: failed=%v skipped=%v, want a skip", r.failed, r.skipped)
	}
}

// BEADS_TEST_SKIP=dolt skips only the container-backed tests; the installed
// dolt binary still runs in that lane, so a Require-guarded test must execute.
func TestB1RequireIgnoresBeadsTestSkip(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt)
	t.Setenv("BEADS_TEST_SKIP", "dolt")
	t.Setenv("REPLAY_REQUIRE", "1")

	executed := false
	r := observe(func(tb testing.TB) {
		replaytest.Require(tb, replaytest.NeedDolt)
		executed = true
	})
	if r.skipped || r.failed || !executed {
		t.Fatalf("Require under BEADS_TEST_SKIP=dolt with dolt present: failed=%v skipped=%v executed=%v (%s), want the guarded test to run",
			r.failed, r.skipped, executed, r.msg)
	}
}

// BEADS_TEST_SKIP does not turn an absent tool into a green run either: with
// the lane flag set, the absence still fails.
func TestB1RequireBeadsTestSkipDoesNotMaskAbsentTool(t *testing.T) {
	t.Setenv("BEADS_TEST_SKIP", "dolt")
	t.Setenv("REPLAY_REQUIRE", "1")
	withoutDolt(t)
	r := observe(func(tb testing.TB) { replaytest.Require(tb, replaytest.NeedDolt) })
	if !r.failed || r.skipped {
		t.Fatalf("Require under BEADS_TEST_SKIP=dolt with REPLAY_REQUIRE=1 and no dolt: failed=%v skipped=%v, want a failure", r.failed, r.skipped)
	}
}

// B1.HermeticFixture, first half: a fixture test passes under an empty HOME
// with no dolt root of its own, because the fixture provides its own identity.
func TestB1HermeticFixturePassesUnderEmptyHome(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt)
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("DOLT_ROOT_PATH", "")
	if err := os.Unsetenv("DOLT_ROOT_PATH"); err != nil {
		t.Fatalf("unsetting DOLT_ROOT_PATH: %v", err)
	}

	dir := replaytest.NewDoltDB(t, "fixture")
	replaytest.RunDolt(t, dir, "sql", "-q", "CREATE TABLE t (id INT PRIMARY KEY)")
	replaytest.RunDolt(t, dir, "add", "-A")
	replaytest.RunDolt(t, dir, "commit", "-m", "create t")
	if head := replaytest.HeadCommit(t, dir); head == "" {
		t.Fatal("HeadCommit returned an empty hash")
	}

	entries, err := os.ReadDir(emptyHome)
	if err != nil {
		t.Fatalf("reading the empty HOME: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the fixture wrote into the ambient HOME: %d entries", len(entries))
	}
}

// B1.HermeticFixture, second half: with REPLAY_REQUIRE=1 and dolt absent the
// fixture fails, not skips.
func TestB1HermeticFixtureFailsNotSkipsWhenDoltIsAbsent(t *testing.T) {
	t.Setenv("REPLAY_REQUIRE", "1")
	withoutDolt(t)
	r := observe(func(tb testing.TB) { replaytest.NewDoltDB(tb, "fixture") })
	if !r.failed || r.skipped {
		t.Fatalf("NewDoltDB with REPLAY_REQUIRE=1 and no dolt: failed=%v skipped=%v, want a failure", r.failed, r.skipped)
	}
}

// dolt and bd each start a detached telemetry sender after a command. It can
// write into the isolated dolt root after the test's temporary directory has
// been removed, which fails that cleanup ("directory not empty"), so Isolate
// turns both off, and the switches must reach the children the harness starts.
func TestIsolateTurnsOffTelemetryForChildren(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt)
	replaytest.Isolate(t)
	env := doltcli.SanitizedEnv(os.Environ())
	for _, name := range []string{"DOLT_DISABLE_EVENT_FLUSH", "BD_DISABLE_METRICS"} {
		found := false
		for _, kv := range env {
			if kv == name+"=1" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s=1 does not reach a child process the harness starts", name)
		}
	}
}

// BdBin answers once per process, so the tests below ask it in a fresh copy of
// this test binary and read back what happened. The child has a stand-in for go
// ahead of the real one on PATH, which logs its arguments instead of building.
const freshProcessOut = "REPLAYTEST_FRESH_PROCESS_OUT"

// TestBdBinInFreshProcess is the child half of runBdBin; run on its own it does
// nothing.
func TestBdBinInFreshProcess(t *testing.T) {
	out := os.Getenv(freshProcessOut)
	if out == "" {
		return
	}
	bin := replaytest.BdBin(t)
	if err := os.WriteFile(out, []byte(bin), 0o644); err != nil {
		t.Fatalf("recording the path BdBin returned: %v", err)
	}
}

// bdBinRun is what a fresh process saw when it asked BdBin for a binary.
type bdBinRun struct {
	returned string   // the path BdBin returned; empty when it failed
	goCalls  []string // the arguments of each go invocation, one line each
	output   string   // the child's combined output
	err      error    // the child's exit error; nil when BdBin returned
}

// runBdBin asks BdBin for a binary in a fresh process. prebuilt is what the
// child sees in BEADS_TEST_BD_BINARY; empty leaves the variable unset, whatever
// the lane this test itself runs in has exported.
func runBdBin(t *testing.T, prebuilt string) bdBinRun {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	scratch := t.TempDir()
	standIn := filepath.Join(scratch, "stand-in")
	if err := os.MkdirAll(standIn, 0o755); err != nil {
		t.Fatalf("creating %s: %v", standIn, err)
	}
	goLog := filepath.Join(scratch, "go-calls.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + goLog + "'\n"
	if err := os.WriteFile(filepath.Join(standIn, "go"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the go stand-in: %v", err)
	}
	returned := filepath.Join(scratch, "returned")

	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BEADS_TEST_BD_BINARY=") {
			env = append(env, kv)
		}
	}
	// TMPDIR keeps whatever the child creates inside this test's own directory.
	env = append(env,
		"PATH="+standIn+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+scratch,
		freshProcessOut+"="+returned,
	)
	if prebuilt != "" {
		env = append(env, "BEADS_TEST_BD_BINARY="+prebuilt)
	}
	cmd := exec.Command(exe, "-test.run=^TestBdBinInFreshProcess$", "-test.count=1")
	cmd.Env = env
	combined, runErr := cmd.CombinedOutput()

	run := bdBinRun{output: string(combined), err: runErr}
	if b, err := os.ReadFile(returned); err == nil {
		run.returned = string(b)
	}
	if b, err := os.ReadFile(goLog); err == nil {
		run.goCalls = strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	return run
}

// The bd the harness builds from this tree is built the way the rest of the
// suite builds its binaries. The default build links the ICU-backed regex
// package, which needs C headers a macOS runner does not have and a Linux
// runner does, so only this pin notices the pure-Go tag going missing.
func TestBdBinBuildsWithTheSuiteTags(t *testing.T) {
	replaytest.Require(t, replaytest.NeedBd)
	run := runBdBin(t, "")
	if run.err != nil {
		t.Fatalf("BdBin in a fresh process: %v\n%s", run.err, run.output)
	}
	if len(run.goCalls) != 1 {
		t.Fatalf("go was invoked %d times, want exactly one build: %q", len(run.goCalls), run.goCalls)
	}
	call := " " + run.goCalls[0] + " "
	if !strings.HasPrefix(call, " build ") || !strings.HasSuffix(call, " ./cmd/bd ") {
		t.Errorf("go invocation = %q, want a build of ./cmd/bd", run.goCalls[0])
	}
	if !strings.Contains(call, " -tags gms_pure_go ") {
		t.Errorf("go invocation = %q, want it to carry -tags gms_pure_go", run.goCalls[0])
	}
}

// A job that hands the suite a prebuilt bd gets that bd back, with no build.
func TestBdBinUsesThePrebuiltBinaryTheJobProvides(t *testing.T) {
	replaytest.Require(t, replaytest.NeedBd)
	prebuilt := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(prebuilt, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing the prebuilt stand-in: %v", err)
	}
	run := runBdBin(t, prebuilt)
	if run.err != nil {
		t.Fatalf("BdBin in a fresh process: %v\n%s", run.err, run.output)
	}
	if run.returned != prebuilt {
		t.Errorf("BdBin returned %q, want the prebuilt %q", run.returned, prebuilt)
	}
	if len(run.goCalls) != 0 {
		t.Errorf("go was invoked with a prebuilt bd provided, want no build: %q", run.goCalls)
	}
}

// A prebuilt bd that cannot be used is a broken job, not a cue to build:
// falling back would hide it behind the very build a runner may not be able to
// do.
func TestBdBinFailsOnAnUnusablePrebuiltBinaryInsteadOfBuilding(t *testing.T) {
	replaytest.Require(t, replaytest.NeedBd)
	missing := filepath.Join(t.TempDir(), "no-such-bd")
	run := runBdBin(t, missing)
	if run.err == nil {
		t.Errorf("BdBin with BEADS_TEST_BD_BINARY=%s returned %q, want a failure", missing, run.returned)
	}
	if !strings.Contains(run.output, "BEADS_TEST_BD_BINARY") {
		t.Errorf("the failure does not name BEADS_TEST_BD_BINARY:\n%s", run.output)
	}
	if len(run.goCalls) != 0 {
		t.Errorf("go was invoked after the prebuilt failed, want no build: %q", run.goCalls)
	}
}

// standInGo writes an executable named go into dir and returns its path. GoBin
// only resolves a path, so the stand-in is never run.
func standInGo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	path := filepath.Join(dir, "go")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("writing the go stand-in: %v", err)
	}
	return path
}

// Under Bazel the go a fixture builds with is the runfile BEADS_TEST_GO names,
// resolved through the runfiles tree like any other declared data.
func TestGoBinUnderBazelIsTheDeclaredRunfile(t *testing.T) {
	runfiles := t.TempDir()
	want := standInGo(t, filepath.Join(runfiles, "go_sdk", "bin"))
	t.Setenv("TEST_SRCDIR", runfiles)
	t.Setenv("RUNFILES_DIR", runfiles)
	t.Setenv("BEADS_TEST_GO", "go_sdk/bin/go")

	got, err := replaytest.GoBin()
	if err != nil {
		t.Fatalf("GoBin under Bazel with BEADS_TEST_GO declared: %v", err)
	}
	if got != want {
		t.Errorf("GoBin = %q, want the runfile %q", got, want)
	}
}

// The go on PATH is never used under Bazel: a go_test that did not declare the
// SDK fails, however well a go on PATH would have served.
func TestGoBinUnderBazelNeverFallsBackToPATH(t *testing.T) {
	onPath := t.TempDir()
	standInGo(t, onPath)
	t.Setenv("PATH", onPath)
	t.Setenv("TEST_SRCDIR", t.TempDir())
	t.Setenv("RUNFILES_DIR", "")
	t.Setenv("BEADS_TEST_GO", "")

	got, err := replaytest.GoBin()
	if err == nil {
		t.Fatalf("GoBin under Bazel without BEADS_TEST_GO = %q, want an error", got)
	}
	if !strings.Contains(err.Error(), "BEADS_TEST_GO") {
		t.Errorf("the error does not name BEADS_TEST_GO: %v", err)
	}
}

// Anywhere else it is the go on PATH, and BEADS_TEST_GO, which only a go_test
// sets, is not consulted.
func TestGoBinOutsideBazelIsTheGoOnPATH(t *testing.T) {
	onPath := t.TempDir()
	want := standInGo(t, onPath)
	t.Setenv("PATH", onPath)
	t.Setenv("TEST_SRCDIR", "")
	t.Setenv("BEADS_TEST_GO", filepath.Join(t.TempDir(), "no-such-go"))

	got, err := replaytest.GoBin()
	if err != nil {
		t.Fatalf("GoBin outside Bazel with a go on PATH: %v", err)
	}
	if got != want {
		t.Errorf("GoBin = %q, want the go on PATH %q", got, want)
	}
}

// With no go on PATH it is an error, not an empty command to run.
func TestGoBinOutsideBazelFailsWithoutGoOnPATH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TEST_SRCDIR", "")

	if got, err := replaytest.GoBin(); err == nil {
		t.Fatalf("GoBin with no go on PATH = %q, want an error", got)
	}
}
