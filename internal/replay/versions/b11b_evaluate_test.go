// B11b: the checks that hold one action's version accounting to the design.
//
// The ledger (see b11b_ledger_test.go) reads what an action did to the work clone
// from Dolt's own diff: which issues really changed, which version rows were
// added, what the fence marker and the revision counter did. Evaluate holds that
// delta to the rules, and is pure, so these tests run it over synthetic deltas and
// pin every rule, every sub-case and every boundary without a database. Each
// finding is one category with one sub-case, for one issue; a negative control
// that breaks one rule must produce that finding and no other.
//
//   - V0 version-fence: the write fence of a store that has one. A legacy record
//     (participation_generation NULL before the action) gets no row, no stamp and
//     no new revision, whatever the action did, a no-op included.
//   - V1 version-count: a participating issue shows exactly one new row if it
//     really changed and exists, and none otherwise.
//   - V2 version-chain: new revisions run on from the highest there was, and the
//     revision counter is the highest revision, or 1 when there is none.
//   - V5 version-attribution, V6 version-epoch, V7 version-time, V8
//     version-survival: who, which epoch, when, and that history is only added to.
//
// A version-gap is a real change that mints nothing for a cause the product's own
// completeness test accepts; it is counted, never a mismatch.
package versions

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"
)

// b11bStart is the start of every synthetic action; the action runs for b11bWindow.
var b11bStart = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

const (
	b11bWindow    = 2 * time.Second
	b11bTolerance = 500 * time.Millisecond
	b11bReplayer  = "replay"
)

func b11bGen(n int64) *int64 { return &n }

// b11bAction is an update of x-1 run as the harness's actor.
func b11bAction(mods ...func(*Action)) Action {
	a := Action{Kind: "update", Issue: "x-1", Actor: b11bReplayer, Start: b11bStart, End: b11bStart.Add(b11bWindow)}
	for _, mod := range mods {
		mod(&a)
	}
	return a
}

// b11bRow is a version row as the product's own mint writes it for the default
// action: the harness's actor, claimed, the store's epoch, written inside the
// action's window.
func b11bRow(revision int64, mods ...func(*Version)) Version {
	v := Version{Revision: revision, Epoch: 1, Actor: b11bReplayer, Status: "claimed", At: b11bStart.Add(time.Second)}
	for _, mod := range mods {
		mod(&v)
	}
	return v
}

// b11bParticipating is an issue that has been versioned and really changes during
// the action: it was at revision 1, and now has revision 2 as its one new row.
func b11bParticipating(mods ...func(*IssueDelta)) IssueDelta {
	i := IssueDelta{
		ID: "x-1", Existed: true, Exists: true,
		GenerationBefore: b11bGen(1), GenerationAfter: b11bGen(1),
		RevisionBefore: 1, RevisionAfter: 2,
		Real: true, Sections: []string{"issue"},
		MaxRevisionBefore: 1, MaxRevisionAfter: 2,
		PreviousChangeAt: b11bStart.Add(-time.Hour),
		Rows:             []Version{b11bRow(2)},
	}
	for _, mod := range mods {
		mod(&i)
	}
	return i
}

// b11bLegacy is a seeded issue that really changes during the action: its row has
// no participation_generation, it has no version and its counter is at the default,
// and the fence leaves all three as they are.
func b11bLegacy(mods ...func(*IssueDelta)) IssueDelta {
	i := IssueDelta{
		ID: "x-1", Existed: true, Exists: true,
		RevisionBefore: 1, RevisionAfter: 1,
		Real: true, Sections: []string{"issue"},
	}
	for _, mod := range mods {
		mod(&i)
	}
	return i
}

// b11bDelta is the delta of a store that has the fence column and one epoch.
func b11bDelta(issues ...IssueDelta) Delta {
	return Delta{From: "before", To: "after", EpochBefore: 1, Epoch: 1, Fence: true, Issues: issues}
}

func b11bOptions() Options { return Options{Tolerance: b11bTolerance} }

// b11bKeys is what a set of findings says, as "category/sub-case@issue", sorted.
func b11bKeys(findings []Finding) []string {
	keys := []string{}
	for _, f := range findings {
		keys = append(keys, f.Category+"/"+f.Sub+"@"+f.Issue)
	}
	sort.Strings(keys)
	return keys
}

// b11bExpectFindings fails the test unless the findings are exactly want, and every
// finding carries the facts it rests on as JSON.
func b11bExpectFindings(t *testing.T, got Result, want ...string) {
	t.Helper()
	sort.Strings(want)
	if want == nil {
		want = []string{}
	}
	if keys := b11bKeys(got.Findings); !reflect.DeepEqual(keys, want) {
		t.Errorf("findings = %v, want %v", keys, want)
	}
	for _, f := range got.Findings {
		if !json.Valid([]byte(f.Expected)) || !json.Valid([]byte(f.Actual)) {
			t.Errorf("finding %s/%s@%s must carry its facts as JSON; expected=%q actual=%q", f.Category, f.Sub, f.Issue, f.Expected, f.Actual)
		}
	}
}

func b11bEval(a Action, d Delta) Result { return Evaluate(a, d, b11bOptions()) }

// B11b.FenceBreachDetected, the rule. V0 holds every legacy record to three
// clauses, in this order: row-minted, promoted, revision-moved. A failed clause is
// a finding, the sub-case lists the failed clauses joined by "+", and a record whose
// issues row is gone is held to the first only. A clean fence is a count, not a pass:
// nothing is reported for it.
func TestB11bEvaluateFence(t *testing.T) {
	minted := func(i *IssueDelta) { i.Rows = []Version{b11bRow(1)}; i.MaxRevisionAfter = 1 }
	promoted := func(i *IssueDelta) { i.GenerationAfter = b11bGen(1) }
	moved := func(i *IssueDelta) { i.RevisionAfter = 2 }
	both := func(mods ...func(*IssueDelta)) func(*IssueDelta) {
		return func(i *IssueDelta) {
			for _, mod := range mods {
				mod(i)
			}
		}
	}

	cases := []struct {
		name  string
		issue IssueDelta
		want  []string
	}{
		{"the-fence-held", b11bLegacy(), nil},
		{"row-minted", b11bLegacy(minted), []string{"version-fence/row-minted@x-1"}},
		{"promoted", b11bLegacy(promoted), []string{"version-fence/promoted@x-1"}},
		{"revision-moved", b11bLegacy(moved), []string{"version-fence/revision-moved@x-1"}},
		{"row-minted-and-promoted", b11bLegacy(both(minted, promoted)), []string{"version-fence/row-minted+promoted@x-1"}},
		{"row-minted-and-revision-moved", b11bLegacy(both(minted, moved)), []string{"version-fence/row-minted+revision-moved@x-1"}},
		{"promoted-and-revision-moved", b11bLegacy(both(promoted, moved)), []string{"version-fence/promoted+revision-moved@x-1"}},
		{"all-three-in-order", b11bLegacy(both(moved, promoted, minted)), []string{"version-fence/row-minted+promoted+revision-moved@x-1"}},
		{
			// A record deleted by the action has no issues row left to hold to the
			// second and third clauses, and the NULL generation and zero counter it
			// then reads as must not be taken for a stamp or a move.
			"deleted-record-is-held-to-the-first-clause-only",
			b11bLegacy(func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0; i.Rows = []Version{b11bRow(1)} }),
			[]string{"version-fence/row-minted@x-1"},
		},
		{
			"deleted-record-without-a-row-is-clean",
			b11bLegacy(func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0 }),
			nil,
		},
		{
			// A row minted by an action that changed nothing is still a row minted
			// for a record the fence protects.
			"row-minted-on-no-op",
			b11bLegacy(minted, func(i *IssueDelta) { i.Real = false; i.Sections = nil }),
			[]string{"version-fence/row-minted@x-1"},
		},
		{
			// The version rules do not apply to a legacy record, so a record that is
			// breached gets the one finding and not a pile: the row it was given has
			// the wrong actor, the wrong epoch and a time out of the window, and none
			// of that is reported.
			"a-breach-is-one-finding",
			b11bLegacy(minted, func(i *IssueDelta) {
				i.Rows = []Version{b11bRow(1, func(v *Version) { v.Actor = "someone-else"; v.Epoch = 9; v.At = b11bStart.Add(-time.Hour) })}
			}),
			[]string{"version-fence/row-minted@x-1"},
		},
		{
			// The fence is about records that were there when history was turned on:
			// one the action creates is a participating record from its first write.
			"created-by-the-action-is-not-legacy",
			b11bParticipating(func(i *IssueDelta) {
				i.Existed = false
				i.GenerationBefore = nil
				i.RevisionBefore = 0
				i.RevisionAfter = 1
				i.MaxRevisionBefore = 0
				i.MaxRevisionAfter = 1
				i.PreviousChangeAt = time.Time{}
				i.Rows = []Version{b11bRow(1)}
			}),
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b11bExpectFindings(t, b11bEval(b11bAction(), b11bDelta(c.issue)), c.want...)
		})
	}

	t.Run("one-finding-per-issue", func(t *testing.T) {
		second := b11bLegacy(minted, func(i *IssueDelta) { i.ID = "x-2" })
		got := b11bEval(b11bAction(), b11bDelta(b11bLegacy(promoted), second))
		b11bExpectFindings(t, got, "version-fence/promoted@x-1", "version-fence/row-minted@x-2")
	})

	t.Run("a-store-without-the-fence-has-no-legacy-records", func(t *testing.T) {
		// The same shape, on a store with no participation_generation column: the
		// issue is an ordinary one, V0 is not evaluated, and what V1 and V2 say about
		// its first version is all there is to say.
		d := b11bDelta(b11bParticipating(func(i *IssueDelta) {
			i.GenerationBefore, i.GenerationAfter = nil, nil
			i.RevisionBefore, i.RevisionAfter = 1, 1
			i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 1
			i.PreviousChangeAt = time.Time{}
			i.Rows = []Version{b11bRow(1)}
		}))
		d.Fence = false
		b11bExpectFindings(t, b11bEval(b11bAction(), d))
	})
}

// B11b.OverMintDetected, B11b.UnderMintDetected, B11b.PhantomDetected: the rule.
// Expected rows for an action and an issue are 1 when the issue really changed, is
// there afterwards and is not a wisp, and 0 otherwise; any other number of new rows
// is a version-count finding, named by which way it is wrong.
func TestB11bEvaluateCount(t *testing.T) {
	noRows := func(i *IssueDelta) {
		i.Rows = nil
		i.RevisionAfter = 1
		i.MaxRevisionAfter = 1
	}
	notReal := func(i *IssueDelta) { i.Real = false; i.Sections = nil }

	cases := []struct {
		name  string
		issue IssueDelta
		want  []string
	}{
		{"one-row-for-a-real-change", b11bParticipating(), nil},
		{
			"over-mint",
			b11bParticipating(func(i *IssueDelta) {
				i.Rows = []Version{b11bRow(2), b11bRow(3)}
				i.RevisionAfter, i.MaxRevisionAfter = 3, 3
			}),
			[]string{"version-count/over-mint@x-1"},
		},
		{"under-mint", b11bParticipating(noRows), []string{"version-count/under-mint@x-1"}},
		{
			"phantom",
			b11bParticipating(notReal),
			[]string{"version-count/phantom@x-1"},
		},
		{
			"phantom-is-not-an-over-mint-when-two-rows-are-minted",
			b11bParticipating(notReal, func(i *IssueDelta) {
				i.Rows = []Version{b11bRow(2), b11bRow(3)}
				i.RevisionAfter, i.MaxRevisionAfter = 3, 3
			}),
			[]string{"version-count/phantom@x-1"},
		},
		{"no-op-mints-nothing", b11bParticipating(notReal, noRows), nil},
		{
			// The issue is gone afterwards: nothing is expected for it, and a row for it
			// is a phantom.
			"deleted-issue-expects-nothing",
			b11bParticipating(noRows, func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0 }),
			nil,
		},
		{
			"deleted-issue-with-a-row-is-a-phantom",
			b11bParticipating(func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0 }),
			[]string{"version-count/phantom@x-1"},
		},
		{"wisp-expects-nothing", b11bParticipating(noRows, func(i *IssueDelta) { i.Wisp = true }), nil},
		{"wisp-with-a-row-is-a-phantom", b11bParticipating(func(i *IssueDelta) { i.Wisp = true }), []string{"version-count/phantom@x-1"}},
		{
			// A created issue: the first row is revision 1 and the counter stays at 1.
			"created-issue-has-its-first-row",
			b11bParticipating(func(i *IssueDelta) {
				i.Existed = false
				i.GenerationBefore = nil
				i.RevisionBefore, i.RevisionAfter = 0, 1
				i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 1
				i.PreviousChangeAt = time.Time{}
				i.Rows = []Version{b11bRow(1)}
			}),
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b11bExpectFindings(t, b11bEval(b11bAction(), b11bDelta(c.issue)), c.want...)
		})
	}
}

// B11b.ChainInvariants. New revisions follow the highest there was, one after
// another, and the revision counter is the highest revision there is, or 1 for an
// issue that has none: the baseline of an issue that was never versioned, which is
// not a finding.
func TestB11bChainInvariants(t *testing.T) {
	unversioned := func(i *IssueDelta) {
		i.Rows = nil
		i.Real = false
		i.Sections = nil
		i.RevisionBefore, i.RevisionAfter = 1, 1
		i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 0
		i.PreviousChangeAt = time.Time{}
	}

	cases := []struct {
		name  string
		issue IssueDelta
		want  []string
	}{
		{"consecutive-revisions-and-a-matching-counter", b11bParticipating(), nil},
		{"baseline-never-versioned-counter-at-one", b11bParticipating(unversioned), nil},
		{
			"baseline-counter-moved-without-a-row",
			b11bParticipating(unversioned, func(i *IssueDelta) { i.RevisionAfter = 2 }),
			[]string{"version-chain/current-revision@x-1"},
		},
		{
			"counter-moved-without-a-row",
			b11bParticipating(func(i *IssueDelta) {
				i.Rows = nil
				i.Real = false
				i.Sections = nil
				i.RevisionAfter = 2
				i.MaxRevisionAfter = 1
			}),
			[]string{"version-chain/current-revision@x-1"},
		},
		{
			"counter-lags-the-row",
			b11bParticipating(func(i *IssueDelta) { i.RevisionAfter = 1 }),
			[]string{"version-chain/current-revision@x-1"},
		},
		{
			"revision-gap",
			b11bParticipating(func(i *IssueDelta) {
				i.Rows = []Version{b11bRow(5)}
				i.RevisionAfter, i.MaxRevisionAfter = 5, 5
			}),
			[]string{"version-chain/revision-gap@x-1"},
		},
		{
			"first-revision-skips-one",
			b11bParticipating(unversioned, func(i *IssueDelta) {
				i.Real = true
				i.Sections = []string{"issue"}
				i.Rows = []Version{b11bRow(2)}
				i.RevisionAfter, i.MaxRevisionAfter = 2, 2
			}),
			[]string{"version-chain/revision-gap@x-1"},
		},
		{
			"first-revision-from-nothing",
			b11bParticipating(unversioned, func(i *IssueDelta) {
				i.Real = true
				i.Sections = []string{"issue"}
				i.Rows = []Version{b11bRow(1)}
				i.MaxRevisionAfter = 1
			}),
			nil,
		},
		{
			// A deleted issue has no counter left to hold to the highest revision.
			"deleted-issue-has-no-counter",
			b11bParticipating(func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0; i.Rows = nil }),
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b11bExpectFindings(t, b11bEval(b11bAction(), b11bDelta(c.issue)), c.want...)
		})
	}
}

// B11b.Attribution, the rule. Each new row names the actor the harness supplied, and
// its status says so: claimed when there is an actor, unknown when there is none.
func TestB11bEvaluateAttribution(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		row    func(*Version)
		want   []string
	}{
		{"claimed-by-the-supplied-actor", b11bAction(), func(*Version) {}, nil},
		{"wrong-actor", b11bAction(), func(v *Version) { v.Actor = "someone-else" }, []string{"version-attribution/change-actor@x-1"}},
		{"no-actor-recorded", b11bAction(), func(v *Version) { v.Actor = "" }, []string{"version-attribution/change-actor@x-1"}},
		{"status-unknown-though-there-is-an-actor", b11bAction(), func(v *Version) { v.Status = "unknown" }, []string{"version-attribution/attribution-status@x-1"}},
		{
			"wrong-actor-and-wrong-status",
			b11bAction(),
			func(v *Version) { v.Actor = "someone-else"; v.Status = "unknown" },
			[]string{"version-attribution/change-actor@x-1", "version-attribution/attribution-status@x-1"},
		},
		{
			"empty-actor-is-unknown",
			b11bAction(func(a *Action) { a.Actor = "" }),
			func(v *Version) { v.Actor = ""; v.Status = "unknown" },
			nil,
		},
		{
			"empty-actor-claimed-is-wrong",
			b11bAction(func(a *Action) { a.Actor = "" }),
			func(v *Version) { v.Actor = ""; v.Status = "claimed" },
			[]string{"version-attribution/attribution-status@x-1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issue := b11bParticipating(func(i *IssueDelta) { i.Rows = []Version{b11bRow(2, c.row)} })
			b11bExpectFindings(t, b11bEval(c.action, b11bDelta(issue)), c.want...)
		})
	}
}

// B11b.EpochStable, the rule. Inside a segment the store's epoch is constant and
// every row carries it. A scenario that declares a bump expects the epoch to change
// during the action; a change nobody declared, and a declared one that did not
// happen, are findings.
func TestB11bEpochStable(t *testing.T) {
	withEpochs := func(before, after int) func(*Delta) {
		return func(d *Delta) { d.EpochBefore, d.Epoch = before, after }
	}
	rowEpoch := func(n int) func(*IssueDelta) {
		return func(i *IssueDelta) { i.Rows = []Version{b11bRow(2, func(v *Version) { v.Epoch = n })} }
	}

	cases := []struct {
		name  string
		delta func(*Delta)
		issue func(*IssueDelta)
		bump  bool
		want  []string
	}{
		{"constant-epoch", withEpochs(1, 1), rowEpoch(1), false, nil},
		{"a-row-with-the-wrong-epoch", withEpochs(1, 1), rowEpoch(2), false, []string{"version-epoch/row-epoch@x-1"}},
		{
			// The epoch moved during the action and nobody said it would: one finding for
			// the action, on its issue, and the rows are not also reported against a
			// target that moved.
			"undeclared-bump", withEpochs(1, 2), rowEpoch(1), false, []string{"version-epoch/undeclared-bump@x-1"},
		},
		{"declared-bump-that-happened", withEpochs(1, 2), rowEpoch(2), true, nil},
		{"declared-bump-that-did-not-happen", withEpochs(1, 1), rowEpoch(1), true, []string{"version-epoch/declared-bump-missing@x-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := b11bDelta(b11bParticipating(c.issue))
			c.delta(&d)
			opts := b11bOptions()
			opts.EpochBump = c.bump
			b11bExpectFindings(t, Evaluate(b11bAction(), d, opts), c.want...)
		})
	}

	// The epoch belongs to the store and not to an issue: a bump is one finding for the
	// action however many issues the action touched, and an action that touched none
	// still has its epoch looked at.
	t.Run("a-bump-is-one-finding-per-action", func(t *testing.T) {
		d := b11bDelta(b11bParticipating(rowEpoch(1)), b11bParticipating(rowEpoch(1), func(i *IssueDelta) { i.ID = "x-2" }))
		withEpochs(1, 2)(&d)
		b11bExpectFindings(t, b11bEval(b11bAction(), d), "version-epoch/undeclared-bump@x-1")
	})
	t.Run("a-bump-by-an-action-that-touched-no-issue", func(t *testing.T) {
		d := b11bDelta()
		withEpochs(1, 2)(&d)
		b11bExpectFindings(t, b11bEval(b11bAction(), d), "version-epoch/undeclared-bump@x-1")
	})
}

// B11b.TimeOrdering, the rule. A row is written inside the action's window, give or
// take the clock tolerance, and an issue's rows never go back in time.
func TestB11bTimeOrdering(t *testing.T) {
	at := func(offset time.Duration) func(*IssueDelta) {
		return func(i *IssueDelta) { i.Rows = []Version{b11bRow(2, func(v *Version) { v.At = b11bStart.Add(offset) })} }
	}
	end := b11bWindow

	cases := []struct {
		name  string
		issue func(*IssueDelta)
		want  []string
	}{
		{"inside-the-window", at(time.Second), nil},
		{"at-the-start", at(0), nil},
		{"at-the-end", at(end), nil},
		{"long-before-the-window", at(-time.Hour), []string{"version-time/outside-window@x-1"}},
		{"long-after-the-window", at(end + time.Hour), []string{"version-time/outside-window@x-1"}},
		{"just-inside-the-tolerance-before", at(-b11bTolerance + time.Millisecond), nil},
		{"just-outside-the-tolerance-before", at(-b11bTolerance - time.Millisecond), []string{"version-time/outside-window@x-1"}},
		{"just-inside-the-tolerance-after", at(end + b11bTolerance - time.Millisecond), nil},
		{"just-outside-the-tolerance-after", at(end + b11bTolerance + time.Millisecond), []string{"version-time/outside-window@x-1"}},
		{
			// The new row is stamped earlier than the issue's previous one, both inside
			// the window, so the window alone cannot see it.
			"earlier-than-the-previous-row",
			func(i *IssueDelta) { i.PreviousChangeAt = b11bStart.Add(1500 * time.Millisecond) },
			[]string{"version-time/decreasing@x-1"},
		},
		{"same-instant-as-the-previous-row", func(i *IssueDelta) { i.PreviousChangeAt = b11bStart.Add(time.Second) }, nil},
		{"no-previous-row", func(i *IssueDelta) { i.PreviousChangeAt = time.Time{} }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b11bExpectFindings(t, b11bEval(b11bAction(), b11bDelta(b11bParticipating(c.issue))), c.want...)
		})
	}

	t.Run("rows-of-one-action-in-order", func(t *testing.T) {
		issue := b11bParticipating(func(i *IssueDelta) {
			i.Rows = []Version{
				b11bRow(2, func(v *Version) { v.At = b11bStart.Add(1500 * time.Millisecond) }),
				b11bRow(3, func(v *Version) { v.At = b11bStart.Add(time.Second) }),
			}
			i.RevisionAfter, i.MaxRevisionAfter = 3, 3
		})
		got := b11bEval(b11bAction(), b11bDelta(issue))
		b11bExpectFindings(t, got, "version-count/over-mint@x-1", "version-time/decreasing@x-1")
	})
}

// B11b.DeleteSurvives, the rule. History is only added to: a row that was there
// before the action is there after it and unchanged, and that holds for a delete
// most of all. A removal mark on a row is recorded and not judged.
func TestB11bEvaluateSurvival(t *testing.T) {
	deleted := func(mods ...func(*IssueDelta)) IssueDelta {
		return b11bParticipating(append([]func(*IssueDelta){func(i *IssueDelta) {
			i.Exists = false
			i.RevisionAfter = 0
			i.Rows = nil
			i.MaxRevisionAfter = i.MaxRevisionBefore
		}}, mods...)...)
	}
	del := b11bAction(func(a *Action) { a.Kind = "delete" })

	t.Run("a-delete-that-keeps-its-history", func(t *testing.T) {
		got := b11bEval(del, b11bDelta(deleted()))
		b11bExpectFindings(t, got)
		if got.RemovedMarked {
			t.Errorf("RemovedMarked is true, and no row carries a removal mark")
		}
	})
	t.Run("rows-lost", func(t *testing.T) {
		b11bExpectFindings(t, b11bEval(del, b11bDelta(deleted(func(i *IssueDelta) { i.Lost = 1 }))), "version-survival/rows-lost@x-1")
	})
	t.Run("rows-altered", func(t *testing.T) {
		b11bExpectFindings(t, b11bEval(del, b11bDelta(deleted(func(i *IssueDelta) { i.Altered = 1 }))), "version-survival/rows-altered@x-1")
	})
	t.Run("a-removal-mark-is-recorded-and-not-judged", func(t *testing.T) {
		got := b11bEval(del, b11bDelta(deleted(func(i *IssueDelta) { i.Marked = true })))
		b11bExpectFindings(t, got)
		if !got.RemovedMarked {
			t.Errorf("RemovedMarked is false, and a row carries a removal mark")
		}
	})
	t.Run("any-action-is-held-to-it", func(t *testing.T) {
		b11bExpectFindings(t, b11bEval(b11bAction(), b11bDelta(b11bParticipating(func(i *IssueDelta) { i.Lost = 2 }))), "version-survival/rows-lost@x-1")
	})
}

// B11b.CascadeClassified, the rule. A delete removes the edges other issues own to
// the deleted one. For an owner whose text does not cite the deleted id the product
// mints nothing for that, and its own completeness test accepts it, so a real change
// with no row, of exactly that shape, is a version-gap and not an under-mint. Every
// other shape of a real change with no row stays an under-mint.
func TestB11bEvaluateCascade(t *testing.T) {
	deleteOf := func(id string) Action {
		return b11bAction(func(a *Action) { a.Kind = "delete"; a.Issue = id })
	}
	owner := func(mods ...func(*IssueDelta)) IssueDelta {
		return b11bParticipating(append([]func(*IssueDelta){func(i *IssueDelta) {
			i.Rows = nil
			i.RevisionAfter, i.MaxRevisionAfter = 1, 1
			i.Sections = []string{"dependencies"}
			i.RemovedEdges = []string{"x-2"}
		}}, mods...)...)
	}

	cases := []struct {
		name   string
		action Action
		issue  IssueDelta
		want   []string
		gap    bool
	}{
		{"edge-removal-of-a-non-citing-owner", deleteOf("x-2"), owner(), []string{"version-gap/cascade-edge-removal@x-1"}, true},
		{
			// The owner's text cites the deleted id, so the delete rewrites it, and the
			// product mints exactly one row for it.
			"citing-owner-shows-one-row",
			deleteOf("x-2"),
			owner(func(i *IssueDelta) {
				i.Sections = []string{"issue", "dependencies"}
				i.Rows = []Version{b11bRow(2)}
				i.RevisionAfter, i.MaxRevisionAfter = 2, 2
			}),
			nil, false,
		},
		{
			"citing-owner-with-no-row-is-an-under-mint",
			deleteOf("x-2"),
			owner(func(i *IssueDelta) { i.Sections = []string{"issue", "dependencies"} }),
			[]string{"version-count/under-mint@x-1"}, false,
		},
		{
			"edge-to-another-issue-is-an-unknown-cause",
			deleteOf("x-2"),
			owner(func(i *IssueDelta) { i.RemovedEdges = []string{"x-3"} }),
			[]string{"version-count/under-mint@x-1"}, false,
		},
		{
			"an-edge-added-as-well-is-an-unknown-cause",
			deleteOf("x-2"),
			owner(func(i *IssueDelta) { i.AddedEdges = []string{"x-4"} }),
			[]string{"version-count/under-mint@x-1"}, false,
		},
		{
			"an-edge-removal-by-dep-remove-is-not-a-cascade",
			b11bAction(func(a *Action) { a.Kind = "dep_remove" }),
			owner(),
			[]string{"version-count/under-mint@x-1"}, false,
		},
		{
			"a-label-change-is-not-a-cascade",
			deleteOf("x-2"),
			owner(func(i *IssueDelta) { i.Sections = []string{"labels"}; i.RemovedEdges = nil }),
			[]string{"version-count/under-mint@x-1"}, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := b11bEval(c.action, b11bDelta(c.issue))
			b11bExpectFindings(t, got, c.want...)
			if c.gap && got.Participating != 1 {
				t.Errorf("Participating = %d, want the change counted: a gap is a real change that minted nothing", got.Participating)
			}
		})
	}
}

// The causes a real change may mint nothing for are a static table: each has a
// reason and a citation, an unknown cause is never in it, and the cascade is first.
func TestB11bExemptCauses(t *testing.T) {
	causes := ExemptCauses()
	if len(causes) == 0 {
		t.Fatal("ExemptCauses is empty")
	}
	if causes[0].Name != "cascade-edge-removal" {
		t.Errorf("the first cause is %q, want cascade-edge-removal", causes[0].Name)
	}
	seen := map[string]bool{}
	for _, c := range causes {
		if c.Name == "" || c.Reason == "" || c.Citation == "" {
			t.Errorf("cause %+v must have a name, a reason and a citation", c)
		}
		if seen[c.Name] {
			t.Errorf("cause %q is listed twice", c.Name)
		}
		seen[c.Name] = true
	}
}

// B11b.FenceAccounting. Every real change the ledger records is counted once, as
// legacy or participating, by the state of its issue before the action ran. A change
// the ledger did not record is counted in neither, whatever a wrapper minted for it.
func TestB11bFenceAccounting(t *testing.T) {
	cases := []struct {
		name         string
		delta        Delta
		legacy       int
		participates int
	}{
		{
			"fenced-mixed",
			b11bDelta(b11bLegacy(), b11bParticipating(func(i *IssueDelta) { i.ID = "x-2" }), b11bLegacy(func(i *IssueDelta) { i.ID = "x-3" })),
			2, 1,
		},
		{
			// Promoted by the action, an issue was a legacy record when the action began
			// and is counted as one; the breach is V0's to report.
			"classified-at-head-before",
			b11bDelta(b11bLegacy(func(i *IssueDelta) { i.GenerationAfter = b11bGen(1) })),
			1, 0,
		},
		{
			"classified-at-head-before-the-other-way",
			b11bDelta(b11bParticipating(func(i *IssueDelta) { i.GenerationAfter = nil })),
			0, 1,
		},
		{
			"created-by-the-action",
			b11bDelta(b11bParticipating(func(i *IssueDelta) {
				i.Existed = false
				i.GenerationBefore = nil
				i.RevisionBefore, i.RevisionAfter = 0, 1
				i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 1
				i.PreviousChangeAt = time.Time{}
				i.Rows = []Version{b11bRow(1)}
			})),
			0, 1,
		},
		{
			"delete-of-legacy",
			b11bDelta(b11bLegacy(func(i *IssueDelta) { i.Exists = false; i.RevisionAfter = 0 })),
			1, 0,
		},
		{
			// A wrapper that mints a row for an issue that did not change: the change is
			// not a change, so it is counted in neither field, and V0 or V1 reports the
			// row.
			"no-op-not-counted",
			b11bDelta(
				b11bLegacy(func(i *IssueDelta) {
					i.Real = false
					i.Sections = nil
					i.Rows = []Version{b11bRow(1)}
				}),
				b11bParticipating(func(i *IssueDelta) { i.ID = "x-2"; i.Real = false; i.Sections = nil }),
			),
			0, 0,
		},
		{
			// A store with no fence column has no legacy records: every real change is a
			// participating one, whatever the generation column would have said.
			"fence-absent",
			func() Delta {
				d := b11bDelta(b11bParticipating(func(i *IssueDelta) {
					i.GenerationBefore, i.GenerationAfter = nil, nil
					i.RevisionBefore, i.RevisionAfter = 1, 1
					i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 1
					i.PreviousChangeAt = time.Time{}
					i.Rows = []Version{b11bRow(1)}
				}))
				d.Fence = false
				return d
			}(),
			0, 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := b11bEval(b11bAction(), c.delta)
			if got.Legacy != c.legacy || got.Participating != c.participates {
				t.Errorf("legacy = %d, participating = %d; want %d and %d", got.Legacy, got.Participating, c.legacy, c.participates)
			}
			changes := 0
			for _, i := range c.delta.Issues {
				if i.Real {
					changes++
				}
			}
			if got.Legacy+got.Participating != changes {
				t.Errorf("legacy + participating = %d, and the ledger recorded %d real changes", got.Legacy+got.Participating, changes)
			}
		})
	}
}

// B11b.SeededBaseline, the branch for a store without the write fence; the branch for
// one that has it is TestB11bSeededBaseline in the driver package, which replays a
// seeded clone. Without participation_generation there are no legacy records, so
// nothing is held back from the history: the first replayed change of a seeded issue
// mints its revision 1, V2 takes that as the start of the chain, the change counts as
// a participating one, and V0 is not evaluated.
func TestB11bSeededBaseline(t *testing.T) {
	t.Run("unfenced", func(t *testing.T) {
		unfenced := func(mods ...func(*IssueDelta)) Delta {
			issue := b11bParticipating(func(i *IssueDelta) {
				i.GenerationBefore, i.GenerationAfter = nil, nil
				i.PreviousChangeAt = time.Time{}
			})
			for _, mod := range mods {
				mod(&issue)
			}
			d := b11bDelta(issue)
			d.Fence = false
			return d
		}

		first := b11bEval(b11bAction(), unfenced(func(i *IssueDelta) {
			i.RevisionBefore, i.RevisionAfter = 1, 1
			i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 1
			i.Rows = []Version{b11bRow(1)}
		}))
		b11bExpectFindings(t, first)
		if first.Legacy != 0 || first.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want 0 and 1: without the fence every change takes part", first.Legacy, first.Participating)
		}

		// The chain carries on from the revision the first change minted.
		second := b11bEval(b11bAction(), unfenced(func(i *IssueDelta) {
			i.RevisionBefore, i.RevisionAfter = 1, 2
			i.MaxRevisionBefore, i.MaxRevisionAfter = 1, 2
			i.PreviousChangeAt = b11bStart.Add(-time.Hour)
			i.Rows = []Version{b11bRow(2)}
		}))
		b11bExpectFindings(t, second)

		// With nothing to excuse it, a first change that mints nothing is an
		// under-mint and not a held-back record.
		silent := b11bEval(b11bAction(), unfenced(func(i *IssueDelta) {
			i.RevisionBefore, i.RevisionAfter = 1, 1
			i.MaxRevisionBefore, i.MaxRevisionAfter = 0, 0
			i.Rows = nil
		}))
		b11bExpectFindings(t, silent, "version-count/under-mint@x-1")
		if silent.Legacy != 0 || silent.Participating != 1 {
			t.Errorf("legacy %d, participating %d; want 0 and 1", silent.Legacy, silent.Participating)
		}
	})
}
