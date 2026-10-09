package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// The replay on a seeded copy. The first tests read the shared flow: one oracle,
// one seeding of it and one replay of the tail on the copy, with the variables a
// careless harness would pass on to its children all set. The rest build their
// own run where they need a different store.

// ---- B8.SeededReplay --------------------------------------------------------------

// B8.SeededReplay: a run that starts from a seeded copy replays the oracle's tail
// and every step matches. The seeded rows are legacy by design, so the run records
// versions only for the rows it creates, and it leaves the oracle as it found it.
func TestB8SeededReplay(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	replaytest.Isolate(t)

	if f.run.Status != "completed" {
		t.Errorf("the run's status is %q, want completed", f.run.Status)
	}
	if len(f.results) != 3 {
		t.Fatalf("the run wrote %d result rows, want one for each of the tail's three commits", len(f.results))
	}
	for _, r := range f.results {
		if r.Verdict != VerdictMatched {
			t.Errorf("issue %s at %s: verdict %q (%s), want matched", r.IssueID, r.SourceCommit, r.Verdict, r.MutationKind)
		}
	}
	if len(f.mismatches) != 0 {
		t.Errorf("the run recorded %d mismatches, want none", len(f.mismatches))
	}

	// The steps the observer heard are the walk's own, in order, and the first is the
	// first commit after the seeded base.
	if len(f.walkSteps) != 3 || len(f.steps) != 3 {
		t.Fatalf("the walk has %d steps and the observer heard %d, want 3 and 3", len(f.walkSteps), len(f.steps))
	}
	for i, ev := range f.steps {
		st := f.walkSteps[i]
		if ev.Step != i || ev.Step != st.Index {
			t.Errorf("event %d is for step %d, the walk's step is %d", i, ev.Step, st.Index)
		}
		if ev.Result.FromCommit != st.From.Hash || ev.Result.SourceCommit != st.To.Hash {
			t.Errorf("step %d replayed %s..%s, the walk's step is %s..%s", i, ev.Result.FromCommit, ev.Result.SourceCommit, st.From.Hash, st.To.Hash)
		}
	}
	if got := f.summary.CoveredRange.BaseCommit; got != f.o.base {
		t.Errorf("the covered range starts at %s, want the seeded base %s", got, f.o.base)
	}
	if got := f.summary.CoveredRange.CommitsReplayed; got != 3 {
		t.Errorf("the summary says %d commits were replayed, want 3", got)
	}

	// The three seeded rows and the one the tail creates. The seeded rows are legacy:
	// the tail updates two of them, and a write on a legacy row is not versioned.
	if f.workIssues != 4 {
		t.Errorf("the work store holds %d issues, want the 3 seeded and the 1 the tail creates", f.workIssues)
	}
	if f.legacyRows != 3 {
		t.Errorf("%d rows have no participation generation, want the 3 seeded rows and no other", f.legacyRows)
	}

	if f.summary.Seed == nil {
		t.Fatalf("the summary of a seeded run does not carry the seed record")
	}
	if *f.summary.Seed != f.rec {
		t.Errorf("the summary's seed record is %+v, the seeding returned %+v", *f.summary.Seed, f.rec)
	}
	if n := seedQueryInt(t, f.runCfg.WorkDataDir, "SELECT COUNT(*) FROM dolt_log WHERE commit_hash = '"+f.rec.SeedHead+"'"); n != 1 {
		t.Errorf("the seed head %s is in the work store's history %d times, want once", f.rec.SeedHead, n)
	}
	if len(f.summary.SchemaSkew) == 0 {
		t.Errorf("the summary records no schema skew: the oracle is older than the candidate by construction, and the difference is recorded, not a mismatch")
	}
	sameTree(t, "the oracle", f.oracleBefore, f.oracleAfter)
}

// ---- B8.BaselineCompare -----------------------------------------------------------

// B8.BaselineCompare: before the first step, every issue the seed holds is
// compared with the oracle's at the base. The comparison is a count in the summary
// and nothing else: it writes no result row, takes no step number and names no
// issue.
func TestB8BaselineCompare(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	replaytest.Isolate(t)

	b := f.summary.Baseline
	if b == nil {
		t.Fatalf("the summary of a seeded run has no baseline: %s", f.summaryRaw)
	}
	if want := len(f.o.ids); b.Compared != want || b.Differing != 0 {
		t.Errorf("the baseline is %+v, want %d compared and none differing", *b, want)
	}
	if len(f.results) != 3 {
		t.Errorf("%d result rows: the baseline must add none to the tail's three", len(f.results))
	}
	for i, ev := range f.steps {
		if ev.Step != i {
			t.Errorf("the observer's event %d is for step %d: the baseline must not take a step number", i, ev.Step)
		}
	}
	keys, err := jsonKeys(f.summary)
	if err != nil {
		t.Fatalf("the summary's keys: %v", err)
	}
	if !slices.Contains(keys, "baseline") || slices.Contains(keys, "enable_refused") {
		t.Errorf("a seeded run's summary has the keys %q, want baseline and not enable_refused", keys)
	}
	plain, err := jsonKeys(Summary{})
	if err != nil {
		t.Fatalf("an unseeded summary's keys: %v", err)
	}
	for _, k := range []string{"seed", "baseline", "enable_refused"} {
		if slices.Contains(plain, k) {
			t.Errorf("an unseeded run's summary carries %q", k)
		}
	}

	// Control: a seeded issue the tail never touches is changed in the copy, in a
	// commit of its own. The baseline counts it, the tail still matches, and the
	// summary names neither the issue nor the text.
	tampered := filepath.Join(t.TempDir(), "work")
	copyProject(t, f.cfg.WorkDir, tampered)
	data := replaytest.DataDir(t, tampered)
	untouched := f.o.ids[2]
	replaytest.RunDolt(t, data, "sql", "-q", "UPDATE issues SET title = 'changed in the copy' WHERE id = '"+untouched+"'")
	replaytest.RunDolt(t, data, "add", "-A")
	replaytest.RunDolt(t, data, "commit", "-m", "change a seeded issue")

	cfg := f.runCfg
	cfg.WorkDir, cfg.WorkDataDir = tampered, data
	cfg.OutDir = filepath.Join(t.TempDir(), "out")
	cfg.Tools = Tools{IntegrationBin: replaytest.BdBin(t)}
	rec := f.rec
	cfg.Seed = &rec
	cfg.Observer = nil
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("the run on the changed copy: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.OutDir, "summary.json")) // #nosec G304 -- this test's own output
	if err != nil {
		t.Fatalf("reading the changed copy's summary: %v", err)
	}
	var s Summary
	if err := unmarshalStrict(raw, &s); err != nil {
		t.Fatalf("the changed copy's summary: %v\n%s", err, raw)
	}
	if s.Baseline == nil || s.Baseline.Compared != len(f.o.ids) || s.Baseline.Differing != 1 {
		t.Errorf("the baseline of the changed copy is %+v, want %d compared and 1 differing", s.Baseline, len(f.o.ids))
	}
	for _, r := range readCommitReplayResults(t, cfg.OutDir) {
		if r.Verdict != VerdictMatched {
			t.Errorf("issue %s at %s: verdict %q on the changed copy, want the tail to match", r.IssueID, r.SourceCommit, r.Verdict)
		}
	}
	for _, leak := range []string{untouched, "changed in the copy"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the summary of the changed copy contains %q: the baseline is counts only", leak)
		}
	}
}

// ---- B8.EnableRefusedRecorded -----------------------------------------------------

// B8.EnableRefusedRecorded (O2): a seeded store can hold a value a version could
// not record, and the product then refuses to turn versioned history on. The run
// records that, as a count, and replays the tail without versioning. It neither
// fails nor passes silently, and the product's own report, which names the issue,
// is not copied.
func TestB8EnableRefusedRecorded(t *testing.T) {
	f := requireSeededRun(t, bigNumberVariant)
	replaytest.Isolate(t)

	if f.run.Status != "completed" {
		t.Errorf("the run's status is %q, want completed: a refusal to turn versioning on is recorded and the run goes on", f.run.Status)
	}
	e := f.summary.EnableRefused
	if e == nil || e.Count != 1 {
		t.Fatalf("the summary's enable_refused is %+v, want a count of 1\n%s", e, f.summaryRaw)
	}
	if n := seedQueryInt(t, f.runCfg.WorkDataDir, "SELECT COUNT(*) FROM config WHERE `key` = 'versioned-history.enabled' AND value = 'true'"); n != 0 {
		t.Errorf("versioned history was turned on in a store the product refused it for")
	}
	if len(f.results) != 3 {
		t.Fatalf("the run wrote %d result rows, want 3", len(f.results))
	}
	for _, r := range f.results {
		if r.Verdict != VerdictMatched {
			t.Errorf("issue %s at %s: verdict %q, want matched", r.IssueID, r.SourceCommit, r.Verdict)
		}
	}
	for _, id := range f.o.ids {
		if strings.Contains(string(f.summaryRaw), id) {
			t.Errorf("the summary names the issue %s: the refusal is recorded as a count", id)
		}
	}
	if strings.Contains(string(f.summaryRaw), "versioned history on") {
		t.Errorf("the summary carries the product's own text: %s", f.summaryRaw)
	}
}

// ---- B8.NoRemotesAfterRun ---------------------------------------------------------

// seedPlantingStandIn is a bd stand-in that, once the second create has finished,
// adds an outbound target to the store the run writes to: as a candidate that left
// one behind would. Nothing writes after the second create of the fault history, so
// nothing could push to it. It returns the stand-in.
func seedPlantingStandIn(t testing.TB, realBd, kind string) string {
	t.Helper()
	dest := "file://" + t.TempDir()
	add := map[string]string{
		"remote": "dolt remote add origin " + shellQuote(dest),
		"backup": "dolt backup add bk " + shellQuote(dest),
	}[kind]
	if add == "" {
		t.Fatalf("no outbound target of kind %q", kind)
	}
	after := `if [ "$cmd" = create ] && [ "$(grep -c '^create ' "$LOG")" -eq 2 ]; then
  for d in .beads/embeddeddolt/*/; do
    (cd "$d" && ` + add + `) >/dev/null 2>&1
  done
fi`
	bin, _ := bdStandInAfter(t, realBd, "", after)
	return bin
}

// B8.NoRemotesAfterRun (H15): a run ends with no remote and no backup on the work
// clone, and a run whose clone ends with one is a harness fault, not a result. The
// check runs at the end of every run, seeded or not.
func TestB8NoRemotesAfterRun(t *testing.T) {
	t.Run("a seeded run ends with none", func(t *testing.T) {
		f := requireSeededRun(t, defaultSeedVariant)
		if f.rec.RemotesStripped != 1 || f.rec.BackupsStripped != 1 {
			t.Errorf("the seeding stripped %d remotes and %d backups, want the 1 and 1 the oracle had", f.rec.RemotesStripped, f.rec.BackupsStripped)
		}
		if len(f.remotesAfter) != 0 || len(f.backupsAfter) != 0 {
			t.Errorf("the work clone ends the run with remotes %q and backups %q", f.remotesAfter, f.backupsAfter)
		}
	})
	for _, c := range []struct{ kind, class string }{
		{"remote", RefusedRemotes},
		{"backup", RefusedBackups},
	} {
		t.Run("a "+c.kind+" left by the candidate ends the run", func(t *testing.T) {
			requireBd(t)
			requireDolt(t)
			o := faultOracle(t)
			f := newFixtureRun(t, o, seedPlantingStandIn(t, o.bin, c.kind))
			run, err := f.run()
			var sr *SeedRefused
			if !errors.As(err, &sr) {
				t.Fatalf("the run returned %v, want a refusal naming the %s", err, c.kind)
			}
			if sr.Class != c.class || !sr.HarnessFault() {
				t.Errorf("the refusal is %s (harness fault %v), want %s as a harness fault", sr.Class, sr.HarnessFault(), c.class)
			}
			if run.Status != "failed" {
				t.Errorf("the run's status is %q, want failed", run.Status)
			}
			planted := seedRemotes(t, f.workData)
			if c.kind == "backup" {
				planted = seedBackups(t, f.workData)
			}
			if len(planted) != 1 {
				t.Errorf("the work clone holds %d %ss, so the control did not plant the one it stands for", len(planted), c.kind)
			}
		})
	}
}

// ---- B8.BackupDestinationUntouched ------------------------------------------------

// B8.BackupDestinationUntouched: the oracle was backed up to a destination, the
// copy inherits the backup's name, and a backup sync from the copy would overwrite
// the destination with the copy's state. The destination's files must be as they
// were before the seeding and after the run.
func TestB8BackupDestinationUntouched(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	if len(f.backupBefore) == 0 {
		t.Fatalf("the backup destination holds no files, so an untouched one proves nothing")
	}
	sameTree(t, "the backup destination", f.backupBefore, f.backupAfter)
}

// ---- B8.NeverTouchesSharedServer --------------------------------------------------

// B8.NeverTouchesSharedServer (NFR1): the flow ran with the variables that name a
// shared dolt server set to a listener of its own, and the listener heard nothing.
// Every child the seeding started ran in the directory the seeding owns, and none
// in the oracle's.
func TestB8NeverTouchesSharedServer(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	if !f.decoyListening {
		t.Fatalf("the flow had no decoy listener, so a silent one proves nothing")
	}
	if f.decoyAccepted != 0 {
		t.Errorf("the decoy server heard %d connections", f.decoyAccepted)
	}
	if len(f.children) == 0 {
		t.Fatalf("the seeding started no children")
	}
	for i, c := range f.children {
		if !seedUnder(c.Dir, f.cfg.WorkDir) && !seedUnder(c.Dir, f.cfg.EnvRoot) {
			t.Errorf("child %d (%s %q) ran in %s, outside the seeding's own directories", i, c.Tool, c.Args, c.Dir)
		}
		if seedUnder(c.Dir, f.o.root) {
			t.Errorf("child %d (%s %q) ran inside the oracle's directory %s", i, c.Tool, c.Args, c.Dir)
		}
	}
}
