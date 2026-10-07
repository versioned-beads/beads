package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// ---- small JSON helpers -------------------------------------------------------

// jsonKeys returns the sorted member names of v's JSON object form.
func jsonKeys(v any) ([]string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// unmarshalStrict decodes data into v and refuses a member v does not have.
func unmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ---- a bd project that is copied, not initialised, for every fixture ----------
//
// bd init applies the whole schema, which is the slowest thing a fixture does,
// so it happens once per test binary and each oracle and work clone is a copy.
// The fixtures here are built in the isolated environment of the first test that
// needs them; a dolt database opens the same under any other.

var shared struct {
	mu       sync.Mutex
	root     string
	template string
	built    map[string]*oracleHistory
}

// removeSharedFixtures deletes everything the shared fixtures built. TestMain
// calls it once every test has run.
func removeSharedFixtures() {
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.root != "" {
		_ = os.RemoveAll(shared.root)
	}
}

// fixtureRoot is the directory the shared fixtures live in.
func fixtureRoot(t testing.TB) string {
	t.Helper()
	if shared.root == "" {
		root, err := os.MkdirTemp("", "driver-fixtures-")
		if err != nil {
			t.Fatalf("creating the fixture root: %v", err)
		}
		shared.root = root
	}
	return shared.root
}

// templateProject returns a freshly initialised bd project, built once.
func templateProject(t testing.TB) string {
	t.Helper()
	shared.mu.Lock()
	defer shared.mu.Unlock()
	if shared.template != "" {
		return shared.template
	}
	bin := replaytest.BdBin(t)
	dir := filepath.Join(fixtureRoot(t), "template")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating the template project: %v", err)
	}
	cmd := exec.Command(bin, "init", "--non-interactive", "--role=maintainer")
	cmd.Dir = dir
	cmd.Env = doltcli.SanitizedEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init for the template project: %v\n%s", err, out)
	}
	shared.template = dir
	return dir
}

// copyProject copies the project at src to dst. cp is POSIX, like the sh
// stand-ins for bd these fixtures use.
func copyProject(t testing.TB, src, dst string) {
	t.Helper()
	if out, err := exec.Command("cp", "-a", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("copying %s to %s: %v\n%s", src, dst, err, out)
	}
}

// ---- an oracle built with bd ---------------------------------------------------

// oracleHistory is a corpus built by a real bd, and the facts about it a test
// needs: the project, its dolt database, the ids bd gave and the dolt head after
// each step.
type oracleHistory struct {
	t    testing.TB
	bin  string
	dir  string
	data string
	ids  []string
	// heads are the commits the steps ended at, oldest first: every commit a
	// command made, so a command that commits twice is two steps.
	heads   []string
	commits int
}

// adoptOracle wraps an existing copy of the template project.
func adoptOracle(t testing.TB, bin, dir string) *oracleHistory {
	t.Helper()
	o := &oracleHistory{t: t, bin: bin, dir: dir, data: replaytest.DataDir(t, dir)}
	o.commits = o.logCount()
	return o
}

// newOracle copies the template into a fresh oracle project.
func newOracle(t testing.TB, root, name string) *oracleHistory {
	t.Helper()
	replaytest.Isolate(t)
	tpl := templateProject(t)
	dir := filepath.Join(root, name)
	copyProject(t, tpl, dir)
	return adoptOracle(t, replaytest.BdBin(t), dir)
}

// logCount is the number of commits in the oracle's database.
func (o *oracleHistory) logCount() int {
	o.t.Helper()
	_, rows, err := doltcli.Query(context.Background(), o.data, "SELECT COUNT(*) FROM dolt_log")
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		o.t.Fatalf("counting the commits of %s: rows=%v err=%v", o.data, rows, err)
	}
	var n int
	if _, err := fmt.Sscanf(rows[0][0].Text, "%d", &n); err != nil {
		o.t.Fatalf("counting the commits of %s: %q is not a count", o.data, rows[0][0].Text)
	}
	return n
}

// bd runs the bd that built the oracle, so both sides share one schema.
func (o *oracleHistory) bd(args ...string) string {
	o.t.Helper()
	return replaytest.RunBd(o.t, o.bin, o.dir, args...)
}

func (o *oracleHistory) head() string {
	o.t.Helper()
	return replaytest.HeadCommit(o.t, o.data)
}

// step records the commits the caller's command just made, oldest first. The
// newest commits by commit_order are those, because the history is linear until
// a merge, and the merge is recorded by mergeSide instead.
func (o *oracleHistory) step() {
	o.t.Helper()
	count := o.logCount()
	made := count - o.commits
	o.commits = count
	_, rows, err := doltcli.Query(context.Background(), o.data, fmt.Sprintf("SELECT commit_hash FROM dolt_log ORDER BY commit_order DESC LIMIT %d", made))
	if err != nil || len(rows) != made {
		o.t.Fatalf("reading the %d newest commits of %s: rows=%v err=%v", made, o.data, rows, err)
	}
	for i := len(rows) - 1; i >= 0; i-- {
		o.heads = append(o.heads, rows[i][0].Text)
	}
}

// create makes an issue and records the step.
func (o *oracleHistory) create(title string, extra ...string) string {
	o.t.Helper()
	args := append([]string{"create", title, "--type", "task", "--json"}, extra...)
	id := replaytest.JSONID(o.t, o.bd(args...))
	o.ids = append(o.ids, id)
	o.step()
	return id
}

// run runs a bd command that is one step.
func (o *oracleHistory) run(args ...string) {
	o.t.Helper()
	o.bd(args...)
	o.step()
}

// sqlStep is a step made straight in the oracle's database: a hand-written
// oracle commit, which is how a fixture says something bd has no verb for. The
// work clone is never touched this way.
func (o *oracleHistory) sqlStep(message, stmt string) {
	o.t.Helper()
	replaytest.RunDolt(o.t, o.data, "sql", "-q", stmt)
	replaytest.RunDolt(o.t, o.data, "add", "-A")
	replaytest.RunDolt(o.t, o.data, "commit", "-m", message)
	o.step()
}

// sharedOracle builds the named history once and hands the same one to every
// test that asks. The runs only ever read it.
func sharedOracle(t testing.TB, name string, build func(o *oracleHistory)) *oracleHistory {
	t.Helper()
	shared.mu.Lock()
	if shared.built == nil {
		shared.built = map[string]*oracleHistory{}
	}
	if o, ok := shared.built[name]; ok {
		shared.mu.Unlock()
		return o
	}
	root := fixtureRoot(t)
	shared.mu.Unlock()

	o := newOracle(t, root, name)
	build(o)

	shared.mu.Lock()
	shared.built[name] = o
	shared.mu.Unlock()
	return o
}

// faultOracle is the small history the fault-injection tests replay: an issue,
// then an issue created with an edge to it.
func faultOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "fault", func(o *oracleHistory) {
		a := o.create("Alpha")
		o.create("Beta", "--deps", a)
	})
}

// rejectOracle is the history the rejection tests replay: two issues, a close,
// a later update of the closed issue and an update of the other.
func rejectOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "reject", func(o *oracleHistory) {
		a := o.create("Alpha")
		b := o.create("Beta")
		o.run("close", a, "--reason", "done")
		o.run("update", a, "--description", "after the close")
		o.run("update", b, "--description", "untouched by the close")
	})
}

// ---- a bd stand-in ---------------------------------------------------------------

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// bdStandIn writes an executable sh script that stands in for bd. It logs its
// arguments, runs body, and then runs the real bd with the same arguments; body
// may exit instead, or change the arguments, which is how a fixture makes bd
// misbehave in exactly one way. It returns the script and its log.
func bdStandIn(t testing.TB, realBd, body string) (bin, log string) {
	t.Helper()
	return bdStandInAfter(t, realBd, body, "")
}

// bdStandInAfter is bdStandIn with a second snippet that runs once the real bd
// has finished, with its exit status in $rc and the subcommand in $cmd; the
// stand-in then exits with that status. It is how a fixture changes what bd
// left behind, as a candidate with a defect of its own would.
func bdStandInAfter(t testing.TB, realBd, body, after string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "bd-stand-in")
	log = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"REAL=" + shellQuote(realBd) + "\n" +
		"LOG=" + shellQuote(log) + "\n" +
		"cmd=\"$1\"\n" +
		"printf '%s\\n' \"$*\" >> \"$LOG\"\n" +
		body + "\n"
	if after == "" {
		script += "exec \"$REAL\" \"$@\"\n"
	} else {
		script += "\"$REAL\" \"$@\"\nrc=$?\n" + after + "\nexit \"$rc\"\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { // #nosec G306 -- a test stand-in must be executable
		t.Fatalf("writing the bd stand-in: %v", err)
	}
	return bin, log
}

// standInCalls returns the logged invocations, one string per call.
func standInCalls(t testing.TB, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", log, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// countCalls counts the logged calls that start with prefix and contain every
// one of the words.
func countCalls(calls []string, prefix string, words ...string) int {
	n := 0
outer:
	for _, c := range calls {
		if !strings.HasPrefix(c, prefix) {
			continue
		}
		for _, w := range words {
			if !strings.Contains(c, w) {
				continue outer
			}
		}
		n++
	}
	return n
}

// ---- a run of the driver ---------------------------------------------------------

// fixtureIntegrationSHA stands for the commit the integration build was made from,
// which a caller of Run gives it: a fixture's bd is a stand-in, built from none.
const fixtureIntegrationSHA = "0123456789abcdef0123456789abcdef01234567"

// fixtureRun is one driver run over an oracle, into a fresh work clone, with the
// bd it is given.
type fixtureRun struct {
	t        *testing.T
	o        *oracleHistory
	workDir  string
	workData string
	outDir   string
	cfg      RunConfig
}

func newFixtureRun(t *testing.T, o *oracleHistory, integrationBin string) *fixtureRun {
	t.Helper()
	replaytest.Isolate(t)
	workDir := filepath.Join(t.TempDir(), "work")
	copyProject(t, templateProject(t), workDir)
	f := &fixtureRun{
		t:        t,
		o:        o,
		workDir:  workDir,
		workData: replaytest.DataDir(t, workDir),
		outDir:   filepath.Join(t.TempDir(), "out"),
	}
	f.cfg = RunConfig{
		IntegrationRef:  "HEAD",
		IntegrationSHA:  fixtureIntegrationSHA,
		IntegrationRepo: findRepoRoot(t),
		OracleDataDir:   o.data,
		WorkDir:         workDir,
		WorkDataDir:     f.workData,
		OutDir:          f.outDir,
		Tools:           Tools{IntegrationBin: integrationBin},
	}
	return f
}

func (f *fixtureRun) run() (ReplayRun, error) {
	f.t.Helper()
	return Run(context.Background(), f.cfg)
}

func (f *fixtureRun) results() []CommitReplayResult {
	f.t.Helper()
	return readCommitReplayResults(f.t, f.outDir)
}

func (f *fixtureRun) mismatches() []Mismatch {
	f.t.Helper()
	return readMismatches(f.t, f.outDir)
}

func (f *fixtureRun) summary() Summary {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.outDir, "summary.json"))
	if err != nil {
		f.t.Fatalf("reading summary.json: %v", err)
	}
	var s Summary
	if err := unmarshalStrict(data, &s); err != nil {
		f.t.Fatalf("summary.json: %v\n%s", err, data)
	}
	return s
}

// resultFor returns the row for one issue at one step, named by the step's
// commit, and fails the test if there is none or more than one.
func (f *fixtureRun) resultFor(commit, issue string) CommitReplayResult {
	f.t.Helper()
	var found []CommitReplayResult
	for _, r := range f.results() {
		if r.SourceCommit == commit && r.IssueID == issue {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("%d rows for issue %s at %s, want exactly 1: %+v", len(found), issue, commit, f.results())
	}
	return found[0]
}

// workCount runs a SELECT COUNT(*) against the work clone.
func (f *fixtureRun) workCount(query string) int {
	f.t.Helper()
	_, rows, err := doltcli.Query(context.Background(), f.workData, query)
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		f.t.Fatalf("%s: rows=%v err=%v", query, rows, err)
	}
	var n int
	if _, err := fmt.Sscanf(rows[0][0].Text, "%d", &n); err != nil {
		f.t.Fatalf("%s: %q is not a count", query, rows[0][0].Text)
	}
	return n
}
