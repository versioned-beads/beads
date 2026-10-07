package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// ---- the real driver-core, run as a child process ---------------------------------
//
// The run lifecycle is what becomes of a run's directories when the process is
// killed, interrupted, started again or asked to resume, so these tests run the
// command a user runs: the driver-core binary, built from this tree, as a child
// they can kill. The "integration build" it makes is the bd stand-in the other
// fixtures use, handed over by a stand-in for the go tool, so no tree is compiled
// for it; the stand-in for go also records which commit it was asked to build.

// Names of the files a run keeps in its output directory.
const (
	runFile     = "run.json"
	journalFile = "journal.jsonl"
	runsFile    = "replay_runs.jsonl"
	resultsFile = "commit_replay_results.jsonl"
	gapsFile    = "coverage_gaps.jsonl"
	mismatchesF = "mismatches.jsonl"
	summaryFile = "summary.json"
)

// How long a test waits for something a child does. Generous: a child may be
// starting bd, which opens an embedded database, on a busy machine.
const waitLimit = 5 * time.Minute

// b7BaseEnv is the process environment as it was before any test changed it. The
// driver-core build runs under it, so an isolated HOME never sends the toolchain
// to a cold cache.
var b7BaseEnv = os.Environ()

var driverCore struct {
	mu   sync.Mutex
	path string
}

// driverCoreBin builds scripts/driver-core once per test binary, as a user would,
// and returns the binary. It lives with the shared fixtures and goes with them.
func driverCoreBin(t *testing.T) string {
	t.Helper()
	replaytest.Require(t, replaytest.NeedBd)
	driverCore.mu.Lock()
	defer driverCore.mu.Unlock()
	if driverCore.path != "" {
		return driverCore.path
	}
	shared.mu.Lock()
	root := fixtureRoot(t)
	shared.mu.Unlock()
	out := filepath.Join(root, "driver-core")
	cmd := exec.Command("go", "build", "-tags", bdBuildTags, "-o", out, "./scripts/driver-core")
	cmd.Dir = findRepoRoot(t)
	cmd.Env = b7BaseEnv
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building scripts/driver-core: %v\n%s", err, b)
	}
	driverCore.path = out
	return out
}

// cliRig is one driver-core command line over an oracle. The integration
// repository is a throwaway git repo with two commits on main; the work project
// and the output directory do not exist until a run makes them.
type cliRig struct {
	t       *testing.T
	o       *oracleHistory
	bin     string // the driver-core binary
	repo    string // the integration repository
	first   string // its older commit
	second  string // its newer commit, which main starts out at
	stand   string // the bd stand-in the go stand-in delivers as the build
	calls   string // the log of the calls the bd stand-in was asked to make
	built   string // one line for every build the go stand-in made: the commit it was asked to build
	marks   string // where the one-shot marks and the ready files of the stand-ins go
	stubDir string // holds the go stand-in; first on the child's PATH
	tmp     string // the child's TMPDIR
	workDir string
	outDir  string
	oracle  string // the oracle's data directory, as --oracle-data-dir gives it
}

func newCLIRig(t *testing.T, o *oracleHistory) *cliRig {
	t.Helper()
	replaytest.Isolate(t)
	bin := driverCoreBin(t)
	repo, first, second := newTestGitRepo(t)
	root := t.TempDir()
	r := &cliRig{
		t: t, o: o, bin: bin, repo: repo, first: first, second: second,
		built:   filepath.Join(root, "built-from"),
		marks:   filepath.Join(root, "marks"),
		stubDir: filepath.Join(root, "stub-bin"),
		tmp:     filepath.Join(root, "tmp"),
		workDir: filepath.Join(root, "work"),
		outDir:  filepath.Join(root, "out"),
		oracle:  o.data,
	}
	for _, dir := range []string{r.marks, r.stubDir, r.tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	return r
}

// mark is a path in the rig's own directory for a stand-in to create.
func (r *cliRig) mark(name string) string { return filepath.Join(r.marks, name) }

// install writes the bd stand-in (see bdStandInAfter for body and after) and the
// go stand-in that delivers it as the build. goHook is shell the go stand-in runs
// first, in the temporary worktree the driver asked it to build in.
func (r *cliRig) install(body, after, goHook string) {
	r.t.Helper()
	r.stand, r.calls = bdStandInAfter(r.t, r.o.bin, body, after)
	script := "#!/bin/sh\n" +
		"out=\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = -o ]; then out=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"git rev-parse HEAD >> " + shellQuote(r.built) + "\n" +
		goHook + "\n" +
		"cp " + shellQuote(r.stand) + " \"$out\"\n"
	if err := os.WriteFile(filepath.Join(r.stubDir, "go"), []byte(script), 0o755); err != nil { // #nosec G306 -- a test stand-in must be executable
		r.t.Fatalf("writing the go stand-in: %v", err)
	}
}

// args is the command line: the rig's directories and oracle, then extra. A flag
// in extra that the rig already gave replaces it.
func (r *cliRig) args(extra ...string) []string {
	return append([]string{
		"--integration-repo", r.repo,
		"--integration-ref", "main",
		"--oracle-data-dir", r.oracle,
		"--work-dir", r.workDir,
		"--out-dir", r.outDir,
	}, extra...)
}

func (r *cliRig) cmd(extra ...string) *exec.Cmd {
	cmd := exec.Command(r.bin, r.args(extra...)...) // #nosec G204 -- the binary is the one this test built
	cmd.Env = append(os.Environ(),
		"PATH="+r.stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+r.tmp,
	)
	// A stand-in that outlives the driver must not keep the test waiting on the
	// output it shares with it.
	cmd.WaitDelay = 15 * time.Second
	return cmd
}

// cliResult is how a command line ended: everything it printed and its exit
// status, which is -1 when a signal ended it.
type cliResult struct {
	out  string
	code int
}

func (r *cliRig) run(extra ...string) cliResult {
	r.t.Helper()
	cmd := r.cmd(extra...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	return r.finish(cmd, cmd.Run(), &buf)
}

func (r *cliRig) finish(cmd *exec.Cmd, err error, out *bytes.Buffer) cliResult {
	r.t.Helper()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
		r.t.Fatalf("running driver-core: %v", err)
	}
	return cliResult{out: out.String(), code: cmd.ProcessState.ExitCode()}
}

// started is a command line that is running, which a test can signal.
type started struct {
	r    *cliRig
	cmd  *exec.Cmd
	out  *bytes.Buffer
	done chan error
}

func (r *cliRig) start(extra ...string) *started {
	r.t.Helper()
	cmd := r.cmd(extra...)
	s := &started{r: r, cmd: cmd, out: &bytes.Buffer{}, done: make(chan error, 1)}
	cmd.Stdout, cmd.Stderr = s.out, s.out
	if err := cmd.Start(); err != nil {
		r.t.Fatalf("starting driver-core: %v", err)
	}
	go func() { s.done <- cmd.Wait() }()
	r.t.Cleanup(func() { _ = cmd.Process.Kill() })
	return s
}

// waitForFile returns once the file exists. It fails the test, and stops the
// command line, if that does not happen while the command line runs.
func (s *started) waitForFile(path string) {
	s.r.t.Helper()
	deadline := time.After(waitLimit)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case err := <-s.done:
			s.done <- err
			s.r.t.Fatalf("driver-core ended before %s appeared:\n%s", filepath.Base(path), s.out.String())
		case <-deadline:
			_ = s.cmd.Process.Kill()
			<-s.done
			s.r.t.Fatalf("%s did not appear in %v:\n%s", filepath.Base(path), waitLimit, s.out.String())
		case <-tick.C:
		}
	}
}

func (s *started) interrupt() {
	s.r.t.Helper()
	if err := s.cmd.Process.Signal(os.Interrupt); err != nil {
		s.r.t.Fatalf("interrupting driver-core: %v", err)
	}
}

// wait returns how the command line ended. It fails the test, and stops the
// command line, if that takes more than limit.
func (s *started) wait(limit time.Duration) cliResult {
	s.r.t.Helper()
	select {
	case err := <-s.done:
		return s.r.finish(s.cmd, err, s.out)
	case <-time.After(limit):
		_ = s.cmd.Process.Kill()
		<-s.done
		s.r.t.Fatalf("driver-core was still running %v after it was interrupted:\n%s", limit, s.out.String())
		return cliResult{}
	}
}

// runIDLine is the line driver-core prints when a run ends, which names the run.
var runIDLine = regexp.MustCompile(`replay run (\S+): status=(\S+)`)

// ranAs returns the run id and status of the result line a command line printed.
func ranAs(t *testing.T, res cliResult) (id, status string) {
	t.Helper()
	m := runIDLine.FindStringSubmatch(res.out)
	if m == nil {
		t.Fatalf("driver-core did not print a result line (exit %d):\n%s", res.code, res.out)
	}
	return m[1], m[2]
}

func (r *cliRig) outPath(name string) string { return filepath.Join(r.outDir, name) }

// runID is the id run.json names, or fallback when there is no run.json to say.
func (r *cliRig) runID(fallback string) string {
	data, err := os.ReadFile(r.outPath(runFile))
	if err != nil {
		return fallback
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return fallback
	}
	if id := memberString(m, "id"); id != "" {
		return id
	}
	return fallback
}

// runKilled runs the command line, which a stand-in is to kill, and returns the
// id of the run it left behind.
func (r *cliRig) runKilled(extra ...string) string {
	r.t.Helper()
	res := r.run(extra...)
	if res.code >= 0 {
		r.t.Fatalf("driver-core was to be killed by the stand-in, and ended on its own with exit %d:\n%s", res.code, res.out)
	}
	id := r.runID("")
	if id == "" {
		r.t.Fatalf("a killed run left no %s to name it:\n%s", runFile, res.out)
	}
	return id
}

// workHead is the head of the work project's database, empty before there is one.
func (r *cliRig) workHead() string {
	dataDir, err := findEmbeddedDoltDir(r.workDir)
	if err != nil {
		return ""
	}
	return replaytest.HeadCommit(r.t, dataDir)
}

// workCount runs a SELECT COUNT(*) against the work project's database.
func (r *cliRig) workCount(query string) int {
	r.t.Helper()
	return countIn(r.t, replaytest.DataDir(r.t, r.workDir), query)
}

func countIn(t *testing.T, dataDir, query string) int {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), dataDir, query)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: rows=%v err=%v", query, rows, err)
	}
	var n int
	if _, err := fmt.Sscanf(rows[0][0].Text, "%d", &n); err != nil {
		t.Fatalf("%s: %q is not a count", query, rows[0][0].Text)
	}
	return n
}

// git runs git in the integration repository, as someone who may commit.
func (r *cliRig) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=driver-core-test", "GIT_AUTHOR_EMAIL=driver-core-test@example.com",
		"GIT_COMMITTER_NAME=driver-core-test", "GIT_COMMITTER_EMAIL=driver-core-test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// worktrees are the paths git lists as working trees of the integration
// repository: its own, and any the driver left registered.
func (r *cliRig) worktrees() []string {
	r.t.Helper()
	var paths []string
	for _, line := range strings.Split(r.git("worktree", "list", "--porcelain"), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// leftInTmp are the entries driver-core made in the child's TMPDIR and left.
func (r *cliRig) leftInTmp() []string {
	r.t.Helper()
	entries, err := os.ReadDir(r.tmp)
	if err != nil {
		r.t.Fatalf("reading the child's TMPDIR: %v", err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "driver-core-") {
			left = append(left, e.Name())
		}
	}
	return left
}

// ---- stand-in snippets -------------------------------------------------------------

// onCall is shell that runs sh only for a bd call whose arguments match the glob.
// The glob is a shell pattern over the arguments joined by spaces; a space in it
// is matched literally.
func onCall(glob, sh string) string {
	return "case \"$*\" in " + strings.ReplaceAll(glob, " ", `\ `) + ")\n" + sh + "\n;;\nesac"
}

// once is shell that runs sh the first time only; the mark file remembers, so a
// run that resumes through the same stand-in passes it.
func once(mark, sh string) string {
	return "if [ ! -e " + shellQuote(mark) + " ]; then : > " + shellQuote(mark) + "\n" + sh + "\nfi"
}

// killDriver is shell for a stand-in: it kills its parent, the driver.
const killDriver = "kill -9 $PPID"

// hold is shell that tells the test it has been reached and then waits, as a
// build or a bd call that is taking long would. The wait outlasts the test's, and
// ends by itself if the test is gone.
func hold(ready string) string {
	return ": > " + shellQuote(ready) + "\nexec sleep 120"
}

// copyOutDir is shell that copies the files of dir into into, to see what a run
// had written by the time it was reached.
func copyOutDir(dir, into string) string {
	return "mkdir -p " + shellQuote(into) + "\ncp " + shellQuote(dir) + "/* " + shellQuote(into) + "/ 2>/dev/null || true"
}

// ---- reading what a run left behind ------------------------------------------------

// jsonRows reads a JSON-Lines file as objects; a file that is not there has none.
func jsonRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	return readJSONLTyped[map[string]any](t, path)
}

// readSummary reads a run's summary.json as the type the driver writes it from.
func readSummary(t *testing.T, path string) Summary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the summary: %v", err)
	}
	var s Summary
	if err := unmarshalStrict(data, &s); err != nil {
		t.Fatalf("the summary at %s: %v\n%s", path, err, data)
	}
	return s
}

// emptyOrAbsent is whether nothing is in dir: it is empty, or there is no dir.
func emptyOrAbsent(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return len(entries) == 0
}

// jsonObject reads a file holding one JSON object.
func jsonObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Base(path), err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s is not a JSON object: %v\n%s", filepath.Base(path), err, data)
	}
	return m
}

func memberString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// memberInt is the integer a member holds, and whether it holds one.
func memberInt(m map[string]any, key string) (int, bool) {
	f, ok := m[key].(float64)
	return int(f), ok
}

// rowsFor are the rows that name the run id, in order.
func rowsFor(rows []map[string]any, id string) []map[string]any {
	var out []map[string]any
	for _, row := range rows {
		if memberString(row, "id") == id || memberString(row, "run_id") == id {
			out = append(out, row)
		}
	}
	return out
}

// journal is what the run's journal says, one object per line, oldest first.
// Every entry has an "event", "started" or "finished", and the "step" it is
// about. A "started" entry also has "from" and "to", the commits the step goes
// between, and "work_head_before", the head of the work database before the step.
func (r *cliRig) journal() []map[string]any {
	r.t.Helper()
	return jsonRows(r.t, r.outPath(journalFile))
}

// unfinished is the step the journal's last entry started and never finished: its
// number, the head of the work database before it, and the commit it goes to. It
// fails the test if the journal does not end that way.
func (r *cliRig) unfinished() (step int, headBefore, to string) {
	r.t.Helper()
	entries := r.journal()
	if len(entries) == 0 {
		r.t.Fatalf("the journal is empty or missing; want a step that was started and not finished")
	}
	last := entries[len(entries)-1]
	if memberString(last, "event") != "started" {
		r.t.Fatalf("the journal's last entry is %v; want a step that was started and not finished", last)
	}
	n, ok := memberInt(last, "step")
	if !ok {
		r.t.Fatalf("the journal's last entry has no step: %v", last)
	}
	return n, memberString(last, "work_head_before"), memberString(last, "to")
}

// killAfterApply makes the bd stand-in kill the driver, once, right after the
// real bd has carried out the call that matches glob. The kill lands with the
// step's work in the database and nothing of it recorded, which is the state a
// rewind has to undo; a kill before bd ran would leave nothing to rewind.
func (r *cliRig) killAfterApply(glob string) {
	r.t.Helper()
	r.install("", onCall(glob, once(r.mark("killed"), killDriver)), "")
}

// dirSnapshot is every file under dir, by path relative to it, with its content.
func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304 -- walking a test directory
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		snap[rel] = string(data)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return snap
}

// changedFiles names, in order, the files that differ between two snapshots of a
// directory: those in only one of them and those whose content is not the same.
// A work project holds a database, so a failure message names files and leaves
// their content out.
func changedFiles(before, after map[string]string) []string {
	var changed []string
	for name, b := range before {
		switch a, ok := after[name]; {
		case !ok:
			changed = append(changed, "removed "+name)
		case a != b:
			changed = append(changed, "changed "+name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			changed = append(changed, "added "+name)
		}
	}
	sort.Strings(changed)
	return changed
}

// firstOf is at most limit of names, with how many were left out.
func firstOf(names []string, limit int) string {
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}

// rigState is what a refusal has to leave alone: the files of the run's
// directory, the head of the work project's database and the calls bd was asked
// to make.
type rigState struct {
	out   map[string]string
	head  string
	calls []string
}

func (r *cliRig) state() rigState {
	r.t.Helper()
	return rigState{out: dirSnapshot(r.t, r.outDir), head: r.workHead(), calls: standInCalls(r.t, r.calls)}
}

// requireUntouched fails the test for everything that changed since before.
func (r *cliRig) requireUntouched(what string, before rigState) {
	r.t.Helper()
	after := r.state()
	names := map[string]bool{}
	for name := range before.out {
		names[name] = true
	}
	for name := range after.out {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		b, inBefore := before.out[name]
		a, inAfter := after.out[name]
		switch {
		case inBefore && !inAfter:
			r.t.Errorf("%s removed %s from the run's directory", what, name)
		case !inBefore && inAfter:
			r.t.Errorf("%s added %s to the run's directory:\n%s", what, name, a)
		case a != b:
			r.t.Errorf("%s changed %s:\nbefore:\n%s\nafter:\n%s", what, name, b, a)
		}
	}
	if after.head != before.head {
		r.t.Errorf("%s moved the work project's head from %s to %s", what, before.head, after.head)
	}
	if len(after.calls) != len(before.calls) {
		r.t.Errorf("%s asked bd for %d more calls: %v", what, len(after.calls)-len(before.calls), after.calls[len(before.calls):])
	}
}

// refused fails the test unless the command line ended by refusing: it exited on
// its own, with an error, and said each of want.
func (r *cliRig) refused(what string, res cliResult, want ...string) {
	r.t.Helper()
	switch {
	case res.code == 0:
		r.t.Errorf("%s: driver-core went ahead (exit 0):\n%s", what, res.out)
		return
	case res.code < 0:
		r.t.Errorf("%s: a signal ended driver-core, which did not refuse:\n%s", what, res.out)
		return
	}
	for _, w := range want {
		if !strings.Contains(res.out, w) {
			r.t.Errorf("%s: the refusal does not say %q:\n%s", what, w, res.out)
		}
	}
}

// ---- comparing a run with a clean one ----------------------------------------------

// outcomeFiles are the files a run's results are in. replay_runs.jsonl and
// run.json are left out: they carry the times the run took.
var outcomeFiles = []string{resultsFile, gapsFile, mismatchesF, summaryFile}

// outcome is what a run left in those files, with its run id taken out so that
// two runs can be compared.
func outcome(t *testing.T, dir, runID string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, name := range outcomeFiles {
		data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- a fixed file name in a test directory
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("reading %s: %v", name, err)
		}
		got[name] = strings.ReplaceAll(string(data), runID, "<run>")
	}
	return got
}

// requireSameOutcome fails the test for every file in which got is not want.
func requireSameOutcome(t *testing.T, what string, want, got map[string]string) {
	t.Helper()
	for _, name := range outcomeFiles {
		if want[name] != got[name] {
			t.Errorf("%s: %s differs from the clean run's.\nclean run:\n%s\nthis run:\n%s", what, name, want[name], got[name])
		}
	}
}

// baseline is what a run over an oracle leaves when nothing goes wrong.
type baseline struct {
	files   map[string]string
	summary Summary
	results []CommitReplayResult
	issues  int
}

var baselines struct {
	mu sync.Mutex
	m  map[string]*baseline
}

// cleanBaseline runs o once, start to finish, through a bd stand-in with the
// given body, and keeps what it left. It is the run the others are compared with,
// so a run that did not complete fails the test that asked for it. The name stands
// for the oracle and the body together.
func cleanBaseline(t *testing.T, name string, o *oracleHistory, body string) *baseline {
	t.Helper()
	baselines.mu.Lock()
	defer baselines.mu.Unlock()
	if b, ok := baselines.m[name]; ok {
		return b
	}
	r := newCLIRig(t, o)
	r.install(body, "", "")
	res := r.run()
	id, status := ranAs(t, res)
	if res.code != 0 || status != "completed" {
		t.Fatalf("the clean run over %s ended with exit %d, status %s:\n%s", name, res.code, status, res.out)
	}
	b := &baseline{
		files:   outcome(t, r.outDir, id),
		summary: readSummary(t, r.outPath(summaryFile)),
		results: readCommitReplayResults(t, r.outDir),
		issues:  r.workCount("SELECT COUNT(*) FROM issues"),
	}
	if baselines.m == nil {
		baselines.m = map[string]*baseline{}
	}
	baselines.m[name] = b
	return b
}

// ---- oracles -----------------------------------------------------------------------

// lifecycleOracle is the history the kill and resume tests replay: an issue, an
// issue created with an edge to it, and an update of each. The kill lands in the
// second step, a create, which is the one a redo would duplicate.
func lifecycleOracle(t *testing.T) *oracleHistory {
	return sharedOracle(t, "lifecycle", func(o *oracleHistory) {
		a := o.create("Alpha")
		b := o.create("Beta", "--deps", a)
		o.run("update", a, "--description", "the first issue, once it has a dependent")
		o.run("update", b, "--description", "the second issue, once it is described")
	})
}

// restoreOracle is the history a resumed run has to know the whole of. Its base
// is a schema change only the oracle has made to the edge table. After it, from
// the first step: create Alpha; create Beta with an edge to Alpha, the step that
// shows the schema skew; change Alpha's owner straight in the database, which
// bd cannot do, so Alpha is quarantined; update Beta, which the bd stand-in of
// refuseUpdate refuses, so Beta is quarantined too, the other way a step
// quarantines; add a label to Beta, a table the replay does not cover; bump a
// counter, a table bd rewrites on its own, in a step that touches no issue and
// so leaves no row, as the label step leaves none; create Gamma, which is where
// the run is killed; update Alpha and update Beta, both quarantined; update Gamma.
func restoreOracle(t *testing.T) *oracleHistory {
	return sharedOracle(t, "restore", func(o *oracleHistory) {
		o.sqlStep("schema: a column only the oracle has", "ALTER TABLE dependencies ADD COLUMN zz_extra INT")
		a := o.create("Alpha")
		b := o.create("Beta", "--deps", a)
		o.sqlStep("test: a direct owner change", "UPDATE issues SET owner = 'someone-else' WHERE id = '"+a+"'")
		o.run("update", b, "--description", refusedUpdate)
		o.run("label", "add", b, "zz-label")
		o.sqlStep("test: a counter bump", "INSERT INTO issue_counter (prefix, last_id) VALUES ('zz', 1)")
		c := o.create("Gamma")
		o.run("update", a, "--description", "after the quarantine")
		o.run("update", b, "--description", "after the refusal")
		o.run("update", c, "--description", "the last step")
	})
}

// refusedUpdate is the description restoreOracle sets in the update that
// refuseUpdate makes bd refuse.
const refusedUpdate = "an update bd refuses"

// refuseUpdate is a bd stand-in body that refuses the update of refusedUpdate:
// it says so and exits with an error, having done nothing, as a bd that rejects a
// step does.
var refuseUpdate = onCall("update*"+refusedUpdate+"*", "echo 'bd: this update is refused' >&2\nexit 1")

// movedOracle copies o's project and adds a commit to the copy: an oracle that
// has moved on. It returns the copy's data directory.
func movedOracle(t *testing.T, o *oracleHistory) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "oracle-moved")
	copyProject(t, o.dir, dst)
	moved := adoptOracle(t, o.bin, dst)
	moved.sqlStep("test: the oracle moved on", "UPDATE issues SET description = 'moved on' WHERE id = '"+o.ids[0]+"'")
	return moved.data
}
