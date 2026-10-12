package driver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// The control for a prerequisite the checkout lacks. The seeding's oracle is built
// by the older bd, and the older bd is built from a commit that a shallow clone
// does not hold. This runs the tests that need the oracle where that commit is
// missing.

// prereqRequireEnv is the variable that turns a missing prerequisite from a skip
// into a failure in the lanes meant to exercise these tests. The control sets it
// in the child itself, so the child's answer never depends on the parent's.
const prereqRequireEnv = "REPLAY_REQUIRE"

const (
	prereqEnvStripped = "TestB8OverrideEnvStripped"
	prereqReplay      = "TestB8SeededReplay"
	prereqEvents      = "TestB8SeedEventsEmitted"
)

// prereqConsumers are the tests the child runs, in the order the package runs them.
// The first asks the shared flow for itself and checks nothing about how it went;
// the others read it through the helpers that fail a test whose flow could not be
// built. All three use the default variant, so they share one cache entry, which is
// what lets the one that asks first hand its outcome to the others.
var prereqConsumers = []string{prereqEnvStripped, prereqReplay, prereqEvents}

// prereqMode is one answer a lane can give to a missing prerequisite.
type prereqMode struct {
	name     string
	required bool   // whether the child is told that this lane requires the commit
	status   string // how every consumer must end
	text     string // what every consumer must say about why
	exit     int    // the child's exit code
}

var prereqModes = []prereqMode{
	{"not required", false, "SKIP", "set " + prereqRequireEnv + "=1 to fail instead of skipping", 0},
	{"required", true, "FAIL", prereqRequireEnv + "=1 says this lane requires it", 1},
}

// prereqRun is one way of asking the child to run its tests.
type prereqRun struct {
	name     string
	tests    []string
	count    int    // how many passes the child makes over tests
	shuffled bool   // whether the child orders each pass by a shuffle seed
	first    string // the test that must start the first pass, and so asks the shared flow first; "" for any
}

// prereqRuns puts every consumer in the place of the one that asks first and every
// consumer behind every other one: all three in the package's order, each one
// alone, and each ordered pair under a shuffle seed that starts with its first
// test. Every row but the first makes two passes, so each consumer also meets what
// its own earlier pass left in the shared flow.
func prereqRuns() []prereqRun {
	runs := []prereqRun{
		{name: "all three, in the package's order", tests: prereqConsumers, count: 1},
		{name: "all three, in the package's order, twice", tests: prereqConsumers, count: 2},
	}
	for _, x := range prereqConsumers {
		runs = append(runs, prereqRun{name: x + " alone, twice", tests: []string{x}, count: 2})
	}
	for _, x := range prereqConsumers {
		for _, y := range prereqConsumers {
			if x != y {
				runs = append(runs, prereqRun{name: x + " then " + y + ", shuffled, twice", tests: []string{x, y}, count: 2, shuffled: true, first: x})
			}
		}
	}
	return runs
}

// B8.MissingOldBdDecidedByEachConsumer: a checkout that lacks the commit the older
// bd is built from cannot build the seeding's oracle, and each test that needs the
// oracle finds that out for itself. Where nothing requires the history, each of
// them skips and says why; where REPLAY_REQUIRE=1 says the lane does, each of them
// fails and says why. Which of them asked first changes neither: the shared flow is
// built once and handed on to the tests that come after, so a build that stopped
// on a skip must not reach them as a failure that is not theirs.
//
// The shared flow lives as long as its process, so the answer is read from a fresh
// process of this test binary. The parent keeps its own history. The child is
// pointed at an empty repository, which hides the commit from it and from nothing
// else, and gets a REPLAY_REQUIRE of its own, never the parent's.
func TestB8MissingOldBdDecidedByEachConsumer(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	prereqRequireGit(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	scratch := t.TempDir()
	gitDir := prereqEmptyRepo(t, filepath.Join(scratch, "empty.git"))
	tmpDir := filepath.Join(scratch, "tmp")
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", tmpDir, err)
	}
	prereqRequireHidden(t, gitDir)

	for _, mode := range prereqModes {
		extra := []string{"GIT_DIR=" + gitDir, "TMPDIR=" + tmpDir}
		if mode.required {
			extra = append(extra, prereqRequireEnv+"=1")
		}
		for _, run := range prereqRuns() {
			out, code, seed := prereqAsk(t, exe, prereqEnv(extra), run)
			label := mode.name + ", " + run.name
			if run.shuffled {
				label += fmt.Sprintf(" (shuffle seed %d)", seed)
			}
			prereqJudge(t, label, mode, run, out, code)
		}
	}
}

// prereqRequireGit makes sure git is there to build the child's empty history.
// Without it the older bd could not be built either, so it is the same missing
// prerequisite and gets the same answer: a failure in a lane that requires it, a
// skip anywhere else.
func prereqRequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err == nil {
		return
	}
	msg := "the control needs git on PATH to give the child a history without the older bd's commit"
	if os.Getenv(prereqRequireEnv) == "1" {
		t.Fatalf("%s, and %s=1 says this lane requires it", msg, prereqRequireEnv)
	}
	t.Skipf("%s; set %s=1 to fail instead of skipping", msg, prereqRequireEnv)
}

// prereqEmptyRepo creates a bare repository with no objects at dir.
func prereqEmptyRepo(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "init", "-q", "--bare", dir)
	cmd.Env = prereqEnv(nil)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("creating the empty repository %s: %v\n%s", dir, err, out)
	}
	return dir
}

// prereqRequireHidden fails the control if the older bd's commit is still there for
// a git pointed at gitDir. The child would then build the older bd, and nothing it
// reported would be an answer to a missing commit.
func prereqRequireHidden(t *testing.T, gitDir string) {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", replaytest.OldBdSHA+"^{commit}")
	cmd.Env = prereqEnv([]string{"GIT_DIR=" + gitDir})
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		t.Fatalf("git finds commit %s with GIT_DIR=%s, so the child would build the older bd and this control would prove nothing", replaytest.OldBdSHA, gitDir)
	case !errors.As(err, &exit):
		t.Fatalf("asking git for commit %s: %v\n%s", replaytest.OldBdSHA, err, out)
	}
}

// prereqEnv is this process's environment without what would decide the child's
// answer for it: the lane's requirement, the temporary directory, and every git
// variable, the ones that point at a repository or at more objects included.
// extra is added after.
func prereqEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == prereqRequireEnv || name == "TMPDIR" || strings.HasPrefix(name, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// prereqSeedLimit is how many shuffle seeds prereqAsk tries before it gives up on
// finding one that starts a row with the test the row wants first.
const prereqSeedLimit = 100

// prereqAsk runs one row in a fresh process of this test binary and returns what it
// printed, the code it exited with and the shuffle seed it ran under (0 when the
// row is not shuffled).
//
// A seed orders every test of the package, not only the ones the row selects, so a
// seed that starts a row with the consumer it wants today would start it with
// another the day the package gains a test. The row gets the first seed that does
// start with its consumer, the same one every time the package is the same. If none
// does, the last run is handed back, and prereqJudge reports the row as having
// started with the wrong test.
func prereqAsk(t *testing.T, exe string, env []string, run prereqRun) (string, int, int) {
	t.Helper()
	if !run.shuffled {
		out, code := prereqRunChild(t, exe, env, run, "")
		return out, code, 0
	}
	var out string
	var code, seed int
	for seed = 1; seed <= prereqSeedLimit; seed++ {
		out, code = prereqRunChild(t, exe, env, run, strconv.Itoa(seed))
		// A child that ran no test has no order to look for; a second seed would
		// not change that, and the judge reports it.
		if order, _ := prereqRead(out); len(order) == 0 || order[0] == run.first {
			break
		}
	}
	return out, code, min(seed, prereqSeedLimit)
}

// prereqRunChild runs the row's tests once in a fresh process of this test binary
// under the given shuffle seed, or in the package's order when the seed is "", and
// returns what it printed and the code it exited with.
func prereqRunChild(t *testing.T, exe string, env []string, run prereqRun, seed string) (string, int) {
	t.Helper()
	args := []string{
		"-test.run=^(" + strings.Join(run.tests, "|") + ")$",
		"-test.count=" + strconv.Itoa(run.count),
		"-test.v",
		"-test.timeout=2m",
	}
	if seed != "" {
		args = append(args, "-test.shuffle="+seed)
	}
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("running %s %s: %v\n%s", exe, strings.Join(args, " "), err, out)
	return "", 0
}

// prereqOutcome is how one run of one test ended, and what it logged on the way.
type prereqOutcome struct {
	status string // PASS, FAIL or SKIP
	log    string
}

var (
	prereqStart  = regexp.MustCompile(`^=== RUN +(\S+)$`)
	prereqResult = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \(`)
)

// prereqRead reads the output of a verbose test run: the top-level tests in the
// order they started, and every run of each with how it ended.
func prereqRead(out string) ([]string, map[string][]prereqOutcome) {
	var order []string
	got := map[string][]prereqOutcome{}
	var open string
	var log []string
	for _, line := range strings.Split(out, "\n") {
		if m := prereqStart.FindStringSubmatch(line); m != nil && !strings.Contains(m[1], "/") {
			open, log = m[1], nil
			order = append(order, open)
			continue
		}
		if m := prereqResult.FindStringSubmatch(line); m != nil && m[2] == open {
			got[open] = append(got[open], prereqOutcome{status: m[1], log: strings.Join(log, "\n")})
			open, log = "", nil
			continue
		}
		if open != "" {
			log = append(log, line)
		}
	}
	return order, got
}

// prereqJudge holds one run of the child to its mode: it started with the consumer
// the row puts first, every consumer ran as many times as the row asks, and each
// run ended as the mode says, for the reason the older bd gives and not for the
// reason of a flow another test left behind.
func prereqJudge(t *testing.T, label string, mode prereqMode, run prereqRun, out string, code int) {
	t.Helper()
	var problems []string
	if code != mode.exit {
		problems = append(problems, fmt.Sprintf("the child exited %d, want %d", code, mode.exit))
	}
	order, got := prereqRead(out)
	first := ""
	if len(order) > 0 {
		first = order[0]
	}
	if run.first != "" && first != run.first {
		problems = append(problems, fmt.Sprintf("the child started with %q, but this row is there to put %s first", first, run.first))
	}
	for _, name := range run.tests {
		if len(got[name]) != run.count {
			problems = append(problems, fmt.Sprintf("%s ran %d times, want %d", name, len(got[name]), run.count))
		}
		for i, o := range got[name] {
			var why []string
			if o.status != mode.status {
				why = append(why, "ended "+o.status+", want "+mode.status)
			}
			for _, want := range []string{"commit " + replaytest.OldBdSHA + " is not in this checkout", mode.text} {
				if !strings.Contains(o.log, want) {
					why = append(why, fmt.Sprintf("never logged %q", want))
				}
			}
			if strings.Contains(o.log, "could not be built") {
				why = append(why, "reports a flow that another test could not build, which is not a decision of its own")
			}
			if len(why) > 0 {
				problems = append(problems, fmt.Sprintf("%s, run %d: %s", name, i+1, strings.Join(why, "; ")))
			}
		}
	}
	if len(problems) > 0 {
		t.Errorf("%s:\n  %s\nthe child's output:\n%s", label, strings.Join(problems, "\n  "), out)
	}
}
