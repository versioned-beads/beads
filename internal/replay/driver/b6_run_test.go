package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
	"github.com/steveyegge/beads/internal/replay/translate"
)

// issueMember reads one member of a stored payload: whether the key is there and
// what it holds, so a test can tell JSON null from a missing key from "".
func issueMember(t *testing.T, payload json.RawMessage, key string) (value any, present bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("stored payload is not a JSON object: %v\n%s", err, payload)
	}
	value, present = m[key]
	return value, present
}

// mismatchFor returns the row-side mismatch of an issue at a step, and fails the
// test if there is none or more than one. A candidate that is wrong on the rows
// is usually wrong in the versions it stored too, and that finding is a row of its
// own, category version-state, which versionMismatchFor reads.
func mismatchFor(t *testing.T, f *fixtureRun, commit, issue string) Mismatch {
	t.Helper()
	var found []Mismatch
	for _, m := range f.mismatches() {
		if m.SourceCommit == commit && m.IssueID == issue && m.Category != compare.CategoryVersionState {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d row mismatches for %s at %s, want exactly 1 (all: %+v)", len(found), issue, commit, f.mismatches())
	}
	return found[0]
}

func mustComplete(t *testing.T, f *fixtureRun) ReplayRun {
	t.Helper()
	run, err := f.run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("run.Status = %q, want completed", run.Status)
	}
	return run
}

// B6.CreateWithDepsOnce: an issue created with an edge is one step and one plan,
// so bd is asked to create it once. Replayed a row at a time, the issue row and
// each of its edge rows were each a "create".
func TestB6CreateWithDepsOnce(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	a, b := o.ids[0], o.ids[1]
	bin, log := bdStandIn(t, o.bin, "")
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f)

	calls := standInCalls(t, log)
	if got := countCalls(calls, "create ", b); got != 1 {
		t.Errorf("bd was asked to create %s %d times, want once; calls:\n%s", b, got, strings.Join(calls, "\n"))
	}
	if got := countCalls(calls, "create "); got != 2 {
		t.Errorf("bd was asked to create %d times, want twice (%s and %s)", got, a, b)
	}
	if got := countCalls(calls, "dep ", b, a); got != 1 {
		t.Errorf("the edge from %s to %s was added %d times, want once", b, a, got)
	}
	for _, r := range f.results() {
		if r.Verdict != VerdictMatched {
			t.Errorf("a faithful replay of %s at %s is %q", r.IssueID, r.SourceCommit, r.Verdict)
		}
	}
	if got := f.resultFor(o.heads[1], b).MutationKind; got != "create+dep_add" {
		t.Errorf("mutation kind of the create with an edge = %q, want create+dep_add", got)
	}
}

// B6.DepDropDetected: a bd that quietly drops its dep subcommands leaves the
// candidate without the edge, and the dependencies section of the view catches
// it. Replayed on the plain row, this was recorded as matched.
func TestB6DepDropDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	b := o.ids[1]
	bin, _ := bdStandIn(t, o.bin, `case "$1" in dep) exit 0 ;; esac`)
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f) // a mismatch is a result, never a reason to stop

	row := f.resultFor(o.heads[1], b)
	if row.Verdict != VerdictMismatch || row.Matched {
		t.Fatalf("%s at %s = %q matched=%v, want a mismatch: its edge was dropped", b, o.heads[1], row.Verdict, row.Matched)
	}
	if got := mismatchFor(t, f, o.heads[1], b).Category; got != "dep-edge" {
		t.Errorf("mismatch category = %q, want dep-edge", got)
	}
}

// B6.MismatchPathRuns: a bd that alters a column makes the driver itself record
// the mismatch, with both payloads. The path used to be exercised only by a test
// that wrote the mismatch row by hand.
func TestB6MismatchPathRuns(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	a := o.ids[0]
	// Every create gets "-altered" appended to its title, by rotating the argument
	// list once and changing the value that follows --title.
	bin, _ := bdStandIn(t, o.bin, `if [ "$1" = create ]; then
	n=$#
	prev=""
	while [ "$n" -gt 0 ]; do
		arg="$1"
		shift
		n=$((n - 1))
		orig="$arg"
		if [ "$prev" = "--title" ]; then arg="$arg-altered"; fi
		prev="$orig"
		set -- "$@" "$arg"
	done
fi`)
	f := newFixtureRun(t, o, bin)
	run := mustComplete(t, f)

	row := f.resultFor(o.heads[0], a)
	if row.Verdict != VerdictMismatch || row.Matched {
		t.Fatalf("%s at %s = %q, want a mismatch", a, o.heads[0], row.Verdict)
	}
	m := mismatchFor(t, f, o.heads[0], a)
	if m.RunID != run.ID {
		t.Errorf("mismatch run id = %q, want %q", m.RunID, run.ID)
	}
	if got, _ := issueMember(t, m.ExpectedJSON, "title"); got != "Alpha" {
		t.Errorf("expected payload title = %v, want Alpha", got)
	}
	if got, _ := issueMember(t, m.ActualJSON, "title"); got != "Alpha-altered" {
		t.Errorf("actual payload title = %v, want Alpha-altered", got)
	}
}

// B6.DriverNullVsEmpty: a candidate that differs from the oracle only as NULL
// versus the empty string is a mismatch, recorded by the driver itself. bd has no
// flag that turns a NULL column into the empty string (it stores NULL for an
// empty assignee), so the stand-in does what a candidate with a defect of its own
// would: once the real bd has created an issue, it sets the empty string in the
// column the oracle holds NULL in, and commits. Read as text, both are "".
func TestB6DriverNullVsEmpty(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	a := o.ids[0]
	f := newFixtureRun(t, o, o.bin)
	dolt, err := doltcli.Path()
	if err != nil {
		t.Fatalf("dolt: %v", err)
	}
	bin, _ := bdStandInAfter(t, o.bin, "", `if [ "$rc" -eq 0 ] && [ "$cmd" = create ]; then
	cd `+shellQuote(f.workData)+` || exit 1
	export DOLT_ROOT_PATH=`+shellQuote(os.Getenv("DOLT_ROOT_PATH"))+` DOLT_DISABLE_EVENT_FLUSH=1
	`+shellQuote(dolt)+` sql -q "UPDATE issues SET assignee = '' WHERE assignee IS NULL" || exit 1
	`+shellQuote(dolt)+` add -A || exit 1
	`+shellQuote(dolt)+` commit -m "stand-in: an empty assignee" || exit 1
fi`)
	f.cfg.Tools.IntegrationBin = bin
	mustComplete(t, f)

	row := f.resultFor(o.heads[0], a)
	if row.Verdict != VerdictMismatch || row.Matched {
		t.Fatalf("%s at %s = %q, want a mismatch: NULL is not the empty string", a, o.heads[0], row.Verdict)
	}
	m := mismatchFor(t, f, o.heads[0], a)
	if got, present := issueMember(t, m.ExpectedJSON, "assignee"); !present || got != nil {
		t.Errorf("expected payload assignee = %v (present=%v), want JSON null", got, present)
	}
	if got, present := issueMember(t, m.ActualJSON, "assignee"); !present || got != "" {
		t.Errorf("actual payload assignee = %v (present=%v), want the empty string", got, present)
	}
}

// B6.VersioningOn: the work clone records versions. Versioned history is off
// unless something turns it on, so the driver does, before the first replayed
// action; after the run the work clone holds version rows and no legacy row (a
// row created while history was off never records).
func TestB6VersioningOn(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	bin, log := bdStandIn(t, o.bin, "")
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f)

	if got := f.workCount("SELECT COUNT(*) FROM issue_versions"); got == 0 {
		t.Error("issue_versions is empty after a run: versioned history was not on")
	}
	if got := f.workCount("SELECT COUNT(*) FROM issues WHERE participation_generation IS NULL"); got != 0 {
		t.Errorf("%d issues were created while history was off", got)
	}
	calls := standInCalls(t, log)
	const enable = "config set versioned-history.enabled true"
	if len(calls) == 0 || calls[0] != enable {
		t.Fatalf("the first bd call = %q, want %q: history must be on before any issue is created", firstOr(calls, "<none>"), enable)
	}
	if got := countCalls(calls, enable); got != 1 {
		t.Errorf("history was switched on %d times, want once", got)
	}
}

func firstOr(s []string, def string) string {
	if len(s) == 0 {
		return def
	}
	return s[0]
}

// B6.VersioningOn, negative control: if the switch silently does not take, every
// row is legacy and the run says so as a harness error, never as a finding.
func TestB6VersioningOffIsAHarnessError(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	bin, _ := bdStandIn(t, o.bin, `case "$1" in config) exit 0 ;; esac`)
	f := newFixtureRun(t, o, bin)

	run, err := f.run()
	if !errors.Is(err, ErrLegacyRowsPresent) {
		t.Fatalf("err = %v, want ErrLegacyRowsPresent", err)
	}
	if run.Status != "failed" {
		t.Errorf("run.Status = %q, want failed", run.Status)
	}
	if got := f.workCount("SELECT COUNT(*) FROM issues WHERE participation_generation IS NULL"); got == 0 {
		t.Error("the control did not make any legacy row, so it proves nothing")
	}
	if s := f.summary(); s.Status != "failed" {
		t.Errorf("summary status = %q, want failed", s.Status)
	}
}

// B6.Rejected: a bd action that exits non-zero is recorded as rejected with what
// bd said, the issue is quarantined, a later step that touches it is recorded
// skipped-quarantined, and the run goes on and completes.
func TestB6Rejected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := rejectOracle(t)
	a, b := o.ids[0], o.ids[1]
	bin, _ := bdStandIn(t, o.bin, `case "$1" in close) echo "refusing to close" >&2; exit 1 ;; esac`)
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f)

	closed := f.resultFor(o.heads[2], a)
	if closed.Verdict != VerdictRejected || closed.Matched {
		t.Fatalf("the refused close of %s = %q, want rejected", a, closed.Verdict)
	}
	d := closed.Detail
	if d == nil || d.ExitCode != 1 || !strings.Contains(d.Output, "refusing to close") || len(d.Argv) == 0 || d.Argv[0] != "close" {
		t.Errorf("rejection detail = %+v, want the argv (starting with close), exit code 1 and bd's output", d)
	}

	later := f.resultFor(o.heads[3], a)
	if later.Verdict != VerdictSkippedQuarantined {
		t.Errorf("a later update of the rejected %s = %q, want skipped-quarantined", a, later.Verdict)
	}
	if later.Detail == nil || later.Detail.QuarantinedAt != o.heads[2] {
		t.Errorf("skipped row detail = %+v, want it to name %s, the step that quarantined %s", later.Detail, o.heads[2], a)
	}

	if got := f.resultFor(o.heads[4], b).Verdict; got != VerdictMatched {
		t.Errorf("the update of %s = %q, want matched: a rejection elsewhere changes nothing for it", b, got)
	}
	for i, h := range o.heads[:2] {
		if got := f.resultFor(h, o.ids[i]).Verdict; got != VerdictMatched {
			t.Errorf("step %d = %q, want matched", i, got)
		}
	}

	s := f.summary()
	if s.Verdicts["rejected"] != 1 || s.Verdicts["skipped-quarantined"] != 1 || s.Verdicts["matched"] != 3 {
		t.Errorf("summary verdicts = %v, want matched 3, rejected 1, skipped-quarantined 1", s.Verdicts)
	}
}

// B6.Rejected, second half: a bd that is killed by a signal is not a refusal. It
// is an infrastructure failure and the run fails, with no row for the step.
func TestB6SignalKilledBdFailsTheRun(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := rejectOracle(t)
	bin, _ := bdStandIn(t, o.bin, `case "$1" in close) kill -9 $$ ;; esac`)
	f := newFixtureRun(t, o, bin)

	run, err := f.run()
	if err == nil {
		t.Fatal("the run succeeded although bd was killed mid-step")
	}
	var refused *translate.ExecError
	if errors.As(err, &refused) {
		t.Errorf("a signal-killed bd was read as a refusal: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("run.Status = %q, want failed", run.Status)
	}
	if got := len(f.results()); got != 2 {
		t.Errorf("%d rows written, want the 2 steps before the kill and nothing for the step that died", got)
	}
	if s := f.summary(); s.Status != "failed" {
		t.Errorf("summary status = %q, want failed", s.Status)
	}
}

// B6.ExistenceVerdicts: whether an issue exists is decided on the views, and the
// driver reports the category CompareViews returns without adding one. A create
// the candidate swallowed is issue-missing; a delete it swallowed is issue-extra;
// a delete both sides performed matches; none is uncomparable, and none counts
// toward the number-fidelity total.
func TestB6ExistenceVerdicts(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := newOracle(t, t.TempDir(), "oracle")
	x := o.create("Xray")
	y := o.create("Yankee")
	z := o.create("Zulu")
	o.run("delete", x, "--force")
	o.run("delete", y, "--force")

	bin, _ := bdStandIn(t, o.bin, `case "$1" in
create) case " $* " in *" `+z+` "*) exit 0 ;; esac ;;
delete) case " $* " in *" `+x+` "*) exit 0 ;; esac ;;
esac`)
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f)

	cases := []struct {
		name     string
		commit   string
		issue    string
		verdict  Verdict
		category string
	}{
		{"create both sides performed", o.heads[0], x, VerdictMatched, ""},
		{"create both sides performed (second)", o.heads[1], y, VerdictMatched, ""},
		{"a create the candidate swallowed", o.heads[2], z, VerdictMismatch, "issue-missing"},
		{"a delete the candidate swallowed", o.heads[3], x, VerdictMismatch, "issue-extra"},
		{"a delete both sides performed", o.heads[4], y, VerdictMatched, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := f.resultFor(tc.commit, tc.issue)
			if row.Verdict != tc.verdict {
				t.Fatalf("verdict = %q, want %q", row.Verdict, tc.verdict)
			}
			if tc.category != "" {
				if got := mismatchFor(t, f, tc.commit, tc.issue).Category; got != tc.category {
					t.Errorf("stored category = %q, want %q", got, tc.category)
				}
			}
		})
	}

	for _, r := range f.results() {
		if r.Verdict == VerdictUncomparable {
			t.Errorf("%s at %s is uncomparable: an existence difference never is", r.IssueID, r.SourceCommit)
		}
	}
	s := f.summary()
	if s.NumberFidelity != 0 {
		t.Errorf("number_fidelity = %d, want 0: no number was refused", s.NumberFidelity)
	}
	if s.Verdicts["mismatch"] != 2 || s.Verdicts["matched"] != 3 || s.Verdicts["uncomparable"] != 0 {
		t.Errorf("summary verdicts = %v, want matched 3, mismatch 2, uncomparable 0", s.Verdicts)
	}
}

// B6.SeedGuard: a base that already holds issues, with no seed, is refused before
// any write. Replaying into an empty clone would fail on the first touch of an
// existing issue, mid-run, with a message about the wrong thing.
func TestB6SeedGuard(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	// An issue, then a commit with a bootstrap message: the base is the last
	// bootstrap commit on the chain, so the base holds the issue.
	o := newOracle(t, t.TempDir(), "oracle")
	o.create("Already in the base")
	replaytest.RunDolt(t, o.data, "commit", "--allow-empty", "-m", "schema: apply migration 9999_test.up.sql")
	o.create("After the base")

	t.Run("Run refuses before any write", func(t *testing.T) {
		bin, log := bdStandIn(t, o.bin, "")
		f := newFixtureRun(t, o, bin)
		headBefore := replaytest.HeadCommit(t, f.workData)

		run, err := f.run()
		if !errors.Is(err, ErrBaseNotSeeded) {
			t.Fatalf("err = %v, want ErrBaseNotSeeded", err)
		}
		if run.ID != "" {
			t.Errorf("a refused run has an id: %+v", run)
		}
		if _, statErr := os.Stat(f.outDir); !os.IsNotExist(statErr) {
			t.Errorf("the output directory exists after a refusal (stat err = %v)", statErr)
		}
		if calls := standInCalls(t, log); len(calls) != 0 {
			t.Errorf("bd was started %d times before the refusal: %v", len(calls), calls)
		}
		if got := replaytest.HeadCommit(t, f.workData); got != headBefore {
			t.Errorf("the work clone's head moved from %s to %s", headBefore, got)
		}
		if got := f.workCount("SELECT COUNT(*) FROM config WHERE `key` = 'versioned-history.enabled'"); got != 0 {
			t.Errorf("history was switched on in the work clone before the refusal")
		}
	})

	t.Run("Options.Run refuses before building or initialising anything", func(t *testing.T) {
		work := filepath.Join(t.TempDir(), "work")
		out := filepath.Join(t.TempDir(), "out")
		_, err := Options{
			IntegrationRef:  "no-such-ref",
			IntegrationRepo: t.TempDir(), // not a repository: a build would fail first, with a git error
			OracleDataDir:   o.data,
			WorkDir:         work,
			OutDir:          out,
		}.Run(context.Background())
		if !errors.Is(err, ErrBaseNotSeeded) {
			t.Fatalf("err = %v, want ErrBaseNotSeeded", err)
		}
		for _, p := range []string{work, out} {
			if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
				t.Errorf("%s exists after a refusal (stat err = %v)", p, statErr)
			}
		}
	})

	t.Run("a seed lets the base through", func(t *testing.T) {
		w, err := ReadWalk(context.Background(), o.data)
		if err != nil {
			t.Fatalf("ReadWalk: %v", err)
		}
		if err := checkSeedGuard(context.Background(), o.data, w, &SeedRecord{SeedHead: "seeded"}); err != nil {
			t.Errorf("checkSeedGuard with a seed = %v, want nil", err)
		}
		if err := checkSeedGuard(context.Background(), o.data, w, nil); !errors.Is(err, ErrBaseNotSeeded) {
			t.Errorf("checkSeedGuard without a seed = %v, want ErrBaseNotSeeded", err)
		}
	})

	t.Run("an empty base needs no seed", func(t *testing.T) {
		empty := faultOracle(t)
		w, err := ReadWalk(context.Background(), empty.data)
		if err != nil {
			t.Fatalf("ReadWalk: %v", err)
		}
		if err := checkSeedGuard(context.Background(), empty.data, w, nil); err != nil {
			t.Errorf("checkSeedGuard on a base with no issues = %v, want nil", err)
		}
	})
}
