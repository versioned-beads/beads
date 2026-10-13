package driver

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// The version accounting, end to end: a replay of a real history through a real bd
// has one version row per real change, and a bd that does one thing wrong is named
// for exactly that. The checks themselves are in the versions package, over
// synthetic input; these tests run them in a run and read what the run wrote.

// ---- a faithful run ------------------------------------------------------------

// B11b.OneRowPerRealChange, its negative control as a whole: a real replay of a
// real history, through a bd that is left alone, finds nothing. Every check ran
// and none fired, and the summary says how many changes it stands for.
func TestB11bFaithfulRunHasNoFindings(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a, b := o.ids[0], o.ids[1]
	bin, log := bdStandIn(t, o.bin, "")
	f := newFixtureRun(t, o, bin)
	mustComplete(t, f)
	read := b11bRead(t, f.outDir)
	issues := []string{a, b, a, b, a}

	t.Run("no-findings", func(t *testing.T) {
		out := read.under(t)
		if len(out.results) != b11bSteps {
			t.Fatalf("%d result rows, want one for each of the %d steps", len(out.results), b11bSteps)
		}
		for i, commit := range o.heads {
			r := out.row(commit, issues[i])
			if string(r.RowVerdict) != "matched" || string(r.VersionVerdict) != "matched" {
				t.Errorf("%s at %s: row axis %q, version axis %q, want both matched", issues[i], commit, r.RowVerdict, r.VersionVerdict)
			}
		}
		out.clean()
	})

	t.Run("summary", func(t *testing.T) {
		out := read.under(t)
		reads := b11aReadsOf(t, out.raw)
		if reads.WriteFence == nil || !*reads.WriteFence {
			t.Errorf("write_fence = %v, want true: the build under test has the fence", reads.WriteFence)
		}
		var doc struct {
			Excludes []struct {
				Name string `json:"name"`
			} `json:"durable_state_excludes"`
		}
		if err := json.Unmarshal(out.raw, &doc); err != nil {
			t.Fatalf("summary.json: %v", err)
		}
		var names []string
		for _, e := range doc.Excludes {
			names = append(names, e.Name)
		}
		for _, want := range []string{"comments", "product-read-surface", "is_blocked", "wisps"} {
			if !slices.Contains(names, want) {
				t.Errorf("durable_state_excludes names %q, want %q among them: what the axis does not check stays stated", names, want)
			}
		}
		var marking string
		if err := json.Unmarshal(b11bReadsOf(t, out.raw)["removed_marking"], &marking); err != nil || marking != "not-in-production" {
			t.Errorf("removed_marking = %q (%v), want not-in-production", marking, err)
		}
		if n := out.count("legacy_changes"); n != 0 {
			t.Errorf("legacy_changes = %d, want 0: a run that starts from an empty store has no legacy record", n)
		}
		if n := out.count("participating_changes"); n != b11bSteps {
			t.Errorf("participating_changes = %d, want %d: one real change for each command", n, b11bSteps)
		}
	})

	// B11b.OneCommitPerAction, in a run: every command the replay ran for an action
	// was given the commit mode and an actor, once each. The commit mode makes a
	// command one Dolt commit; the actor is the one every version row is held to.
	t.Run("every-action-is-pinned", func(t *testing.T) {
		var calls []string
		for _, c := range standInCalls(t, log) {
			switch w := strings.Fields(c); {
			case len(w) == 0:
			case slices.Contains([]string{"create", "update", "close", "delete", "dep", "label"}, w[0]):
				calls = append(calls, c)
			}
		}
		if len(calls) != b11bSteps {
			t.Fatalf("%d action commands were run, want %d: %q", len(calls), b11bSteps, calls)
		}
		var changers []string
		for _, c := range calls {
			w := strings.Fields(c)
			var mode, actor []string
			for _, word := range w {
				if word == "--dolt-auto-commit=on" {
					mode = append(mode, word)
				}
				if v, ok := strings.CutPrefix(word, "--actor="); ok {
					actor = append(actor, v)
				}
			}
			if len(mode) != 1 {
				t.Errorf("%q carries --dolt-auto-commit=on %d times, want once", c, len(mode))
			}
			if len(actor) != 1 || actor[0] == "" {
				t.Errorf("%q carries the actors %q, want exactly one, not empty", c, actor)
				continue
			}
			if w[0] != "create" {
				changers = append(changers, actor[0])
			}
		}
		for _, who := range changers {
			if who != changers[0] {
				t.Errorf("the commands that change an issue ran as %q, want one actor for all of them", changers)
				break
			}
		}
	})
}

// B11b.Attribution, creates: the creator is a column the two sides compare, and the
// only way to give an issue a creator is the actor the create runs as. So a create
// is replayed as the oracle's creator, and its version row is attributed to that
// actor; every other command is the replay's own, and its row says so. Pinning one
// actor on everything would make every replayed issue a different issue from the
// oracle's.
func TestB11bCreateIsReplayedAsItsCreator(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bCreatorOracle(t)
	a := o.ids[0]
	f := newFixtureRun(t, o, o.bin)
	mustComplete(t, f)
	out := b11bRead(t, f.outDir)

	for _, commit := range o.heads {
		r := out.row(commit, a)
		if string(r.RowVerdict) != "matched" || string(r.VersionVerdict) != "matched" {
			t.Errorf("%s at %s: row axis %q, version axis %q, want both matched: the creator is a compared column", a, commit, r.RowVerdict, r.VersionVerdict)
		}
	}
	q := doltcli.SQLQuote(a)
	if got := seedQueryText(t, f.workData, "SELECT created_by FROM issues WHERE id = "+q); got != b11bCreator {
		t.Errorf("created_by = %q, want %q, the oracle's", got, b11bCreator)
	}
	if got := seedQueryText(t, f.workData, "SELECT change_actor FROM issue_versions WHERE issue_id = "+q+" AND revision = 1"); got != b11bCreator {
		t.Errorf("the create's version row is attributed to %q, want %q: it ran as the creator", got, b11bCreator)
	}
	if got := seedQueryText(t, f.workData, "SELECT change_actor FROM issue_versions WHERE issue_id = "+q+" AND revision = 2"); got == "" || got == b11bCreator {
		t.Errorf("the change after it is attributed to %q, want the replay's own actor, which is not the creator", got)
	}
	out.clean()
}

// ---- B11b.OverMintDetected, UnderMintDetected, PhantomDetected --------------------

// B11b.OverMintDetected: a bd that records two versions for one change is named
// over-mint, for the issue and the step.
func TestB11bOverMintDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a := o.ids[0]
	f, out := b11bTampered(t, o, "", b11bAfter("update", a,
		b11bSQL("a second row for one change", b11bDuplicateNewest(a), b11bSyncCounter(a))))

	if got := b11bVersions(t, f.workData, a); got != 4 {
		t.Fatalf("the issue has %d version rows, want 4: the create, the change, the stand-in's second row and the close", got)
	}
	step := o.heads[b11bUpdateA]
	out.one(b11bCount, "over-mint", step, a)
	out.requireVersionMismatch(step, a)
	out.onlyFinding(b11bCount)
}

// B11b.UnderMintDetected: a bd that changes an issue and records no version is
// named under-mint. The single-read comparison also objects to an issue with no
// stored version; the finding here is the new one, and it is the only new one.
func TestB11bUnderMintDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a := o.ids[0]
	f, out := b11bTampered(t, o, "", b11bAfter("create", a,
		b11bSQL("the version of a create goes missing", b11bDropVersions(a))))

	if got := b11bVersions(t, f.workData, a); got != 2 {
		t.Fatalf("the issue has %d version rows, want 2: the change and the close, and none for the create", got)
	}
	step := o.heads[b11bCreateA]
	out.one(b11bCount, "under-mint", step, a)
	out.requireVersionMismatch(step, a)
	out.onlyFinding(b11bCount, b11bState)
}

// B11b.PhantomDetected: a bd that records a version for an issue the command did
// not change is named phantom, and filed under the issue that got the row.
func TestB11bPhantomDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a, b := o.ids[0], o.ids[1]
	f, out := b11bTampered(t, o, "", b11bAfter("create", b,
		b11bSQL("a row for an issue the command did not change", b11bDuplicateNewest(a), b11bSyncCounter(a))))

	if got := b11bVersions(t, f.workData, a); got != 4 {
		t.Fatalf("the issue has %d version rows, want 4: the create, the stand-in's row, the change and the close", got)
	}
	step := o.heads[b11bCreateB]
	out.one(b11bCount, "phantom", step, a)
	if !out.stepIsMismatch(step) {
		t.Errorf("no row of the step is a mismatch: the issue with the phantom row is not read there, and the step still has to show it")
	}
	out.onlyFinding(b11bCount)
}

// ---- B11b.ChainInvariants ---------------------------------------------------------

func TestB11bChainDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a, b := o.ids[0], o.ids[1]

	// A counter that is not where the rows are.
	t.Run("current-revision", func(t *testing.T) {
		f, out := b11bTampered(t, o, "", b11bAfter("update", b,
			b11bSQL("a counter that is ahead of the rows", b11bSetCounter(b, "current_revision + 1"))))
		q := doltcli.SQLQuote(b)
		counter := seedQueryInt(t, f.workData, "SELECT current_revision FROM issues WHERE id = "+q)
		newest := seedQueryInt(t, f.workData, "SELECT MAX(revision) FROM issue_versions WHERE issue_id = "+q)
		if counter != newest+1 {
			t.Fatalf("the counter is %d and the newest revision %d, want the counter one ahead", counter, newest)
		}
		step := o.heads[b11bUpdateB]
		out.one(b11bChain, "current-revision", step, b)
		out.requireVersionMismatch(step, b)
		out.onlyFinding(b11bChain)
	})

	// A revision that is not the next one. The close after it carries on from it,
	// so the run has one gap and not two.
	t.Run("revision-gap", func(t *testing.T) {
		f, out := b11bTampered(t, o, "", b11bAfter("update", a,
			b11bSQL("a revision that skips ahead", b11bSetVersion(a, 2, "revision = 5"), b11bSetCounter(a, "5"))))
		q := doltcli.SQLQuote(a)
		if got := seedQueryInt(t, f.workData, "SELECT MAX(revision) FROM issue_versions WHERE issue_id = "+q); got != 6 {
			t.Fatalf("the newest revision is %d, want 6: the skip to 5 and the close after it", got)
		}
		step := o.heads[b11bUpdateA]
		out.one(b11bChain, "revision-gap", step, a)
		out.requireVersionMismatch(step, a)
		out.onlyFinding(b11bChain)
	})
}

// ---- B11b.Attribution -------------------------------------------------------------

func TestB11bAttributionDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a := o.ids[0]
	step := o.heads[b11bUpdateA]

	// A row attributed to someone the harness did not run the command as.
	t.Run("change-actor", func(t *testing.T) {
		_, out := b11bTampered(t, o, "", b11bAfter("update", a,
			b11bSQL("a row with another actor", b11bSetVersion(a, 2, "change_actor = 'someone-else'"))))
		out.one(b11bAttribution, "change-actor", step, a)
		out.requireVersionMismatch(step, a)
		out.onlyFinding(b11bAttribution)
	})

	// A row that says the actor is unknown although the command ran as one.
	t.Run("attribution-status", func(t *testing.T) {
		_, out := b11bTampered(t, o, "", b11bAfter("update", a,
			b11bSQL("a row that disowns its actor", b11bSetVersion(a, 2, "attribution_status = 'unknown'"))))
		out.one(b11bAttribution, "attribution-status", step, a)
		out.requireVersionMismatch(step, a)
		out.onlyFinding(b11bAttribution)
	})
}

// ---- B11b.EpochStable -------------------------------------------------------------

func TestB11bEpochDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	a := o.ids[0]

	// A row from another epoch than the store's.
	t.Run("row-epoch", func(t *testing.T) {
		_, out := b11bTampered(t, o, "", b11bAfter("update", a,
			b11bSQL("a row from another epoch", b11bSetVersion(a, 2, "epoch = 2"))))
		step := o.heads[b11bUpdateA]
		out.one(b11bEpoch, "row-epoch", step, a)
		out.requireVersionMismatch(step, a)
		out.onlyFinding(b11bEpoch)
	})

	// The store's epoch moves in a command no scenario said would move it. The last
	// command is the one that does it, so the rows after it cannot be what is blamed.
	t.Run("undeclared-bump", func(t *testing.T) {
		_, out := b11bTampered(t, o, "", b11bAfter("close", a,
			b11bSQL("the store's epoch moves", "UPDATE store_epoch SET epoch = 2 WHERE id = 1")))
		step := o.heads[b11bCloseA]
		out.one(b11bEpoch, "undeclared-bump", step, a)
		out.requireVersionMismatch(step, a)
		out.onlyFinding(b11bEpoch)
	})
}

// ---- B11b.TimeOrdering --------------------------------------------------------------

// A row stamped far outside the command that wrote it. A time in the past would be
// two findings, the window and the order; one in the far future is the window only.
func TestB11bTimeWindowDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bOracle(t)
	b := o.ids[1]
	_, out := b11bTampered(t, o, "", b11bAfter("update", b,
		b11bSQL("a row stamped in the far future", b11bSetVersion(b, 2, "change_at = '2099-01-01 00:00:00.000000'"))))

	step := o.heads[b11bUpdateB]
	out.one(b11bTime, "outside-window", step, b)
	out.requireVersionMismatch(step, b)
	out.onlyFinding(b11bTime)
}

// ---- version-state, per action ---------------------------------------------------------

// A stored version that is not the issue's state is a finding after the command
// that wrote it, whatever a later command makes of the issue. The create of Beta
// stores a wrong title, and the edge added to Beta in the same step stores a right
// one, so the state at the end of the step agrees with the oracle and only the
// check that follows each command can see it.
func TestB11bActionStateDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	alpha, beta := o.ids[0], o.ids[1]
	rewrite := `UPDATE issue_versions SET durable_state = CAST(REPLACE(CAST(durable_state AS CHAR), '"title":"Beta"', '"title":"Beta-stored"') AS BINARY) WHERE issue_id = ` + doltcli.SQLQuote(beta)
	f, out := b11bTampered(t, o, "", b11bAfter("create", beta, b11bSQL("a stored title that is not the row's", rewrite)))

	q := doltcli.SQLQuote(beta)
	if got := seedQueryInt(t, f.workData, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = "+q); got != 2 {
		t.Fatalf("Beta has %d version rows, want 2: the create and the edge", got)
	}
	if got := seedQueryInt(t, f.workData, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = "+q+" AND CAST(durable_state AS CHAR) LIKE '%Beta-stored%'"); got != 1 {
		t.Fatalf("%d of Beta's rows hold the rewritten title, want only the create's", got)
	}
	step := o.heads[1]
	m := out.one(b11bState, "issue", step, beta)
	if got, _ := issueMember(t, m.ExpectedJSON, "title"); got != "Beta" {
		t.Errorf("expected title = %v, want the issue's own, Beta", got)
	}
	if got, _ := issueMember(t, m.ActualJSON, "title"); got != "Beta-stored" {
		t.Errorf("actual title = %v, want the stored one, Beta-stored", got)
	}
	out.requireVersionMismatch(step, beta)
	out.onlyFinding(b11bState)
	if r := out.row(o.heads[0], alpha); r.Verdict != VerdictMatched {
		t.Errorf("%s at %s: verdict %q, want matched: the stand-in did not touch it", alpha, o.heads[0], r.Verdict)
	}
}

// ---- B11b.DeleteSurvives ----------------------------------------------------------------

// A delete leaves the deleted issue's versions where they were. The product never
// marks them removed, and the summary records that as a fact about this build.
func TestB11bDeleteSurvives(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bDeleteOracle(t)
	b := o.ids[1]
	step := o.heads[2]

	t.Run("faithful", func(t *testing.T) {
		f := newFixtureRun(t, o, o.bin)
		mustComplete(t, f)
		out := b11bRead(t, f.outDir)
		q := doltcli.SQLQuote(b)
		if got := seedQueryInt(t, f.workData, "SELECT COUNT(*) FROM issues WHERE id = "+q); got != 0 {
			t.Fatalf("the issue is still there after the delete")
		}
		if got := b11bVersions(t, f.workData, b); got != 1 {
			t.Errorf("the deleted issue has %d version rows, want the 1 it had", got)
		}
		var marking string
		if err := json.Unmarshal(b11bReadsOf(t, out.raw)["removed_marking"], &marking); err != nil || marking != "not-in-production" {
			t.Errorf("removed_marking = %q (%v), want not-in-production: the product never sets removed_at", marking, err)
		}
		out.clean()
	})

	// A bd that takes the versions with the issue.
	t.Run("rows-lost", func(t *testing.T) {
		f, out := b11bTampered(t, o, "", b11bAfter("delete", b,
			b11bSQL("the versions of a deleted issue go with it", b11bDropVersions(b))))
		if got := b11bVersions(t, f.workData, b); got != 0 {
			t.Fatalf("the deleted issue still has %d version rows", got)
		}
		out.one(b11bSurvival, "rows-lost", step, b)
		if r := out.row(step, b); r.Verdict != VerdictMismatch || r.Matched {
			t.Errorf("%s at %s: verdict %q matched=%v, want a mismatch", b, step, r.Verdict, r.Matched)
		}
		out.onlyFinding(b11bSurvival)
	})
}

// ---- B11b.CascadeClassified --------------------------------------------------------------

// Deleting an issue removes the edges others hold to it. What the product records
// for the owner of such an edge is the product's to say, and this run records it
// whichever it is: a row for the owner, or none, which the summary counts as a gap
// with its cause. It is never a count finding and never dropped.
func TestB11bCascadeClassifiedInARun(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := b11bCascadeOracle(t)
	alpha, beta := o.ids[0], o.ids[1]
	f := newFixtureRun(t, o, o.bin)
	mustComplete(t, f)
	out := b11bRead(t, f.outDir)

	rows := b11bVersions(t, f.workData, beta)
	var wantGap int
	switch rows {
	case 3: // the replay removed the edge itself, and the product recorded it
		wantGap = 0
	case 2: // the edge went with the delete, and the owner got no row
		wantGap = 1
	default:
		t.Fatalf("the edge's owner has %d version rows, want 3 (the replay removed the edge) or 2 (the delete did)", rows)
	}
	findings := out.findings()
	t.Logf("the edge's owner has %d version rows, and the run counts %d gap", rows, findings[b11bGap])
	if findings[b11bGap] != wantGap {
		t.Errorf("findings[%s] = %d with %d rows for the owner, want %d", b11bGap, findings[b11bGap], rows, wantGap)
	}
	for _, c := range b11bCategories {
		if c != b11bGap && findings[c] != 0 {
			t.Errorf("findings[%s] = %d: a cascade is a recorded gap or a row, never another finding", c, findings[c])
		}
	}
	for _, m := range out.mismatches {
		t.Errorf("a mismatch row for a cascade: %+v", m)
	}
	if r := out.row(o.heads[2], alpha); r.Verdict != VerdictMatched {
		t.Errorf("%s at %s: verdict %q, want matched: a gap does not worsen a step", alpha, o.heads[2], r.Verdict)
	}
}

// ---- B11b.FenceBreachDetected ----------------------------------------------------------

// b11bRequireSeededCounts holds the counters of a run on the seeded copy to the
// tail's: two changes to legacy records and the create of a new issue.
func b11bRequireSeededCounts(t *testing.T, out *b11bOutcome) {
	t.Helper()
	if n := out.count("legacy_changes"); n != 2 {
		t.Errorf("legacy_changes = %d, want 2: the tail changes two issues the seeding left without a record", n)
	}
	if n := out.count("participating_changes"); n != 1 {
		t.Errorf("participating_changes = %d, want 1: the tail creates one issue", n)
	}
}

// B11b.FenceBreachDetected: the write fence says a change to an issue that has no
// record in the history mints nothing and promotes nothing. A bd that breaks it, in
// any of the three ways, is named for that way, for the issue and the step, and the
// breach is the only finding: the version checks that apply to participating issues
// do not run for it, and the single-read comparison's own objection is replaced.
//
// Each breach is made on a copy of the seeded clone, by a bd that runs the real one
// and then does the one thing. The first has the product itself write the row.
func TestB11bFenceBreachDetected(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	first, legacy := b11bSeededStep(t, f, 0)
	third, tail := b11bSeededStep(t, f, 2)
	if tail != f.o.tailID {
		t.Fatalf("the third step is for %s, want the tail's create of %s", tail, f.o.tailID)
	}
	q := doltcli.SQLQuote(legacy)

	// The product mints the row. The stand-in makes the issue a participating one
	// for the length of the command, which is what makes the product write its
	// version, and puts it back as it was afterwards. The row is then exactly what
	// the product writes for the change, so it is the oracle's state, and no
	// comparison of states can tell it from a right one.
	t.Run("row-minted", func(t *testing.T) {
		var counter int
		data, out := b11bSeededCopy(t, f, func(data string) string {
			counter = seedQueryInt(t, data, "SELECT current_revision FROM issues WHERE id = "+q)
			stamp := b11bSQL("the issue takes part for one command",
				"UPDATE issues SET participation_generation = 1, updated_at = updated_at WHERE id = "+q)
			restore := b11bSQL("the issue is a legacy record again",
				"UPDATE issues SET participation_generation = NULL, current_revision = "+strconv.Itoa(counter)+", updated_at = updated_at WHERE id = "+q)
			return b11bStandIn(t, replaytest.BdBin(t), data, b11bWhen("update", legacy, stamp), b11bAfter("update", legacy, restore))
		})
		if got := b11bVersions(t, data, legacy); got != 1 {
			t.Fatalf("the legacy issue has %d version rows, want the 1 the product wrote", got)
		}
		if got := seedQueryInt(t, data, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = "+q+" AND CAST(durable_state AS CHAR) LIKE '%tail description%'"); got != 1 {
			t.Fatalf("the row does not hold the change: it is not the product's own version of it")
		}
		if got := seedQueryInt(t, data, "SELECT COUNT(*) FROM issues WHERE id = "+q+" AND participation_generation IS NULL AND current_revision = "+strconv.Itoa(counter)); got != 1 {
			t.Fatalf("the issue is not a legacy record as it was: the stand-in did not put it back")
		}
		out.one(b11bFence, "row-minted", first, legacy)
		out.requireVersionMismatch(first, legacy)
		out.onlyFinding(b11bFence)
		b11bRequireSeededCounts(t, out)
	})

	// The issue is promoted: it takes part, and no row is written.
	t.Run("promoted", func(t *testing.T) {
		data, out := b11bSeededCopy(t, f, func(data string) string {
			return b11bStandIn(t, replaytest.BdBin(t), data, "", b11bAfter("update", legacy,
				b11bSQL("a legacy record takes part", "UPDATE issues SET participation_generation = 1, updated_at = updated_at WHERE id = "+q)))
		})
		if got := seedQueryInt(t, data, "SELECT COUNT(*) FROM issues WHERE id = "+q+" AND participation_generation IS NOT NULL"); got != 1 {
			t.Fatalf("the issue was not promoted")
		}
		out.one(b11bFence, "promoted", first, legacy)
		out.requireVersionMismatch(first, legacy)
		out.onlyFinding(b11bFence)
		b11bRequireSeededCounts(t, out)
	})

	// The counter moves, and no row is written and nothing is promoted.
	t.Run("revision-moved", func(t *testing.T) {
		var counter int
		data, out := b11bSeededCopy(t, f, func(data string) string {
			counter = seedQueryInt(t, data, "SELECT current_revision FROM issues WHERE id = "+q)
			return b11bStandIn(t, replaytest.BdBin(t), data, "", b11bAfter("update", legacy,
				b11bSQL("a counter moves with no row", b11bSetCounter(legacy, "current_revision + 1"))))
		})
		if got := seedQueryInt(t, data, "SELECT COUNT(*) FROM issues WHERE id = "+q+" AND participation_generation IS NULL AND current_revision = "+strconv.Itoa(counter+1)); got != 1 {
			t.Fatalf("the counter did not move, or the issue was promoted")
		}
		if got := b11bVersions(t, data, legacy); got != 0 {
			t.Fatalf("the legacy issue has %d version rows, want none", got)
		}
		out.one(b11bFence, "revision-moved", first, legacy)
		out.requireVersionMismatch(first, legacy)
		out.onlyFinding(b11bFence)
		b11bRequireSeededCounts(t, out)
	})

	// A row is written for a legacy issue by a command that changed nothing for it:
	// the create of the tail's issue also writes a row for a seeded issue it never
	// touched. That is still a row for a legacy record, and the issue is counted in
	// neither of the two accounting fields, because it had no real change.
	t.Run("row-minted-on-no-op", func(t *testing.T) {
		untouched := f.o.ids[2]
		copyRow := "INSERT INTO issue_versions (issue_id, revision, epoch, durable_state, change_actor, change_agent, change_message, change_at, attribution_status) " +
			"SELECT " + doltcli.SQLQuote(untouched) + ", 1, epoch, durable_state, change_actor, change_agent, change_message, UTC_TIMESTAMP(6), attribution_status " +
			"FROM issue_versions WHERE issue_id = " + doltcli.SQLQuote(tail) + " AND revision = 1"
		data, out := b11bSeededCopy(t, f, func(data string) string {
			return b11bStandIn(t, replaytest.BdBin(t), data, "", b11bAfter("create", tail,
				b11bSQL("a row for a seeded issue the create did not touch", copyRow)))
		})
		if got := b11bVersions(t, data, untouched); got != 1 {
			t.Fatalf("the untouched legacy issue has %d version rows, want the 1 the stand-in wrote", got)
		}
		out.one(b11bFence, "row-minted", third, untouched)
		if !out.stepIsMismatch(third) {
			t.Errorf("no row of the step is a mismatch: the untouched issue is not read there, and the step still has to show the breach")
		}
		out.onlyFinding(b11bFence)
		b11bRequireSeededCounts(t, out)
	})
}

// ---- B11b.SeededBaseline ----------------------------------------------------------------

// B11b.SeededBaseline, the branch for a build with the write fence (the other, for
// a build without it, is TestB11bSeededBaseline in the versions package, over a
// store that has no such column): the issues the seeding left without a record
// stay without one for the whole replay. Nothing is minted for them, nothing
// promoted, no counter moved, and every read of them says legacy. The issue the
// replay creates is the first with a record.
func TestB11bSeededBaseline(t *testing.T) {
	t.Run("fenced", func(t *testing.T) {
		f := requireSeededRun(t, defaultSeedVariant)
		replaytest.Isolate(t)
		data := f.runCfg.WorkDataDir
		if !b11aWriteFence(t, data) {
			t.Fatalf("the store has no write fence, so the fenced baseline cannot be exercised here; the unfenced one is the versions package's")
		}
		reads := b11aReadsOf(t, f.summaryRaw)
		if reads.WriteFence == nil || !*reads.WriteFence {
			t.Errorf("write_fence = %v, want true: the store has the column", reads.WriteFence)
		}

		seed := doltcli.SQLQuote(f.rec.SeedHead)
		for i, id := range f.o.ids[:2] {
			q := doltcli.SQLQuote(id)
			if was, now := seedQueryInt(t, data, "SELECT COUNT(*) FROM issue_versions AS OF "+seed+" WHERE issue_id = "+q), b11bVersions(t, data, id); was != now {
				t.Errorf("%s has %d version rows, and had %d when the seeding finished", id, now, was)
			}
			if n := seedQueryInt(t, data, "SELECT COUNT(*) FROM issues WHERE id = "+q+" AND participation_generation IS NULL"); n != 1 {
				t.Errorf("%s was promoted: it has a generation", id)
			}
			was := seedQueryInt(t, data, "SELECT current_revision FROM issues AS OF "+seed+" WHERE id = "+q)
			if now := seedQueryInt(t, data, "SELECT current_revision FROM issues WHERE id = "+q); was != now {
				t.Errorf("%s has the counter at %d, and had it at %d when the seeding finished", id, now, was)
			}
			if r := f.results[i]; r.IssueID != id || string(r.VersionVerdict) != b11aLegacy {
				t.Errorf("step %d for %s: version axis %q, want %q", i, r.IssueID, r.VersionVerdict, b11aLegacy)
			}
		}

		tail := doltcli.SQLQuote(f.o.tailID)
		if n := seedQueryInt(t, data, "SELECT current_revision FROM issues WHERE id = "+tail); n != 1 {
			t.Errorf("the created issue has the counter at %d, want its first revision", n)
		}
		if n := b11bVersions(t, data, f.o.tailID); n != 1 {
			t.Errorf("the created issue has %d version rows, want 1", n)
		}
		if r := f.results[2]; r.IssueID != f.o.tailID || string(r.VersionVerdict) != "matched" {
			t.Errorf("step 2 for %s: version axis %q, want matched", r.IssueID, r.VersionVerdict)
		}

		out := &b11bOutcome{t: t, results: f.results, mismatches: f.mismatches, raw: f.summaryRaw}
		out.clean()
		b11bRequireSeededCounts(t, out)
	})
}

// ---- B11b.FenceAccounting, in the summary -----------------------------------------------

// The two counters of the summary: how many real changes the run made to issues that
// had no record, and how many to issues that did. They are always written, they sum
// to the changes the ledger saw, and a run that is resumed reports what a run that
// was not interrupted does.
func TestB11bFenceAccountingInTheSummary(t *testing.T) {
	// A run with nothing legacy in it writes the zero.
	t.Run("zeros-emitted", func(t *testing.T) {
		requireBd(t)
		requireDolt(t)
		o := faultOracle(t)
		f := newFixtureRun(t, o, o.bin)
		mustComplete(t, f)
		out := b11bRead(t, f.outDir)
		if n := out.count("legacy_changes"); n != 0 {
			t.Errorf("legacy_changes = %d, want 0", n)
		}
		if n := out.count("participating_changes"); n != 3 {
			t.Errorf("participating_changes = %d, want 3: the create of Alpha, the create of Beta and its edge", n)
		}
	})

	// The seeded run, as the fixture has it: two legacy changes and one create.
	t.Run("seeded-expected", func(t *testing.T) {
		f := requireSeededRun(t, defaultSeedVariant)
		replaytest.Isolate(t)
		reads := b11aReadsOf(t, f.summaryRaw)
		if reads.WriteFence == nil || !*reads.WriteFence {
			t.Errorf("write_fence = %v, want true", reads.WriteFence)
		}
		legacy, participating := b11bInt(t, f.summaryRaw, "legacy_changes"), b11bInt(t, f.summaryRaw, "participating_changes")
		if legacy != 2 || participating != 1 {
			t.Errorf("legacy_changes = %d and participating_changes = %d, want 2 and 1", legacy, participating)
		}
		if legacy+participating != len(f.results) {
			t.Errorf("the counters sum to %d, want the %d real changes of the tail", legacy+participating, len(f.results))
		}
	})

	// A run killed in the middle of a step and resumed has the counters the run that
	// was not killed has: they are folded from the rows the run keeps.
	t.Run("resume-reproduces", func(t *testing.T) {
		requireBd(t)
		requireDolt(t)
		o := lifecycleOracle(t)
		clean := cleanBaseline(t, "lifecycle", o, "")
		cleanSummary := []byte(clean.files[summaryFile])
		if n := b11bInt(t, cleanSummary, "participating_changes"); n != 5 {
			t.Fatalf("the clean run's participating_changes = %d, want 5: both creates, the edge and the two updates", n)
		}
		if n := b11bInt(t, cleanSummary, "legacy_changes"); n != 0 {
			t.Fatalf("the clean run's legacy_changes = %d, want 0", n)
		}

		r := newCLIRig(t, o)
		r.killAfterApply("create*Beta*")
		id := r.runKilled()
		res := r.run("--resume", id)
		if got := requireCompleted(t, "the resumed run", res); got != id {
			t.Fatalf("the resume made a run of its own, %s, instead of finishing %s", got, id)
		}
		resumed := outcome(t, r.outDir, id)
		requireSameOutcome(t, "the resumed run", clean.files, resumed)
		got := []byte(resumed[summaryFile])
		for _, key := range []string{"legacy_changes", "participating_changes"} {
			if a, b := b11bInt(t, got, key), b11bInt(t, cleanSummary, key); a != b {
				t.Errorf("the resumed run's %s = %d, want the clean run's %d", key, a, b)
			}
		}
	})
}
