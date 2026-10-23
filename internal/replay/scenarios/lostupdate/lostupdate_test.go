//go:build cgo

// Package lostupdate is a replay scenario for concurrent edits to one corpus
// snapshot. Two clones are taken of the same snapshot, each is changed on its
// own with nothing shared between the two sequences of changes, and the two are
// then reconciled through a shared remote. It measures what reconciling does to
// the edits: a same-field race must come back as conflict data and never as a
// silent winner, and edits to different issues must merge with both issues and
// their attribution intact.
//
// The reconciled clone is read the way the harness reads history, not through
// the store that wrote it: an issue's row at a commit through the oracle, and
// the version rows and their change_actor through doltcli. The scenario replays
// no history, so it never runs the driver; reading the reconciled clone through
// the oracle is how it runs the harness against the reconciled result.
//
// The race count is a measurement. A nonzero count is recorded as a
// METRIC_SAMPLE line and is not a failure; in the same-field case a count of
// zero is the failure, because it means a conflict went unreported.
package lostupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/embeddeddolt"
	"github.com/steveyegge/beads/internal/testutil/bazeltest"
	"github.com/steveyegge/beads/internal/types"
)

const (
	// dbName is the corpus database both clones carry.
	dbName = "lostupdate"
	// sharedID is the one issue the seed snapshot holds.
	sharedID = "shared-1"
	// remoteName is what each clone calls the shared remote.
	remoteName = "shared"
)

// acquire is the corpus-acquire binary, built once for the whole test binary.
var acquire struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func TestMain(m *testing.M) {
	code := replaytest.Main(m)
	if acquire.dir != "" {
		_ = os.RemoveAll(acquire.dir)
	}
	os.Exit(code)
}

// acquireBin returns the corpus-acquire binary, building it from this tree on
// the first call. The build runs in the environment the test binary started
// with, before any test narrows it.
func acquireBin(t testing.TB) string {
	t.Helper()
	root := bazeltest.RepoRoot(t)
	acquire.once.Do(func() {
		dir, err := os.MkdirTemp("", "lostupdate-acquire-")
		if err != nil {
			acquire.err = err
			return
		}
		acquire.dir = dir
		out := filepath.Join(dir, "corpus-acquire")
		goBin, err := replaytest.GoBin()
		if err != nil {
			acquire.err = fmt.Errorf("building corpus-acquire from %s: %w", root, err)
			return
		}
		cmd := exec.Command(goBin, "build", "-o", out, "./scripts/corpus-acquire")
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			acquire.err = fmt.Errorf("building corpus-acquire from %s: %w\n%s", root, err, b)
			return
		}
		acquire.path = out
	})
	if acquire.err != nil {
		t.Fatalf("%v", acquire.err)
	}
	return acquire.path
}

func TestLostUpdateRace(t *testing.T) {
	// Building corpus-acquire needs the Go toolchain and this source tree, the same
	// two things building bd needs, and neither exists inside a Bazel sandbox, so
	// the bd requirement is the right gate for it.
	replaytest.Require(t, replaytest.NeedDolt|replaytest.NeedBd)
	bin := acquireBin(t)

	t.Run("concurrent_same_field_edit_is_a_detected_conflict_not_a_silent_lost_update", func(t *testing.T) {
		sameFieldEdit(t, bin)
	})
	t.Run("concurrent_edits_to_different_issues_reconcile_cleanly_and_match_the_oracle", func(t *testing.T) {
		differentIssueEdits(t, bin)
	})
}

// sameFieldEdit is the positive control: both clones edit the same field of the
// same issue with no shared state, which is a textbook lost-update race. The
// merge must report it as conflict data and abort, never pick a winner and never
// fail with a Go error.
func sameFieldEdit(t *testing.T, bin string) {
	ctx := context.Background()
	f := newFixture(t, bin)

	check(t, "clone A update", f.a.store.UpdateIssue(ctx, sharedID, map[string]interface{}{"title": "A's title"}, "actor-a"))
	check(t, "clone A commit", f.a.store.Commit(ctx, "A: update title"))
	check(t, "clone B update", f.b.store.UpdateIssue(ctx, sharedID, map[string]interface{}{"title": "B's title"}, "actor-b"))
	check(t, "clone B commit", f.b.store.Commit(ctx, "B: update title"))

	check(t, "clone A add remote", f.a.store.AddRemote(ctx, remoteName, f.remoteURL))
	check(t, "clone A push", f.a.store.PushTo(ctx, remoteName))
	check(t, "clone B add remote", f.b.store.AddRemote(ctx, remoteName, f.remoteURL))
	conflicts, err := f.b.store.PullFrom(ctx, remoteName)
	if err != nil {
		t.Fatalf("clone B pull: %v (a lost-update race must be reported as data, a non-empty conflicts slice with a nil error, never as a Go error)", err)
	}

	races := recordRaceCount(t, conflicts)
	if races == 0 {
		t.Fatalf("clone A and clone B both edited %s's title on their own, but reconciling reported no conflict; a same-field lost-update race must be surfaced and never silently merged", sharedID)
	}
	var sawIssues bool
	for _, c := range conflicts {
		sawIssues = sawIssues || c.Field == "issues"
	}
	if !sawIssues {
		t.Fatalf("conflicts=%+v: expected a conflict in the issues table from the concurrent title edit", conflicts)
	}

	// The conflicted pull aborts the merge, so clone B's own edit must be neither
	// lost nor overwritten by A's.
	f.closeStores()
	headB := replaytest.HeadCommit(t, f.b.dir)
	got, err := oracle.QueryAsOf(ctx, f.b.dir, headB, sharedID)
	if err != nil {
		t.Fatalf("reading %s as of clone B's head %s: %v", sharedID, headB, err)
	}
	if got == nil {
		t.Fatalf("%s has no row as of clone B's head %s", sharedID, headB)
	}
	if got["title"] != "B's title" {
		t.Errorf("clone B's row after a conflicted pull: title = %q, want %q (an aborted merge must never overwrite the puller's own edit)", got["title"], "B's title")
	}
}

// differentIssueEdits is the negative control: the clones create two different
// issues, so no row or key overlaps. It must reconcile with no conflict, and each
// issue must read back from the reconciled clone exactly as it reads in the clone
// that made it, version rows and attribution included.
func differentIssueEdits(t *testing.T, bin string) {
	ctx := context.Background()
	f := newFixture(t, bin)

	// edit is an issue and the clone that holds the authoritative copy of it.
	type edit struct {
		id, title, actor string
		origin           *clone
	}
	created := []edit{
		{"a-only", "A's own issue", "actor-a", f.a},
		{"b-only", "B's own issue", "actor-b", f.b},
	}
	for _, e := range created {
		issue := &types.Issue{ID: e.id, Title: e.title, IssueType: types.TypeTask, Status: types.StatusOpen}
		check(t, "create "+e.id, e.origin.store.CreateIssue(ctx, issue, e.actor))
		check(t, "commit "+e.id, e.origin.store.Commit(ctx, "create "+e.id))
	}
	// The seed issue is in both clones and nobody touches it, so either clone is
	// its origin.
	expected := append(created, edit{sharedID, "seed", "seed-actor", f.a})

	// What each clone says on its own, before anything is reconciled.
	before := map[*clone]ref{
		f.a: {f.a.dir, f.head(t, f.a)},
		f.b: {f.b.dir, f.head(t, f.b)},
	}

	check(t, "clone A add remote", f.a.store.AddRemote(ctx, remoteName, f.remoteURL))
	check(t, "clone A push", f.a.store.PushTo(ctx, remoteName))
	check(t, "clone B add remote", f.b.store.AddRemote(ctx, remoteName, f.remoteURL))
	conflicts, err := f.b.store.PullFrom(ctx, remoteName)
	if err != nil {
		t.Fatalf("clone B pull: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("clone A and clone B created two different issues with no key overlap, but reconciling reported conflicts=%+v, want none", conflicts)
	}

	f.closeStores()
	reconciledAt := ref{f.b.dir, replaytest.HeadCommit(t, f.b.dir)}

	var wantVersionRows int
	for _, e := range expected {
		oracleSays := readState(t, before[e.origin], e.id)
		reconciled := readState(t, reconciledAt, e.id)

		if title, _ := oracleSays.issue.Cell("title"); title.Text != e.title {
			t.Fatalf("the clone that made %s reads title %q, want %q: the fixture is wrong, not the merge", e.id, title.Text, e.title)
		}
		if diffs := diffRows(oracleSays.issue, reconciled.issue); len(diffs) > 0 {
			t.Errorf("reconciled clone: %s differs from the clone that made it:\n  %s", e.id, strings.Join(diffs, "\n  "))
		}
		if len(reconciled.versions) == 0 {
			t.Errorf("reconciled clone: issue_versions has no rows for %s; the merge must not lose either side's version history", e.id)
			continue
		}
		if !reflect.DeepEqual(oracleSays.versions, reconciled.versions) {
			t.Errorf("reconciled clone: issue_versions rows for %s = %s, want %s (the rows in the clone that made it)", e.id, showCells(reconciled.versions), showCells(oracleSays.versions))
		}
		if actor := reconciled.versions[0][2]; actor.Null || actor.Text != e.actor {
			t.Errorf("reconciled clone: issue_versions for %s change_actor = %s, want %q (attribution must survive the merge)", e.id, showCell(actor), e.actor)
		}
		wantVersionRows += len(oracleSays.versions)
	}

	// Nothing extra either: the merge adds no version row of its own.
	_, rows, err := doltcli.Query(ctx, f.b.dir, "SELECT COUNT(*) FROM issue_versions AS OF "+doltcli.SQLQuote(reconciledAt.at))
	if err != nil {
		t.Fatalf("counting issue_versions in the reconciled clone: %v", err)
	}
	if got := rows[0][0].Text; got != fmt.Sprint(wantVersionRows) {
		t.Errorf("reconciled clone holds %s issue_versions rows, want %d (one set per issue, none lost and none added)", got, wantVersionRows)
	}
}

// recordRaceCount writes the METRIC_SAMPLE line for the lost-update race count
// and returns the value it wrote.
//
// The count is the sum of dolt_conflicts.num_conflicts over the conflicted
// tables, as versioncontrolops.GetConflicts read it from Dolt while the merge
// was still open. A conflicted pull aborts the merge before it returns, so
// dolt_conflicts is empty afterwards (see MergeConflictsError) and PullFrom's
// result is the only place the count survives. It is Dolt's own number, neither
// hard-coded nor counted here.
func recordRaceCount(t *testing.T, conflicts []storage.Conflict) int {
	t.Helper()
	races := 0
	for _, c := range conflicts {
		t.Logf("conflict: table=%s num_conflicts=%d", c.Field, c.Count)
		races += c.Count
	}
	t.Logf("METRIC_SAMPLE lost_update_race_conflict_count=%d", races)
	return races
}

// clone is one working clone of the seed snapshot: the store a test writes
// through and the directory the dolt CLI reads it from.
type clone struct {
	store  *embeddeddolt.EmbeddedDoltStore
	dir    string
	closed bool
}

// close releases the store, once. The dolt CLI reads a clone only after this, so
// no writer holds it open.
func (c *clone) close() {
	if c.closed {
		return
	}
	c.closed = true
	_ = c.store.Close()
}

// fixture is two disconnected clones of one seeded snapshot and the bare git
// repository they reconcile through.
type fixture struct {
	a, b      *clone
	remoteURL string
}

// closeStores releases both stores, so the dolt CLI can read the clones without
// a writer holding them open. It is safe to call more than once.
func (f *fixture) closeStores() {
	f.a.close()
	f.b.close()
}

// head returns the commit a clone's store is at.
func (f *fixture) head(t *testing.T, c *clone) string {
	t.Helper()
	sha, err := c.store.GetCurrentCommit(context.Background())
	check(t, "reading a clone's head", err)
	return sha
}

// newFixture seeds a snapshot holding one issue, takes two clones of it with
// corpus-acquire, and opens both for writing. Each call is hermetic and shares
// nothing with another fixture, so one test's changes cannot reach another's.
//
// Three details of the stores are easy to get wrong:
//
//   - A freshly opened store has no issue_prefix yet and refuses every change
//     until one is set and committed.
//   - embeddeddolt.Open reads <beadsDir>/embeddeddolt/<db>/.dolt, while
//     corpus-acquire writes <dest>/.dolt. Acquiring each clone straight to
//     <beadsDir>/embeddeddolt/<db> satisfies both from one copy.
//   - CreateIssue and UpdateIssue only commit the SQL transaction, never a Dolt
//     commit, and a push carries Dolt commits only, so every change needs an
//     explicit Commit before the clone is pushed or pulled.
func newFixture(t *testing.T, bin string) *fixture {
	t.Helper()
	ctx := context.Background()
	replaytest.Isolate(t)
	// Dolt runs git for a git+file remote, and the isolated HOME holds no git
	// identity for it to commit with.
	for name, value := range map[string]string{
		"GIT_AUTHOR_NAME":     "replay-test",
		"GIT_AUTHOR_EMAIL":    "replay-test@example.invalid",
		"GIT_COMMITTER_NAME":  "replay-test",
		"GIT_COMMITTER_EMAIL": "replay-test@example.invalid",
	} {
		t.Setenv(name, value)
	}
	root := t.TempDir()

	seedBeadsDir := filepath.Join(root, "seed", ".beads")
	seed, err := embeddeddolt.Open(ctx, seedBeadsDir, dbName, "main")
	check(t, "opening the seed store", err)
	check(t, "setting the seed issue_prefix", seed.SetConfig(ctx, "issue_prefix", dbName))
	check(t, "committing the seed init", seed.Commit(ctx, "init"))
	seed.SetVersionedHistoryEnabled(true)
	check(t, "creating the seed issue", seed.CreateIssue(ctx, &types.Issue{ID: sharedID, Title: "seed", IssueType: types.TypeTask, Status: types.StatusOpen}, "seed-actor"))
	check(t, "committing the seed issue", seed.Commit(ctx, "seed: create "+sharedID))
	check(t, "closing the seed store", seed.Close())
	sourceDataDir := filepath.Join(seedBeadsDir, "embeddeddolt")

	a, snapshotA := openClone(t, bin, sourceDataDir, filepath.Join(root, "clone-a-beads"))
	b, snapshotB := openClone(t, bin, sourceDataDir, filepath.Join(root, "clone-b-beads"))
	if snapshotA != snapshotB {
		t.Fatalf("the two clones come from different snapshots: %s and %s", snapshotA, snapshotB)
	}
	f := &fixture{a: a, b: b}

	gitDir := filepath.Join(root, "shared.git")
	initBareRepo(t, gitDir)
	f.remoteURL = "git+file://" + gitDir
	return f
}

// openClone acquires a clone of the seed database straight to where a store
// opened on beadsDir looks for it, opens that store, and returns it with the
// snapshot commit corpus-acquire says it copied.
func openClone(t *testing.T, bin, sourceDataDir, beadsDir string) (*clone, string) {
	t.Helper()
	dir := filepath.Join(beadsDir, "embeddeddolt", dbName)
	snapshot := runAcquire(t, bin, sourceDataDir, dir)
	store, err := embeddeddolt.Open(context.Background(), beadsDir, dbName, "main")
	check(t, "opening the clone at "+beadsDir, err)
	store.SetVersionedHistoryEnabled(true)
	c := &clone{store: store, dir: dir}
	t.Cleanup(c.close)
	return c, snapshot
}

// runAcquire takes a filesystem-level clone of the seed database into dest with
// corpus-acquire, and returns the snapshot commit it reports. A clone is never
// taken with git or with dolt clone: the store is in chunk-journal format, which
// a dolt clone of a file remote cannot open.
func runAcquire(t *testing.T, bin, dataDir, dest string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), bin, "-data-dir", dataDir, "-db", dbName, "-dest", dest)
	cmd.Env = doltcli.SanitizedEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("corpus-acquire -data-dir=%s -db=%s -dest=%s: %v\n%s", dataDir, dbName, dest, err, stderr.String())
	}
	var result struct {
		SnapshotCommitHash string `json:"snapshot_commit_hash"`
		IssueCount         int    `json:"issue_count"`
		Partial            bool   `json:"partial"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("corpus-acquire printed something other than its result: %v\n%s", err, out)
	}
	if result.Partial || result.IssueCount != 1 || result.SnapshotCommitHash == "" {
		t.Fatalf("corpus-acquire returned an unusable clone of the seed: %+v", result)
	}
	return result.SnapshotCommitHash
}

// initBareRepo creates a bare git repository at dir holding one empty commit on
// main. A git+file remote needs a branch to exist before the first push: pushing
// to a repository with no commits fails.
func initBareRepo(t *testing.T, dir string) {
	t.Helper()
	runGit(t, "", "init", "--bare", "-q", dir)
	tree := strings.TrimSpace(runGit(t, dir, "hash-object", "-t", "tree", os.DevNull))
	commit := strings.TrimSpace(runGit(t, dir, "commit-tree", tree, "-m", "init"))
	runGit(t, dir, "update-ref", "refs/heads/main", commit)
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")
}

// runGit runs git, in dir when dir is not empty, under the sanitized
// environment. A bare repository needs no work tree for plumbing commands.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Env = doltcli.SanitizedEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// ref names a commit of a clone's database.
type ref struct {
	dir, at string
}

// state is what one clone says about one issue at one commit: the issue's row as
// the oracle reads it, and the issue's version rows as doltcli reads them.
type state struct {
	issue    oracle.Row
	versions [][]doltcli.Cell
}

// readState reads issue id at r. Only counts, NULL-ness and change_actor are
// read from issue_versions; the stored state is left alone.
func readState(t *testing.T, r ref, id string) state {
	t.Helper()
	ctx := context.Background()
	view, err := oracle.ReadView(ctx, r.dir, r.at, id)
	if err != nil {
		t.Fatalf("reading %s as of %s in %s: %v", id, r.at, r.dir, err)
	}
	if view == nil {
		t.Fatalf("%s has no row as of %s in %s", id, r.at, r.dir)
	}
	_, rows, err := doltcli.Query(ctx, r.dir, "SELECT issue_id, revision, change_actor FROM issue_versions AS OF "+
		doltcli.SQLQuote(r.at)+" WHERE issue_id = "+doltcli.SQLQuote(id)+" ORDER BY revision")
	if err != nil {
		t.Fatalf("reading the version rows of %s as of %s in %s: %v", id, r.at, r.dir, err)
	}
	return state{issue: view.Issue, versions: rows}
}

// diffRows lists every column on which two rows of one table differ, telling
// NULL from the empty string.
func diffRows(want, got oracle.Row) []string {
	if !reflect.DeepEqual(want.Columns, got.Columns) {
		return []string{fmt.Sprintf("the columns differ: %v and %v", want.Columns, got.Columns)}
	}
	var diffs []string
	for i, name := range want.Columns {
		if want.Cells[i] != got.Cells[i] {
			diffs = append(diffs, fmt.Sprintf("%s: %s, want %s", name, showCell(got.Cells[i]), showCell(want.Cells[i])))
		}
	}
	return diffs
}

func showCell(c doltcli.Cell) string {
	if c.Null {
		return "NULL"
	}
	return fmt.Sprintf("%q", c.Text)
}

func showCells(rows [][]doltcli.Cell) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		cells := make([]string, len(row))
		for j, c := range row {
			cells[j] = showCell(c)
		}
		parts[i] = "(" + strings.Join(cells, ", ") + ")"
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func check(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}
