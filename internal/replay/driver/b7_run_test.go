package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// These tests run the real driver-core command, through the rig in
// b7_fixture_test.go: they kill it, interrupt it, start it again and ask it to
// resume, and then look at what the run left in its directories.

// stepEnds are the oracle commits the steps of a run end at, in order. The base is
// the commit before the first step: a fixture that begins with a schema change of
// its own has that commit among its heads, any other starts from the template's.
func stepEnds(o *oracleHistory, base string) []string {
	if len(o.heads) > 0 && o.heads[0] == base {
		return o.heads[1:]
	}
	return o.heads
}

// outline is a journal as one line, "started 0, finished 0, started 1, ...", so a
// failure can show all of it.
func outline(entries []map[string]any) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		step, _ := memberInt(e, "step")
		parts = append(parts, fmt.Sprintf("%s %d", memberString(e, "event"), step))
	}
	return strings.Join(parts, ", ")
}

// commitHash is what the hash of a commit in the work project's database looks like.
var commitHash = regexp.MustCompile(`^[0-9a-z]{16,64}$`)

// killedMidway is a rig whose run was killed in the second step of the lifecycle
// oracle, the create of Beta, right after bd carried out the create and before the
// edge to Alpha: half a step is in the work database and none of it is recorded.
// It returns the id of the run it left behind.
func killedMidway(t *testing.T, o *oracleHistory) (*cliRig, string) {
	t.Helper()
	r := newCLIRig(t, o)
	r.killAfterApply("create*Beta*")
	return r, r.runKilled()
}

// requireCompleted fails the test unless the command line ran its run to the end,
// and returns the run's id.
func requireCompleted(t *testing.T, what string, res cliResult) string {
	t.Helper()
	id, status := ranAs(t, res)
	if res.code != 0 || status != "completed" {
		t.Fatalf("%s ended with exit %d, status %s:\n%s", what, res.code, status, res.out)
	}
	return id
}

// B7.RunningRow: a run says it is running before it does anything a crash could
// cut short. By the first step run.json names the run and what it is pinned to,
// replay_runs.jsonl holds its running row and the journal has started the first
// step; at the end the journal holds a started and a finished entry for every step,
// in order, and every result row names its step.
func TestB7RunningRow(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := restoreOracle(t)
	r := newCLIRig(t, o)
	atFirstStep := r.mark("at-first-step")
	r.install(onCall("create*", once(r.mark("copied"), copyOutDir(r.outDir, atFirstStep))), "", "")

	id := requireCompleted(t, "the run", r.run())
	sum := readSummary(t, r.outPath(summaryFile))
	base := sum.CoveredRange.BaseCommit
	ends := stepEnds(o, base)
	if len(ends) != sum.CoveredRange.CommitsReplayed {
		t.Fatalf("the oracle has %d steps after its base and the run replayed %d", len(ends), sum.CoveredRange.CommitsReplayed)
	}

	// What the run had written when bd was first asked to change anything.
	if _, err := os.Stat(filepath.Join(atFirstStep, runFile)); err != nil {
		t.Fatalf("by the first step the run had not written %s", runFile)
	}
	pinned := jsonObject(t, filepath.Join(atFirstStep, runFile))
	for key, want := range map[string]string{
		"id":              id,
		"integration_sha": r.second,
		"oracle_head":     replaytest.HeadCommit(t, o.data),
		"base_commit":     base,
		"mode":            "exhaustive",
		"status":          "running",
	} {
		if got := memberString(pinned, key); got != want {
			t.Errorf("by the first step %s has %s = %q, want %q", runFile, key, got, want)
		}
	}
	if rows := rowsFor(jsonRows(t, filepath.Join(atFirstStep, runsFile)), id); len(rows) != 1 || memberString(rows[0], "status") != "running" {
		t.Errorf("by the first step %s holds %v for the run, want its one running row", runsFile, rows)
	}
	first := jsonRows(t, filepath.Join(atFirstStep, journalFile))
	if len(first) != 1 || memberString(first[0], "event") != "started" {
		t.Errorf("by the first step the journal is [%s], want the started entry of step 0 and nothing else", outline(first))
	} else {
		requireStarted(t, first[0], 0, base, ends[0])
	}

	// What it held at the end.
	var statuses []string
	for _, row := range rowsFor(jsonRows(t, r.outPath(runsFile)), id) {
		statuses = append(statuses, memberString(row, "status"))
	}
	if want := []string{"running", "completed"}; !slices.Equal(statuses, want) {
		t.Errorf("%s holds the statuses %v for the run, want %v", runsFile, statuses, want)
	}

	var want []string
	for k := range ends {
		want = append(want, fmt.Sprintf("started %d", k), fmt.Sprintf("finished %d", k))
	}
	entries := r.journal()
	if got := outline(entries); got != strings.Join(want, ", ") {
		t.Errorf("the journal is\n  %s\nwant a started and a finished entry for every step, in order:\n  %s", got, strings.Join(want, ", "))
	} else {
		heads := make([]string, len(ends))
		for k := range ends {
			from := base
			if k > 0 {
				from = ends[k-1]
			}
			requireStarted(t, entries[2*k], k, from, ends[k])
			heads[k] = memberString(entries[2*k], "work_head_before")
			if !commitHash.MatchString(heads[k]) {
				continue
			}
			if n := r.workCount("SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '" + heads[k] + "'"); n != 1 {
				t.Errorf("step %d started from the work head %s, which the work project's history does not hold", k, heads[k])
			}
		}
		if heads[0] == heads[1] {
			t.Errorf("steps 0 and 1 both started from the work head %s, though step 0 created an issue", heads[0])
		}
	}

	for _, row := range jsonRows(t, r.outPath(resultsFile)) {
		commit := memberString(row, "source_commit")
		got, ok := memberInt(row, "step")
		if want := slices.Index(ends, commit); !ok || got != want {
			t.Errorf("the result row for %s has step %v (present: %v), want %d", commit, got, ok, want)
		}
	}
}

// requireStarted checks one started entry of the journal: the step, the commits
// it goes between and the head of the work database it began from.
func requireStarted(t *testing.T, e map[string]any, step int, from, to string) {
	t.Helper()
	if event := memberString(e, "event"); event != "started" {
		t.Errorf("journal entry %v is %q, want started", e, event)
	}
	if got, ok := memberInt(e, "step"); !ok || got != step {
		t.Errorf("journal entry %v has step %v (present: %v), want %d", e, got, ok, step)
	}
	if got := memberString(e, "from"); got != from {
		t.Errorf("the started entry of step %d goes from %q, want %q", step, got, from)
	}
	if got := memberString(e, "to"); got != to {
		t.Errorf("the started entry of step %d goes to %q, want %q", step, got, to)
	}
	if head := memberString(e, "work_head_before"); !commitHash.MatchString(head) {
		t.Errorf("the started entry of step %d has work_head_before %q, want the work database's head", step, head)
	}
}

// B7.RefuseNonEmpty: a run starts into a work directory that is empty or not
// there. A start over one that holds anything is refused, so is a resume that
// names a run the directory does not hold, and a start over a run that never
// finished says so and says how to go on. A refusal writes nothing and asks bd
// for nothing.
func TestB7RefuseNonEmpty(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)

	t.Run("a work directory that holds something", func(t *testing.T) {
		r := newCLIRig(t, o)
		r.install("", "", "")
		if err := os.MkdirAll(r.workDir, 0o755); err != nil {
			t.Fatalf("creating the work directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(r.workDir, "keep.txt"), []byte("not the run's to touch\n"), 0o600); err != nil {
			t.Fatalf("writing into the work directory: %v", err)
		}
		work := dirSnapshot(t, r.workDir)
		before := r.state()

		res := r.run()
		r.refused("a start into a work directory that is not empty", res, r.workDir)
		r.requireUntouched("the refused start", before)
		if changed := changedFiles(work, dirSnapshot(t, r.workDir)); len(changed) != 0 {
			t.Errorf("the refused start changed the work directory: %s", firstOf(changed, 10))
		}
	})

	t.Run("a resume that names another run", func(t *testing.T) {
		r, _ := killedMidway(t, o)
		before := r.state()

		const other = "replay-run-0123456789abcdef"
		res := r.run("--resume", other)
		r.refused("a resume of a run the directories do not hold", res, other)
		r.requireUntouched("the refused resume", before)
	})

	t.Run("a start over a run that never finished", func(t *testing.T) {
		r, id := killedMidway(t, o)
		before := r.state()

		res := r.run()
		r.refused("a start over an unfinished run", res, "unfinished", id, "--resume "+id, r.outDir)
		r.requireUntouched("the refused start", before)

		// The way out the refusal names is a real one: with the directories gone,
		// the same command line makes a run of its own.
		for _, dir := range []string{r.outDir, r.workDir} {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatalf("removing %s: %v", dir, err)
			}
		}
		if again := requireCompleted(t, "the run after the directories were removed", r.run()); again == id {
			t.Errorf("the run after the directories were removed has the id %s of the run that was killed", id)
		}
	})
}

// B7.RerunCompletedRefused: a run that completed is not applied again. Asked to
// resume it, the command prints the result the run ended with and exits with an
// error; a plain start over its directories is refused, because the work directory
// is not empty. Neither runs bd or touches the directories.
func TestB7RerunCompletedRefused(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)
	r := newCLIRig(t, o)
	r.install("", "", "")
	id := requireCompleted(t, "the run", r.run())
	before := r.state()
	if len(before.calls) == 0 {
		t.Fatalf("the run asked bd for nothing, so a second run would show nothing")
	}

	res := r.run("--resume", id)
	r.refused("a resume of a completed run", res, "replay run "+id+": status=completed")
	r.requireUntouched("the refused resume", before)

	res = r.run()
	r.refused("a start over the directories of a completed run", res, r.workDir)
	r.requireUntouched("the refused start", before)
}

// B7.ResumeAfterKill: a run killed with a step half done, or done and not
// recorded, resumes to the result a run that was not killed has. The step is rewound
// in the work database and run again, so nothing is applied twice; rows the killed
// run wrote for it before it was journaled finished are dropped.
func TestB7ResumeAfterKill(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)
	clean := cleanBaseline(t, "lifecycle", o, "")

	// resumeAndCompare resumes the killed run, which the caller has looked at, and
	// holds the result up against the run that was not killed.
	resumeAndCompare := func(t *testing.T, r *cliRig, id string) {
		t.Helper()
		res := r.run("--resume", id)
		if got := requireCompleted(t, "the resumed run", res); got != id {
			t.Fatalf("the resume made a run of its own, %s, instead of finishing %s", got, id)
		}
		requireSameOutcome(t, "the resumed run", clean.files, outcome(t, r.outDir, id))
		if n := r.workCount("SELECT COUNT(*) FROM issues"); n != clean.issues {
			t.Errorf("the work project holds %d issues after the resume, want %d", n, clean.issues)
		}
		if last := rowsFor(jsonRows(t, r.outPath(runsFile)), id); len(last) == 0 || memberString(last[len(last)-1], "status") != "completed" {
			t.Errorf("the last row for the run in %s is not its completed row: %v", runsFile, last)
		}
	}

	for _, tc := range []struct{ name, glob string }{
		{"killed after bd created the issue of a step", "create*Beta*"},
		{"killed after bd added the edge of a step", "dep add*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCLIRig(t, o)
			r.killAfterApply(tc.glob)
			id := r.runKilled()

			// What the kill left: a run on record that is not finished, a step
			// started and not finished, and that step's work in the database.
			var statuses []string
			for _, row := range rowsFor(jsonRows(t, r.outPath(runsFile)), id) {
				statuses = append(statuses, memberString(row, "status"))
			}
			if want := []string{"running"}; !slices.Equal(statuses, want) {
				t.Errorf("%s holds the statuses %v for the killed run, want %v", runsFile, statuses, want)
			}
			step, headBefore, _ := r.unfinished()
			if step != 1 {
				t.Fatalf("the run was killed in step %d, want the create of Beta, step 1", step)
			}
			crashHead := r.workHead()
			if crashHead == headBefore {
				t.Fatalf("the work database is where step %d found it: the kill came before bd applied anything", step)
			}

			resumeAndCompare(t, r, id)

			// Nothing was applied twice.
			calls := standInCalls(t, r.calls)
			if n := countCalls(calls, "init"); n != 1 {
				t.Errorf("bd initialised the work project %d times, want once", n)
			}
			if n := countCalls(calls, "create", "Alpha"); n != 1 {
				t.Errorf("bd was asked to create Alpha %d times, want once: step 0 had finished", n)
			}
			if n := countCalls(calls, "create", "Beta"); n != 2 {
				t.Errorf("bd was asked to create Beta %d times, want twice: the killed attempt and the redo", n)
			}

			// The killed attempt was rewound, not built on.
			if !commitHash.MatchString(crashHead) {
				t.Fatalf("the work head after the kill, %q, is not a commit hash", crashHead)
			}
			if n := r.workCount("SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '" + crashHead + "'"); n != 0 {
				t.Errorf("the work database's history still holds %s, the head the killed attempt left", crashHead)
			}
			var starts []string
			for _, e := range r.journal() {
				if n, _ := memberInt(e, "step"); n == step && memberString(e, "event") == "started" {
					starts = append(starts, memberString(e, "work_head_before"))
				}
			}
			if want := []string{headBefore, headBefore}; !slices.Equal(starts, want) {
				t.Errorf("the journal started step %d from the work heads %v, want %v: once for the attempt that was killed and once for the redo, from the head the rewind restored", step, starts, want)
			}
		})
	}

	t.Run("rows the killed run wrote for a step it never finished", func(t *testing.T) {
		r, id := killedMidway(t, o)
		step, _, to := r.unfinished()

		// The row a run that was not killed wrote for the step, in the killed
		// run's name: as if the kill had come after the row and before the journal
		// said the step was finished.
		var forged []string
		for _, line := range strings.Split(strings.TrimSpace(clean.files[resultsFile]), "\n") {
			var row CommitReplayResult
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("the clean run's result row %q: %v", line, err)
			}
			if row.SourceCommit == to {
				forged = append(forged, strings.ReplaceAll(line, "<run>", id))
			}
		}
		if len(forged) == 0 {
			t.Fatalf("the clean run wrote no result row for step %d, so there is nothing to forge", step)
		}
		f, err := os.OpenFile(r.outPath(resultsFile), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) // #nosec G304 -- a file in the test's own directory
		if err != nil {
			t.Fatalf("opening the killed run's results: %v", err)
		}
		if _, err := f.WriteString(strings.Join(forged, "\n") + "\n"); err != nil {
			t.Fatalf("writing the killed run's results: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing the killed run's results: %v", err)
		}

		resumeAndCompare(t, r, id)
	})
}

// B7.ResumeRefusesMovedSource: a resume only continues a run over what the run
// began with. When the oracle has moved on, the integration ref points somewhere
// else, or the base is not the one the run recorded, it is refused, naming what
// differs, and nothing is written or run.
func TestB7ResumeRefusesMovedSource(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)

	t.Run("the oracle has moved on", func(t *testing.T) {
		r, id := killedMidway(t, o)
		before := r.state()

		res := r.run("--resume", id, "--oracle-data-dir", movedOracle(t, o))
		r.refused("a resume over an oracle that has moved on", res, "oracle_head")
		r.requireUntouched("the refused resume", before)
	})

	t.Run("the integration ref has moved", func(t *testing.T) {
		r, id := killedMidway(t, o)
		r.git("commit", "--allow-empty", "-m", "the ref moves on")
		before := r.state()

		res := r.run("--resume", id)
		r.refused("a resume after the integration ref moved", res, "integration_sha")
		r.requireUntouched("the refused resume", before)
	})

	t.Run("the base is not the recorded one", func(t *testing.T) {
		r, id := killedMidway(t, o)
		pinned := jsonObject(t, r.outPath(runFile))
		pinned["base_commit"] = strings.Repeat("0", 32)
		data, err := json.Marshal(pinned)
		if err != nil {
			t.Fatalf("marshalling %s: %v", runFile, err)
		}
		if err := os.WriteFile(r.outPath(runFile), data, 0o600); err != nil {
			t.Fatalf("rewriting %s: %v", runFile, err)
		}
		before := r.state()

		res := r.run("--resume", id)
		r.refused("a resume of a run recorded over another base", res, "base_commit")
		r.requireUntouched("the refused resume", before)
	})
}

// B7.SingleSHA: the commit a run records is the commit it built. The integration
// ref moves while the build runs; the run names the commit that was built, in
// run.json, in its row in replay_runs.jsonl and in the line the command prints,
// and does not look the ref up a second time.
func TestB7SingleSHA(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)
	r := newCLIRig(t, o)
	// The go stand-in runs in the temporary worktree the driver made for the build.
	// It moves main back to the older commit while it "builds".
	r.install("", "", "git -C "+shellQuote(r.repo)+" update-ref refs/heads/main "+r.first)

	res := r.run()
	id := requireCompleted(t, "the run", res)

	built, err := os.ReadFile(r.built)
	if err != nil {
		t.Fatalf("reading what the go stand-in was asked to build: %v", err)
	}
	if got := strings.Fields(string(built)); !slices.Equal(got, []string{r.second}) {
		t.Fatalf("the build was made of %v, want the one commit main was at when the run started, %s", got, r.second)
	}

	rows := rowsFor(jsonRows(t, r.outPath(runsFile)), id)
	if len(rows) == 0 {
		t.Errorf("%s holds no row for the run", runsFile)
	}
	for _, row := range rows {
		if got := memberString(row, "integration_sha"); got != r.second {
			t.Errorf("a %q row in %s names the integration build %s, want the commit that was built, %s", memberString(row, "status"), runsFile, got, r.second)
		}
	}
	if data, err := os.ReadFile(r.outPath(runFile)); err != nil {
		t.Errorf("reading %s: %v", runFile, err)
	} else {
		var pinned map[string]any
		if err := json.Unmarshal(data, &pinned); err != nil {
			t.Errorf("%s is not a JSON object: %v", runFile, err)
		} else if got := memberString(pinned, "integration_sha"); got != r.second {
			t.Errorf("%s names the integration build %s, want the commit that was built, %s", runFile, got, r.second)
		}
	}
	line := regexp.MustCompile(`integration_sha=(\S+)`).FindStringSubmatch(res.out)
	if line == nil || line[1] != r.second {
		t.Errorf("the command printed %q, want integration_sha=%s", strings.TrimSpace(res.out), r.second)
	}
}

// B7.SigintNoWorktree: an interrupt ends the run the way a failure would, and
// cleans up after it. Whether it comes during the build or in a step, the command
// exits with an error of its own, no worktree is left registered in the integration
// repository and nothing is left in the temporary directory. An interrupted step is
// not recorded as bd refusing it, and the run resumes from it.
func TestB7SigintNoWorktree(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := lifecycleOracle(t)

	// requireStopped checks what an interrupted command line left of itself.
	requireStopped := func(t *testing.T, r *cliRig, res cliResult) {
		t.Helper()
		if res.code <= 0 {
			t.Errorf("the interrupt ended the command with exit %d, want an error exit of its own, which a signal's end is not:\n%s", res.code, res.out)
		}
		if worktrees := r.worktrees(); len(worktrees) != 1 {
			t.Errorf("the integration repository lists the worktrees %v, want only its own", worktrees)
		}
		if left := r.leftInTmp(); len(left) != 0 {
			t.Errorf("the command left %v in the temporary directory", left)
		}
	}

	t.Run("during the build", func(t *testing.T) {
		r := newCLIRig(t, o)
		ready := r.mark("build-ready")
		r.install("", "", once(r.mark("build-held"), hold(ready)))
		s := r.start()
		s.waitForFile(ready)
		if worktrees := r.worktrees(); len(worktrees) != 2 {
			t.Fatalf("while the build ran the integration repository lists the worktrees %v, want its own and the build's", worktrees)
		}

		s.interrupt()
		requireStopped(t, r, s.wait(time.Minute))
		if files := dirSnapshot(t, r.outDir); len(files) != 0 {
			t.Errorf("a run interrupted before it began wrote %d files into its directory", len(files))
		}
		if !emptyOrAbsent(t, r.workDir) {
			t.Errorf("a run interrupted before it began left something in its work directory")
		}

		// Nothing was left that stands in the way of the next start.
		requireCompleted(t, "the run after the interrupt", r.run())
	})

	t.Run("during a step", func(t *testing.T) {
		clean := cleanBaseline(t, "lifecycle", o, "")
		r := newCLIRig(t, o)
		ready := r.mark("step-ready")
		r.install(onCall("create*Beta*", once(r.mark("step-held"), hold(ready))), "", "")
		s := r.start()
		s.waitForFile(ready)

		s.interrupt()
		requireStopped(t, r, s.wait(time.Minute))
		id := r.runID("")
		if id == "" {
			t.Fatalf("the interrupted run left no %s to name it", runFile)
		}
		for _, row := range jsonRows(t, r.outPath(resultsFile)) {
			if memberString(row, "verdict") == string(VerdictRejected) {
				t.Errorf("the interrupt was recorded as bd refusing the step: %v", row)
			}
		}
		if step, _, _ := r.unfinished(); step != 1 {
			t.Errorf("the journal ends with step %d started, want step 1, the create of Beta", step)
		}

		if got := requireCompleted(t, "the resumed run", r.run("--resume", id)); got != id {
			t.Fatalf("the resume made a run of its own, %s, instead of finishing %s", got, id)
		}
		requireSameOutcome(t, "the run resumed after the interrupt", clean.files, outcome(t, r.outDir, id))
	})
}

// B7.ResumeRestoresRunState: a resume restores the whole of the run's state, not
// only where it was. Early steps quarantine two issues, one because the change
// cannot be translated and one because bd refuses it, and steps after the kill
// touch both; an early step changes a table bd rewrites on its own and one shows a
// schema difference, which are counted as the run goes and are not in any row; the
// run is killed after the quarantining steps. The resumed run's rows and summary
// are a clean run's, and steps that leave no result row cannot mislead where it
// resumes.
func TestB7ResumeRestoresRunState(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := restoreOracle(t)
	clean := cleanBaseline(t, "restore", o, refuseUpdate)

	// The fixture has to have something to lose, or a resume that restored nothing
	// would pass.
	sum := clean.summary
	if len(sum.Derived) == 0 {
		t.Fatalf("the clean run counted no derived table: %+v", sum)
	}
	if len(sum.SchemaSkew) == 0 {
		t.Fatalf("the clean run saw no schema skew: %+v", sum)
	}
	for verdict, want := range map[Verdict]int{VerdictUntranslatable: 1, VerdictRejected: 1, VerdictSkippedQuarantined: 2} {
		if got := sum.Verdicts[string(verdict)]; got != want {
			t.Fatalf("the clean run has %d %s rows, want %d: %+v", got, verdict, want, sum)
		}
	}
	if sum.Gaps.Columns["issues.owner"] < 1 || sum.Gaps.Tables["labels"] < 1 {
		t.Fatalf("the clean run's gaps are %+v, want the owner column and the labels table", sum.Gaps)
	}

	r := newCLIRig(t, o)
	r.install(refuseUpdate, onCall("create*Gamma*", once(r.mark("killed"), killDriver)), "")
	id := r.runKilled()
	if step, _, _ := r.unfinished(); step != 6 {
		t.Fatalf("the run was killed in step %d, want the create of Gamma, step 6", step)
	}

	// Steps 4 and 5 finished before the kill and left no result row: a resume that
	// went by the rows would start after step 3 and count them again.
	if got := requireCompleted(t, "the resumed run", r.run("--resume", id)); got != id {
		t.Fatalf("the resume made a run of its own, %s, instead of finishing %s", got, id)
	}
	requireSameOutcome(t, "the resumed run", clean.files, outcome(t, r.outDir, id))
}
