// B11b: the ledger, over a real bd.
//
// Ledger.Delta reads what one action did to the work clone from Dolt's own diff
// between the clone's heads around it: which issues really changed, the version rows
// that were added, the fence marker and the revision counter before and after. These
// tests make real bd write, pinned the way the harness pins every write, and hold the
// ledger to what bd did; they then run the delta through Evaluate, so a faithful bd is
// shown to produce no finding at all. The rules themselves are pinned over synthetic
// deltas in b11b_evaluate_test.go.
package versions

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// b11bLedgerActor is the actor every write in these tests is pinned to, as the harness
// pins its own. The clock tolerance is far wider than the skew between a test and the
// bd it started on the same host.
const (
	b11bLedgerActor    = "ledger-actor"
	b11bClockTolerance = 2 * time.Second
)

// b11bPinned adds the two flags the harness sets on every write to a bd argument
// list: before the first "--" terminator when there is one, at the end when there is
// not, so the subcommand stays where bd looks for it.
func b11bPinned(actor string, args []string) []string {
	flags := []string{"--dolt-auto-commit=on", "--actor=" + actor}
	for i, arg := range args {
		if arg == "--" {
			out := append([]string{}, args[:i]...)
			out = append(out, flags...)
			return append(out, args[i:]...)
		}
	}
	return append(append([]string{}, args...), flags...)
}

// b11bStep is one bd write with the work clone's heads and the clock around it.
type b11bStep struct {
	Before, After string
	Start, End    time.Time
	Out           string
}

func b11bRun(s *b11aStore, actor string, args ...string) b11bStep {
	s.t.Helper()
	st := b11bStep{Before: s.head()}
	st.Start = time.Now().UTC()
	st.Out = s.bd(b11bPinned(actor, args)...)
	st.End = time.Now().UTC()
	st.After = s.head()
	return st
}

// b11bMake creates an issue as actor and returns its id with the step that made it.
func b11bMake(s *b11aStore, actor, title string, extra ...string) (string, b11bStep) {
	s.t.Helper()
	st := b11bRun(s, actor, append(append([]string{"create", title}, extra...), "--json")...)
	return replaytest.JSONID(s.t, st.Out), st
}

func b11bDeltaBetween(t *testing.T, s *b11aStore, from, to string) Delta {
	t.Helper()
	d, err := Ledger{}.Delta(context.Background(), s.data, from, to)
	if err != nil {
		t.Fatalf("Delta(%s, %s): %v", from, to, err)
	}
	return d
}

func b11bDeltaOf(t *testing.T, s *b11aStore, st b11bStep) Delta {
	t.Helper()
	return b11bDeltaBetween(t, s, st.Before, st.After)
}

func b11bIDs(d Delta) []string {
	ids := []string{}
	for _, i := range d.Issues {
		ids = append(ids, i.ID)
	}
	return ids
}

func b11bIssueIn(t *testing.T, d Delta, id string) IssueDelta {
	t.Helper()
	issue, ok := d.Issue(id)
	if !ok {
		t.Fatalf("the delta has no entry for %s; it has %v", id, b11bIDs(d))
	}
	return issue
}

// b11bQuiet fails the test if the delta says an issue changed or was given a row. An
// issue the diff does not mention at all is quiet.
func b11bQuiet(t *testing.T, d Delta, id string) {
	t.Helper()
	if issue, ok := d.Issue(id); ok && (issue.Real || len(issue.Rows) > 0 || issue.Lost > 0 || issue.Altered > 0) {
		t.Errorf("%s should be quiet, and the delta says: real %v, sections %v, rows %+v", id, issue.Real, issue.Sections, issue.Rows)
	}
}

func b11bSameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// b11bJudge runs a real step's delta through the evaluator as the harness will.
func b11bJudge(kind, issue, actor string, st b11bStep, d Delta) Result {
	action := Action{Kind: kind, Issue: issue, Actor: actor, Start: st.Start, End: st.End}
	return Evaluate(action, d, Options{Tolerance: b11bClockTolerance})
}

// b11bWant is what a step must have done, by the rule: one real change for the issue
// the command names, and one row.
type b11bWant struct {
	kind     string // the translator's name for the command
	issue    string
	created  bool
	sections []string
	added    []string // targets of the edges the command added
	removed  []string // targets of the edges the command removed
	quiet    []string // issues that must neither change nor gain a row
}

// B11b.OneRowPerRealChange. Every bd write that changes an issue is one Dolt commit
// that adds one version row for the issue it changed, in sequence, by the actor the
// harness supplied, and the ledger sees that from Dolt's diff alone. The same ledger
// run over the same commands shows no finding to the evaluator.
func TestB11bOneRowPerRealChange(t *testing.T) {
	s := newB11aStore(t, true)
	top := map[string]int64{}
	lastAt := map[string]time.Time{}

	check := func(t *testing.T, st b11bStep, w b11bWant) {
		t.Helper()
		if st.Before == st.After {
			t.Fatalf("bd committed nothing for a command that changes %s", w.issue)
		}
		d := b11bDeltaOf(t, s, st)
		got := b11bIssueIn(t, d, w.issue)
		if got.Existed == w.created || !got.Exists {
			t.Errorf("existed %v, exists %v, but created is %v", got.Existed, got.Exists, w.created)
		}
		if !got.Real {
			t.Errorf("Real is false for %s, and the command changed it", w.issue)
		}
		if !reflect.DeepEqual(got.Sections, w.sections) {
			t.Errorf("sections = %v, want %v", got.Sections, w.sections)
		}
		if !b11bSameStrings(got.AddedEdges, w.added) || !b11bSameStrings(got.RemovedEdges, w.removed) {
			t.Errorf("edges added %v removed %v, want added %v removed %v", got.AddedEdges, got.RemovedEdges, w.added, w.removed)
		}
		if len(got.Rows) != 1 {
			t.Fatalf("%d new rows for %s, want exactly 1: %+v", len(got.Rows), w.issue, got.Rows)
		}
		row := got.Rows[0]
		next := top[w.issue] + 1
		if row.Revision != next {
			t.Errorf("revision = %d, want %d", row.Revision, next)
		}
		if got.MaxRevisionBefore != top[w.issue] || got.MaxRevisionAfter != next || got.RevisionAfter != next {
			t.Errorf("highest revision %d then %d and counter %d, want %d then %d and %d", got.MaxRevisionBefore, got.MaxRevisionAfter, got.RevisionAfter, top[w.issue], next, next)
		}
		if row.Actor != b11bLedgerActor || row.Status != "claimed" {
			t.Errorf("row is by %q with status %q, want %q and claimed", row.Actor, row.Status, b11bLedgerActor)
		}
		if row.Epoch != d.Epoch || d.EpochBefore != d.Epoch {
			t.Errorf("row epoch %d, store epoch %d then %d, want all the same", row.Epoch, d.EpochBefore, d.Epoch)
		}
		if !got.PreviousChangeAt.Equal(lastAt[w.issue]) {
			t.Errorf("PreviousChangeAt = %v, want the time of the issue's last row, %v", got.PreviousChangeAt, lastAt[w.issue])
		}
		if row.At.Before(st.Start.Add(-b11bClockTolerance)) || row.At.After(st.End.Add(b11bClockTolerance)) {
			t.Errorf("the row was written at %v, outside the command's window %v to %v", row.At, st.Start, st.End)
		}
		for _, id := range w.quiet {
			b11bQuiet(t, d, id)
		}
		top[w.issue] = next
		lastAt[w.issue] = row.At

		res := b11bJudge(w.kind, w.issue, b11bLedgerActor, st, d)
		b11bExpectFindings(t, res)
		if res.Legacy != 0 || res.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want the one change counted as participating", res.Legacy, res.Participating)
		}
	}

	var a, b string
	t.Run("create", func(t *testing.T) {
		var st b11bStep
		a, st = b11bMake(s, b11bLedgerActor, "Alpha")
		check(t, st, b11bWant{kind: "create", issue: a, created: true, sections: []string{"issue"}})
	})
	t.Run("create-second", func(t *testing.T) {
		var st b11bStep
		b, st = b11bMake(s, b11bLedgerActor, "Beta")
		check(t, st, b11bWant{kind: "create", issue: b, created: true, sections: []string{"issue"}})
	})
	if a == "" || b == "" {
		t.Fatal("the creates did not run, so there is nothing to change")
	}

	steps := []struct {
		name string
		args []string
		want b11bWant
	}{
		{"update", []string{"update", "--description", "alpha text", "--", a}, b11bWant{kind: "update", issue: a, sections: []string{"issue"}}},
		{"update-add-label", []string{"update", "--add-label", "foo", "--", a}, b11bWant{kind: "update", issue: a, sections: []string{"labels"}}},
		{"update-remove-label", []string{"update", "--remove-label", "foo", "--", a}, b11bWant{kind: "update", issue: a, sections: []string{"labels"}}},
		{"label-add", []string{"label", "add", a, "bar"}, b11bWant{kind: "update", issue: a, sections: []string{"labels"}}},
		{"label-remove", []string{"label", "remove", a, "bar"}, b11bWant{kind: "update", issue: a, sections: []string{"labels"}}},
		{"claim", []string{"update", "--claim", "--", a}, b11bWant{kind: "update", issue: a, sections: []string{"issue"}}},
		{"close", []string{"close", "--reason", "done", "--", a}, b11bWant{kind: "close", issue: a, sections: []string{"issue"}}},
		{"reopen", []string{"reopen", a}, b11bWant{kind: "update", issue: a, sections: []string{"issue"}}},
		{
			// The edge belongs to the issue that depends on the other, and only that one is
			// given a row: the target is not changed by having an edge pointed at it.
			"dep-add",
			[]string{"dep", "add", "--", a, b},
			b11bWant{kind: "dep_add", issue: a, sections: []string{"dependencies"}, added: []string{b}, quiet: []string{b}},
		},
		{
			"dep-remove",
			[]string{"dep", "remove", "--", a, b},
			b11bWant{kind: "dep_remove", issue: a, sections: []string{"dependencies"}, removed: []string{b}, quiet: []string{b}},
		},
		{
			// One command that changes the issue's own row and its labels is one change, one
			// commit and one row, not one of each.
			"composite-update",
			[]string{"update", "--title", "Alpha again", "--priority", "1", "--add-label", "comp", "--", a},
			b11bWant{kind: "update", issue: a, sections: []string{"issue", "labels"}},
		},
	}
	for _, c := range steps {
		t.Run(c.name, func(t *testing.T) {
			check(t, b11bRun(s, b11bLedgerActor, c.args...), c.want)
		})
	}
}

// B11b.NoOpMintsNothing. A command that changes nothing leaves the clone where it was.
// The ledger does not take bd's word for it: the delta between the heads is empty of
// real changes and rows, and the same holds for a commit that is empty or only moves
// the stamps and the bookkeeping.
func TestB11bNoOpMintsNothing(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")
	b, _ := b11bMake(s, b11bLedgerActor, "Beta")
	for _, setup := range [][]string{
		{"update", "--description", "same text", "--", a},
		{"label", "add", a, "foo"},
		{"dep", "add", "--", a, b},
		{"update", "--claim", "--", a},
	} {
		b11bRun(s, b11bLedgerActor, setup...)
	}

	nothing := func(t *testing.T, st b11bStep, kind string) {
		t.Helper()
		d := b11bDeltaOf(t, s, st)
		for _, id := range []string{a, b} {
			b11bQuiet(t, d, id)
		}
		res := b11bJudge(kind, a, b11bLedgerActor, st, d)
		b11bExpectFindings(t, res)
		if res.Legacy != 0 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; a command that changed nothing is counted in neither", res.Legacy, res.Participating)
		}
	}

	for _, c := range []struct {
		name string
		kind string
		args []string
	}{
		{"same-value-update", "update", []string{"update", "--description", "same text", "--", a}},
		{"duplicate-label-add", "update", []string{"label", "add", a, "foo"}},
		{"duplicate-add-label", "update", []string{"update", "--add-label", "foo", "--", a}},
		{"absent-label-remove", "update", []string{"label", "remove", a, "absent"}},
		{"absent-remove-label", "update", []string{"update", "--remove-label", "absent", "--", a}},
		{"duplicate-dep-add", "dep_add", []string{"dep", "add", "--", a, b}},
		{"absent-dep-remove", "dep_remove", []string{"dep", "remove", "--", b, a}},
		{"repeated-claim", "update", []string{"update", "--claim", "--", a}},
		{"reopen-an-open-issue", "update", []string{"reopen", b}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := b11bRun(s, b11bLedgerActor, c.args...)
			if st.Before != st.After {
				t.Errorf("premise: bd committed for a command that changes nothing (%s to %s)", st.Before, st.After)
			}
			nothing(t, st, c.kind)
		})
	}

	t.Run("close-then-close-again", func(t *testing.T) {
		// The setup gave the issue an edge to the other, so it is blocked, and bd will
		// not close a blocked issue unless it is made to.
		first := b11bRun(s, b11bLedgerActor, "close", "--force", "--reason", "done", "--", a)
		changed := b11bIssueIn(t, b11bDeltaOf(t, s, first), a)
		if !changed.Real || len(changed.Rows) != 1 {
			t.Fatalf("the first close should be one real change with one row: real %v, rows %+v", changed.Real, changed.Rows)
		}
		again := b11bRun(s, b11bLedgerActor, "close", "--force", "--reason", "done", "--", a)
		if again.Before != again.After {
			t.Errorf("premise: bd committed for a second close")
		}
		nothing(t, again, "close")
	})

	t.Run("an-empty-commit", func(t *testing.T) {
		before := s.head()
		replaytest.RunDolt(t, s.data, "commit", "--allow-empty", "-m", "an empty commit")
		after := s.head()
		if before == after {
			t.Fatal("premise: the empty commit did not move the head")
		}
		nothing(t, b11bStep{Before: before, After: after, Start: time.Now().UTC(), End: time.Now().UTC()}, "update")
	})

	t.Run("the-same-head-twice", func(t *testing.T) {
		head := s.head()
		d := b11bDeltaBetween(t, s, head, head)
		if len(d.Issues) != 0 {
			t.Errorf("a delta from a head to itself names %v", b11bIDs(d))
		}
	})
}

// B11b.StampOnlyIsNotARealChange. The columns a store writes about a write rather than
// about the issue, the derived flag, and the recorder's own bookkeeping are not the
// issue changing: a diff confined to them is no change and no row is owed for it. A
// column the version document carries is the control: change one and the issue
// really changed.
func TestB11bStampOnlyIsNotARealChange(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")

	// The set of columns that are bookkeeping is the classification the drift table
	// already holds: a stamp, the derived flag, or a column the document never carries.
	bookkeeping := map[string]bool{}
	for _, f := range IssueFields() {
		if f.Class == ClassStamp || f.Class == ClassDerived {
			for _, c := range f.Columns {
				bookkeeping[c] = true
			}
		}
	}
	for c := range UncarriedColumns() {
		bookkeeping[c] = true
	}

	const later = "'2031-01-01 00:00:00'"
	stamps := []struct{ column, set string }{
		{"created_at", later},
		{"updated_at", later},
		{"started_at", later},
		{"closed_at", later},
		{"closed_by_session", "'stamp-test'"},
		{"compacted_at", later},
		{"compacted_at_commit", "'stamp-test'"},
		{"is_blocked", "1 - is_blocked"},
		{"current_revision", "current_revision + 7"},
		{"content_hash", "'stamp-test'"},
		{"participation_generation", "COALESCE(participation_generation, 0) + 5"},
	}
	edit := func(t *testing.T, column, set string) (before, after string) {
		t.Helper()
		before = s.head()
		b11aSQL(t, s.data, fmt.Sprintf("UPDATE issues SET %s = %s WHERE id = %s", column, set, doltcli.SQLQuote(a)))
		return before, b11aCommit(t, s.data, "edit "+column)
	}

	for _, c := range stamps {
		t.Run(c.column, func(t *testing.T) {
			if !bookkeeping[c.column] {
				t.Fatalf("%s is not a stamp, a derived column or an uncarried one in the drift table; it is a column the version document carries", c.column)
			}
			before, after := edit(t, c.column, c.set)
			d := b11bDeltaBetween(t, s, before, after)
			if issue, ok := d.Issue(a); ok && (issue.Real || len(issue.Sections) > 0 || len(issue.Rows) > 0) {
				t.Errorf("a change confined to %s is reported as real %v in sections %v with rows %+v", c.column, issue.Real, issue.Sections, issue.Rows)
			}
			res := Evaluate(Action{Kind: "update", Issue: a, Actor: b11bLedgerActor, Start: time.Now().UTC(), End: time.Now().UTC()}, d, Options{Tolerance: b11bClockTolerance})
			if res.Legacy != 0 || res.Participating != 0 {
				t.Errorf("legacy %d, participating %d; a stamp is no change", res.Legacy, res.Participating)
			}
		})
	}

	for _, c := range []struct{ column, set string }{
		{"notes", "'changed by sql'"},
		{"description", "'changed by sql'"},
		{"title", "'Changed by sql'"},
	} {
		t.Run("control-"+c.column, func(t *testing.T) {
			if bookkeeping[c.column] {
				t.Fatalf("%s is bookkeeping in the drift table, so it is not a control", c.column)
			}
			before, after := edit(t, c.column, c.set)
			issue := b11bIssueIn(t, b11bDeltaBetween(t, s, before, after), a)
			if !issue.Real || !reflect.DeepEqual(issue.Sections, []string{"issue"}) {
				t.Errorf("a change to %s is real %v in sections %v, want real in [issue]", c.column, issue.Real, issue.Sections)
			}
		})
	}
}

// B11b.DeleteSurvives, as the ledger sees it. A delete takes the issue's row and adds
// no version row; the rows the issue had are there afterwards and unchanged, and none
// carries a removal mark, which is recorded as a fact about the product and not judged.
func TestB11bDeleteSurvives(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")
	b11bRun(s, b11bLedgerActor, "update", "--description", "before the delete", "--", a)

	st := b11bRun(s, b11bLedgerActor, "delete", "--force", "--", a)
	if st.Before == st.After {
		t.Fatal("bd committed nothing for the delete")
	}
	d := b11bDeltaOf(t, s, st)
	got := b11bIssueIn(t, d, a)
	if !got.Existed || got.Exists || !got.Real {
		t.Errorf("existed %v, exists %v, real %v; want an issue that was there, is gone, and really changed", got.Existed, got.Exists, got.Real)
	}
	if len(got.Rows) != 0 {
		t.Errorf("the delete added rows for %s: %+v", a, got.Rows)
	}
	if got.Lost != 0 || got.Altered != 0 || got.Marked {
		t.Errorf("lost %d, altered %d, marked %v; want the prior rows intact and unmarked", got.Lost, got.Altered, got.Marked)
	}
	if got.MaxRevisionBefore != 2 || got.MaxRevisionAfter != 2 {
		t.Errorf("highest revision %d then %d, want 2 then 2", got.MaxRevisionBefore, got.MaxRevisionAfter)
	}

	res := b11bJudge("delete", a, b11bLedgerActor, st, d)
	b11bExpectFindings(t, res)
	if res.RemovedMarked {
		t.Error("RemovedMarked is true, and no row carries a removal mark")
	}
	if res.Legacy != 0 || res.Participating != 1 {
		t.Errorf("legacy %d, participating %d; want the delete counted once, as participating", res.Legacy, res.Participating)
	}

	// The rows themselves, read straight from the clone.
	_, rows, err := doltcli.Query(context.Background(), s.data,
		"SELECT revision, removed_at, removed_reason FROM issue_versions WHERE issue_id = "+doltcli.SQLQuote(a)+" ORDER BY revision")
	if err != nil {
		t.Fatalf("reading the surviving rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows survive the delete, want 2", len(rows))
	}
	for _, row := range rows {
		if !row[1].Null || !row[2].Null {
			t.Errorf("revision %s carries removed_at %+v and removed_reason %+v, want neither: bd never marks a removal", row[0].Text, row[1], row[2])
		}
	}
}

// B11b.DeleteSurvives, the facts the ledger reads when history is not only added to.
func TestB11bLedgerSurvivalFacts(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")
	b11bRun(s, b11bLedgerActor, "update", "--description", "second revision", "--", a)
	quoted := doltcli.SQLQuote(a)

	cases := []struct {
		name    string
		stmt    string
		lost    int
		altered int
		marked  bool
	}{
		{"a-row-is-removed", "DELETE FROM issue_versions WHERE issue_id = " + quoted + " AND revision = 1", 1, 0, false},
		{"a-row-is-rewritten", "UPDATE issue_versions SET change_actor = 'tampered' WHERE issue_id = " + quoted + " AND revision = 2", 0, 1, false},
		{"a-row-is-marked-removed", "UPDATE issue_versions SET removed_at = UTC_TIMESTAMP(6), removed_reason = 'gone' WHERE issue_id = " + quoted + " AND revision = 2", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := s.head()
			b11aSQL(t, s.data, c.stmt)
			after := b11aCommit(t, s.data, c.name)
			got := b11bIssueIn(t, b11bDeltaBetween(t, s, before, after), a)
			if got.Lost != c.lost || got.Altered != c.altered || got.Marked != c.marked {
				t.Errorf("lost %d, altered %d, marked %v; want %d, %d, %v", got.Lost, got.Altered, got.Marked, c.lost, c.altered, c.marked)
			}
		})
	}
}

// B11b.CascadeClassified, as the ledger sees it. Deleting an issue removes the edges
// other issues own to it. What the product mints for an edge owner depends on whether
// its text cites the deleted id, and the harness records whichever it does: the facts
// are read from the diff and the evaluator classifies them, never drops them.
func TestB11bCascadeClassified(t *testing.T) {
	t.Run("an-owner-that-does-not-cite-the-deleted-id", func(t *testing.T) {
		s := newB11aStore(t, true)
		alpha, _ := b11bMake(s, b11bLedgerActor, "Alpha")
		beta, _ := b11bMake(s, b11bLedgerActor, "Beta", "--description", "a note that cites nothing")
		b11bRun(s, b11bLedgerActor, "dep", "add", "--", beta, alpha)

		st := b11bRun(s, b11bLedgerActor, "delete", "--force", "--", alpha)
		d := b11bDeltaOf(t, s, st)
		owner := b11bIssueIn(t, d, beta)
		if !owner.Real || !reflect.DeepEqual(owner.Sections, []string{"dependencies"}) {
			t.Fatalf("the owner is real %v in sections %v, want a real change to its edges alone", owner.Real, owner.Sections)
		}
		if !b11bSameStrings(owner.RemovedEdges, []string{alpha}) || len(owner.AddedEdges) != 0 {
			t.Errorf("edges added %v removed %v, want only the edge to the deleted issue removed", owner.AddedEdges, owner.RemovedEdges)
		}
		if len(owner.Rows) > 1 {
			t.Fatalf("%d rows for the owner, want at most 1: %+v", len(owner.Rows), owner.Rows)
		}

		// Either outcome is accepted and neither is dropped: with no row the change is a
		// gap and is counted; with one it is a plain change with its row.
		res := b11bJudge("delete", alpha, b11bLedgerActor, st, d)
		if len(owner.Rows) == 0 {
			b11bExpectFindings(t, res, "version-gap/cascade-edge-removal@"+beta)
		} else {
			b11bExpectFindings(t, res)
		}
		if res.Participating != 2 || res.Legacy != 0 {
			t.Errorf("legacy %d, participating %d; want the deleted issue and its edge owner both counted", res.Legacy, res.Participating)
		}
	})

	t.Run("an-owner-that-cites-the-deleted-id", func(t *testing.T) {
		s := newB11aStore(t, true)
		alpha, _ := b11bMake(s, b11bLedgerActor, "Alpha")
		gamma, _ := b11bMake(s, b11bLedgerActor, "Gamma", "--description", "see "+alpha+" for details")
		b11bRun(s, b11bLedgerActor, "dep", "add", "--", gamma, alpha)

		st := b11bRun(s, b11bLedgerActor, "delete", "--force", "--", alpha)
		d := b11bDeltaOf(t, s, st)
		owner := b11bIssueIn(t, d, gamma)
		if !reflect.DeepEqual(owner.Sections, []string{"issue", "dependencies"}) {
			t.Errorf("sections = %v, want the text rewritten and the edge removed", owner.Sections)
		}
		if len(owner.Rows) != 1 {
			t.Fatalf("%d rows for the citing owner, want exactly 1: %+v", len(owner.Rows), owner.Rows)
		}
		b11bExpectFindings(t, b11bJudge("delete", alpha, b11bLedgerActor, st, d))
	})
}

// B11b.Attribution, over the rows real bd writes. A row names the actor its command
// was pinned to, and the claim path pins its actor like any other; an evaluator told a
// different actor reads the same rows as wrong.
func TestB11bAttribution(t *testing.T) {
	s := newB11aStore(t, true)
	x, creation := b11bMake(s, "actor-one", "Alpha")
	update := b11bRun(s, "actor-two", "update", "--description", "second hand", "--", x)
	y, _ := b11bMake(s, "actor-one", "Beta")
	claim := b11bRun(s, "actor-three", "update", "--claim", "--", y)

	for _, c := range []struct {
		name  string
		step  b11bStep
		issue string
		actor string
		kind  string
	}{
		{"create", creation, x, "actor-one", "create"},
		{"update", update, x, "actor-two", "update"},
		{"claim", claim, y, "actor-three", "update"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := b11bDeltaOf(t, s, c.step)
			got := b11bIssueIn(t, d, c.issue)
			if len(got.Rows) != 1 {
				t.Fatalf("%d rows, want 1: %+v", len(got.Rows), got.Rows)
			}
			if got.Rows[0].Actor != c.actor || got.Rows[0].Status != "claimed" {
				t.Errorf("row is by %q with status %q, want %q and claimed", got.Rows[0].Actor, got.Rows[0].Status, c.actor)
			}
			b11bExpectFindings(t, b11bJudge(c.kind, c.issue, c.actor, c.step, d))

			// The same rows against the wrong actor are a finding, and only that one.
			wrong := b11bJudge(c.kind, c.issue, "someone-else", c.step, d)
			b11bExpectFindings(t, wrong, "version-attribution/change-actor@"+c.issue)
		})
	}
}

// B11b.EpochStable, as the ledger reads it. The delta carries the store's epoch at both
// heads, and a bump between them is what the evaluator is told about.
func TestB11bLedgerEpoch(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")

	t.Run("unchanged", func(t *testing.T) {
		st := b11bRun(s, b11bLedgerActor, "update", "--description", "one", "--", a)
		d := b11bDeltaOf(t, s, st)
		if d.EpochBefore != 1 || d.Epoch != 1 {
			t.Errorf("epochs %d then %d, want the store's 1 then 1", d.EpochBefore, d.Epoch)
		}
	})

	t.Run("bumped", func(t *testing.T) {
		before := s.head()
		b11aSQL(t, s.data, "UPDATE store_epoch SET epoch = 2 WHERE id = 1")
		after := b11aCommit(t, s.data, "bump the epoch")
		d := b11bDeltaBetween(t, s, before, after)
		if d.EpochBefore != 1 || d.Epoch != 2 {
			t.Errorf("epochs %d then %d, want 1 then 2", d.EpochBefore, d.Epoch)
		}
		action := Action{Kind: "update", Issue: a, Actor: b11bLedgerActor, Start: time.Now().UTC(), End: time.Now().UTC()}
		b11bExpectFindings(t, Evaluate(action, d, Options{Tolerance: b11bClockTolerance}), "version-epoch/undeclared-bump@"+a)
		b11bExpectFindings(t, Evaluate(action, d, Options{Tolerance: b11bClockTolerance, EpochBump: true}))
	})

	// The store has no epoch row until the first version is written: the product
	// inserts it, at 1, in the command that mints. Absent is the starting epoch and
	// not a bump, so a run whose first command is that mint, or one that has not
	// minted yet, is stable at 1.
	t.Run("first-mint-is-not-a-bump", func(t *testing.T) {
		fresh := newB11aStore(t, true)
		if n := b11bEpochRows(t, fresh); n != "0" {
			t.Fatalf("a store with history on and nothing written has %s epoch rows, want 0", n)
		}
		x, st := b11bMake(fresh, b11bLedgerActor, "Alpha")
		if n := b11bEpochRows(t, fresh); n != "1" {
			t.Fatalf("the first version left %s epoch rows, want the 1 the product inserts", n)
		}
		d := b11bDeltaOf(t, fresh, st)
		if d.EpochBefore != 1 || d.Epoch != 1 {
			t.Errorf("epochs %d then %d, want 1 then 1: no row is the starting epoch, and the row inserted is it", d.EpochBefore, d.Epoch)
		}
		b11bExpectFindings(t, b11bJudge("create", x, b11bLedgerActor, st, d))
	})

	t.Run("no-row-at-either-head", func(t *testing.T) {
		fresh := newB11aStore(t, true)
		before := fresh.head()
		b11aSQL(t, fresh.data, "CREATE TABLE epoch_probe (id INT PRIMARY KEY)")
		after := b11aCommit(t, fresh.data, "a commit that mints nothing")
		d := b11bDeltaBetween(t, fresh, before, after)
		if d.EpochBefore != 1 || d.Epoch != 1 {
			t.Errorf("epochs %d then %d, want 1 then 1: a store that has minted nothing has not changed epoch", d.EpochBefore, d.Epoch)
		}
		if len(d.Issues) != 0 {
			t.Errorf("the delta names %v, want no issue", b11bIDs(d))
		}
	})
}

// b11bEpochRows is how many rows the store's epoch table holds, as the text the
// query returned.
func b11bEpochRows(t *testing.T, s *b11aStore) string {
	t.Helper()
	_, rows, err := doltcli.Query(context.Background(), s.data, "SELECT COUNT(*) FROM store_epoch")
	if err != nil || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("counting the epoch rows: %v, %v", rows, err)
	}
	return rows[0][0].Text
}

// B11b.TimeOrdering, as the ledger reads it. change_at is a DATETIME written in UTC,
// and the ledger reads it as UTC whatever zone the process is in.
func TestB11bLedgerTimesAreUTC(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("seven-hours-west", -7*60*60)
	t.Cleanup(func() { time.Local = saved })

	s := newB11aStore(t, true)
	a, st := b11bMake(s, b11bLedgerActor, "Alpha")
	got := b11bIssueIn(t, b11bDeltaOf(t, s, st), a)
	if len(got.Rows) != 1 {
		t.Fatalf("%d rows, want 1", len(got.Rows))
	}
	at := got.Rows[0].At
	if at.Before(st.Start.Add(-b11bClockTolerance)) || at.After(st.End.Add(b11bClockTolerance)) {
		t.Errorf("the row was written at %v, and the command ran from %v to %v", at, st.Start, st.End)
	}
}

// B11b.FenceAccounting, over a real store whose history was turned on after it had
// issues: the seeded issues are legacy records, an issue made afterwards is not. Each
// change is classified once, by the state of its issue at the head before the action.
func TestB11bFenceAccountingFromTheLedger(t *testing.T) {
	s := newB11aStore(t, false)
	legacy := map[string]string{}
	for _, name := range []string{"one", "two", "three", "four"} {
		legacy[name] = s.create("Seeded " + name)
	}
	s.enableHistory()
	made, _ := b11bMake(s, b11bLedgerActor, "Made after history was on")

	judge := func(d Delta, issue string, from, to time.Time) Result {
		return Evaluate(Action{Kind: "update", Issue: issue, Actor: b11bLedgerActor, Start: from, End: to}, d, Options{Tolerance: b11bClockTolerance})
	}

	t.Run("fenced-mixed", func(t *testing.T) {
		before := s.head()
		start := time.Now().UTC()
		s.bd(b11bPinned(b11bLedgerActor, []string{"update", "--title", "Seeded one, changed", "--", legacy["one"]})...)
		s.bd(b11bPinned(b11bLedgerActor, []string{"update", "--title", "Made after, changed", "--", made})...)
		d := b11bDeltaBetween(t, s, before, s.head())
		if !d.Fence {
			t.Fatal("the store has participation_generation, and the delta says there is no fence")
		}
		if !sort.SliceIsSorted(d.Issues, func(i, j int) bool { return d.Issues[i].ID < d.Issues[j].ID }) {
			t.Errorf("the issues are not in id order: %v", b11bIDs(d))
		}
		one := b11bIssueIn(t, d, legacy["one"])
		if one.GenerationBefore != nil || !one.Real || len(one.Rows) != 0 || one.RevisionBefore != one.RevisionAfter {
			t.Errorf("the seeded issue: generation %v, real %v, rows %+v, counter %d then %d; want NULL, real, no rows, counter unmoved", one.GenerationBefore, one.Real, one.Rows, one.RevisionBefore, one.RevisionAfter)
		}
		later := b11bIssueIn(t, d, made)
		if later.GenerationBefore == nil || len(later.Rows) != 1 {
			t.Errorf("the issue made after: generation %v, rows %+v; want a generation and one row", later.GenerationBefore, later.Rows)
		}
		res := judge(d, legacy["one"], start, time.Now().UTC())
		b11bExpectFindings(t, res)
		if res.Legacy != 1 || res.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want 1 and 1", res.Legacy, res.Participating)
		}
	})

	t.Run("a-label-on-a-legacy-issue-is-seen", func(t *testing.T) {
		// The change is in the labels table alone: the issue's own row is not touched,
		// and the delta must still name the issue.
		st := b11bRun(s, b11bLedgerActor, "label", "add", legacy["one"], "tagged")
		d := b11bDeltaOf(t, s, st)
		one := b11bIssueIn(t, d, legacy["one"])
		if !one.Real || !reflect.DeepEqual(one.Sections, []string{"labels"}) || len(one.Rows) != 0 {
			t.Errorf("real %v, sections %v, rows %+v; want a real change to the labels and no rows", one.Real, one.Sections, one.Rows)
		}
		res := judge(d, legacy["one"], st.Start, st.End)
		b11bExpectFindings(t, res)
		if res.Legacy != 1 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; want 1 and 0", res.Legacy, res.Participating)
		}
	})

	t.Run("classified-at-head-before", func(t *testing.T) {
		// One commit stamps the seeded issue and changes it; the change is judged by
		// the state it began in, so it is a legacy change and the stamp is the breach.
		before := s.head()
		start := time.Now().UTC()
		b11aSQL(t, s.data, "UPDATE issues SET participation_generation = 1, notes = 'stamped and changed' WHERE id = "+doltcli.SQLQuote(legacy["two"]))
		stamped := b11aCommit(t, s.data, "stamp and change")
		d := b11bDeltaBetween(t, s, before, stamped)
		two := b11bIssueIn(t, d, legacy["two"])
		if two.GenerationBefore != nil || two.GenerationAfter == nil || *two.GenerationAfter != 1 {
			t.Errorf("generation %v then %v, want NULL then 1", two.GenerationBefore, two.GenerationAfter)
		}
		res := judge(d, legacy["two"], start, time.Now().UTC())
		b11bExpectFindings(t, res, "version-fence/promoted@"+legacy["two"])
		if res.Legacy != 1 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; want 1 and 0", res.Legacy, res.Participating)
		}

		// The next change to the same issue begins from the stamped state.
		start = time.Now().UTC()
		b11aSQL(t, s.data, "UPDATE issues SET notes = 'changed again' WHERE id = "+doltcli.SQLQuote(legacy["two"]))
		next := b11aCommit(t, s.data, "change again")
		d = b11bDeltaBetween(t, s, stamped, next)
		if again := b11bIssueIn(t, d, legacy["two"]); again.GenerationBefore == nil || *again.GenerationBefore != 1 {
			t.Errorf("generation before = %v, want 1", again.GenerationBefore)
		}
		if res := judge(d, legacy["two"], start, time.Now().UTC()); res.Legacy != 0 || res.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want 0 and 1", res.Legacy, res.Participating)
		}
	})

	t.Run("created-by-the-action", func(t *testing.T) {
		id, st := b11bMake(s, b11bLedgerActor, "Created in the action")
		d := b11bDeltaOf(t, s, st)
		if got := b11bIssueIn(t, d, id); got.Existed || got.GenerationBefore != nil {
			t.Errorf("existed %v, generation before %v; want an issue that was not there", got.Existed, got.GenerationBefore)
		}
		res := b11bJudge("create", id, b11bLedgerActor, st, d)
		b11bExpectFindings(t, res)
		if res.Legacy != 0 || res.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want 0 and 1", res.Legacy, res.Participating)
		}
	})

	t.Run("delete-of-legacy", func(t *testing.T) {
		st := b11bRun(s, b11bLedgerActor, "delete", "--force", "--", legacy["three"])
		d := b11bDeltaOf(t, s, st)
		got := b11bIssueIn(t, d, legacy["three"])
		if !got.Existed || got.Exists || got.GenerationBefore != nil || len(got.Rows) != 0 {
			t.Errorf("existed %v, exists %v, generation %v, rows %+v; want a legacy record that is gone and was given nothing", got.Existed, got.Exists, got.GenerationBefore, got.Rows)
		}
		res := b11bJudge("delete", legacy["three"], b11bLedgerActor, st, d)
		b11bExpectFindings(t, res)
		if res.Legacy != 1 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; want 1 and 0", res.Legacy, res.Participating)
		}
	})

	t.Run("no-op-not-counted", func(t *testing.T) {
		st := b11bRun(s, b11bLedgerActor, "update", "--title", "Seeded four", "--", legacy["four"])
		res := b11bJudge("update", legacy["four"], b11bLedgerActor, st, b11bDeltaOf(t, s, st))
		if res.Legacy != 0 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; a same-value update is counted in neither", res.Legacy, res.Participating)
		}

		// A row written for an issue that did not change is no change either: it is
		// counted in neither field, and the fence reports it.
		before := s.head()
		start := time.Now().UTC()
		b11aInsertVersion(t, s.data, legacy["four"], 1, []byte(`{"id":"`+legacy["four"]+`"}`))
		after := b11aCommit(t, s.data, "a row for an issue that did not change")
		d := b11bDeltaBetween(t, s, before, after)
		four := b11bIssueIn(t, d, legacy["four"])
		if four.Real || len(four.Rows) != 1 {
			t.Fatalf("real %v, rows %+v; want no real change and the one row", four.Real, four.Rows)
		}
		res = judge(d, legacy["four"], start, time.Now().UTC())
		b11bExpectFindings(t, res, "version-fence/row-minted@"+legacy["four"])
		if res.Legacy != 0 || res.Participating != 0 {
			t.Errorf("legacy %d, participating %d; want neither", res.Legacy, res.Participating)
		}
	})
}

// B11b.FenceAccounting without a fence. A base that has no participation_generation
// has no legacy records: the delta says there is no fence, and every real change is a
// participating one.
func TestB11bFenceAccountingWithoutTheFence(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")
	b11aSQL(t, s.data, "ALTER TABLE issues DROP COLUMN participation_generation")
	before := b11aCommit(t, s.data, "a base without the fence")

	start := time.Now().UTC()
	b11aSQL(t, s.data, "UPDATE issues SET notes = 'changed' WHERE id = "+doltcli.SQLQuote(a))
	after := b11aCommit(t, s.data, "change")
	d := b11bDeltaBetween(t, s, before, after)
	if d.Fence {
		t.Fatal("the delta says there is a fence, and the column is not there")
	}
	got := b11bIssueIn(t, d, a)
	if got.GenerationBefore != nil || got.GenerationAfter != nil || !got.Real {
		t.Errorf("generation %v then %v, real %v; want no generation and a real change", got.GenerationBefore, got.GenerationAfter, got.Real)
	}
	res := Evaluate(Action{Kind: "update", Issue: a, Actor: b11bLedgerActor, Start: start, End: time.Now().UTC()}, d, Options{Tolerance: b11bClockTolerance})
	if res.Legacy != 0 || res.Participating != 1 {
		t.Errorf("legacy %d, participating %d; want every change participating", res.Legacy, res.Participating)
	}
}

// B11b.FenceAccounting, an unreadable classification. The fence marker is read from the
// clone, and a read that fails or returns a cell that is neither NULL nor an integer
// is an error of the harness: the ledger returns it and a zero delta, and never
// defaults the issue to legacy or to participating.
func TestB11bFenceAccountingUnreadable(t *testing.T) {
	s := newB11aStore(t, true)
	a, _ := b11bMake(s, b11bLedgerActor, "Alpha")
	quoted := doltcli.SQLQuote(a)

	b11aSQL(t, s.data, "ALTER TABLE issues MODIFY COLUMN participation_generation VARCHAR(20)")
	typed := b11aCommit(t, s.data, "the marker as text")

	t.Run("at-the-head-before", func(t *testing.T) {
		b11aSQL(t, s.data, "UPDATE issues SET participation_generation = 'oops' WHERE id = "+quoted)
		corrupt := b11aCommit(t, s.data, "a marker that is not an integer")
		b11aSQL(t, s.data, "UPDATE issues SET notes = 'changed' WHERE id = "+quoted)
		after := b11aCommit(t, s.data, "change")
		d, err := Ledger{}.Delta(context.Background(), s.data, corrupt, after)
		if err == nil {
			t.Fatalf("Delta read %+v from a classification that is not an integer", d)
		}
		if !reflect.DeepEqual(d, Delta{}) {
			t.Errorf("the delta is not zero beside its error: %+v", d)
		}
	})

	t.Run("at-the-head-after", func(t *testing.T) {
		// The marker was fine before and is not afterwards.
		b11aSQL(t, s.data, "UPDATE issues SET participation_generation = '1' WHERE id = "+quoted)
		fine := b11aCommit(t, s.data, "a marker that is an integer")
		b11aSQL(t, s.data, "UPDATE issues SET participation_generation = 'oops' WHERE id = "+quoted)
		corrupt := b11aCommit(t, s.data, "a marker that is not")
		d, err := Ledger{}.Delta(context.Background(), s.data, fine, corrupt)
		if err == nil {
			t.Fatalf("Delta read %+v from a classification that is not an integer", d)
		}
		if !reflect.DeepEqual(d, Delta{}) {
			t.Errorf("the delta is not zero beside its error: %+v", d)
		}
	})

	t.Run("a-ref-that-is-not-there", func(t *testing.T) {
		for _, c := range []struct{ name, from, to string }{
			{"from", "no-such-ref", typed},
			{"to", typed, "no-such-ref"},
		} {
			d, err := Ledger{}.Delta(context.Background(), s.data, c.from, c.to)
			if err == nil {
				t.Errorf("%s: Delta read %+v from a ref that is not there", c.name, d)
			}
		}
	})
}
