package driver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/replay/versions"
	"github.com/steveyegge/beads/internal/types"
)

// The values the version axis reports besides matched, mismatch and uncomparable.
// They are pinned as literals: the spelling is what a summary reader greps for.
const (
	b11aLegacy       = "version-legacy"
	b11aRefused      = "version-not-run(enable-refused)"
	b11aNoRow        = "version-not-run(no-row)"
	b11aWisp         = "version-not-run(wisp)"
	b11aVersionState = "version-state"
)

// B11a.CandidateSeam: the driver reads the work clone through two readers, the
// rows and the versions the product stored, and reports a verdict for each. A step
// is as bad as the worse of the two. The version axis has values the row axis
// has not: a legacy record, and a read that did not run. Neither is a pass, and
// neither is a mismatch.
func TestB11aCandidateSeam(t *testing.T) {
	storedBig := func(i *types.Issue) { i.Metadata = json.RawMessage(`{"n":9007199254740993}`) }
	otherRow := func(env *fakeEnv) { env.candidate["x-1"] = b11aView("x-1", "OTHER") }

	cases := []struct {
		name  string
		setup func(env *fakeEnv)

		wantStep    Verdict
		wantRow     string
		wantVersion string
		rowMismatch bool   // the row axis stored a mismatch
		category    string // the category of the version axis's mismatch, "" when it has none
		section     string // its section, "" when it has none
	}{
		{
			name:     "everything agrees",
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: "matched",
		},
		{
			name: "the stored title differs",
			setup: func(env *fakeEnv) {
				env.versioned = b11aStored(env, func(i *types.Issue) { i.Title = "T-stored" })
			},
			wantStep: VerdictMismatch, wantRow: "matched", wantVersion: "mismatch",
			category: b11aVersionState, section: "issue",
		},
		{
			name: "the stored version has an edge the rows lack",
			setup: func(env *fakeEnv) {
				env.versioned = b11aStored(env, func(i *types.Issue) {
					i.Dependencies = []*types.Dependency{{IssueID: "x-1", DependsOnID: "y-1", Type: types.DepBlocks}}
				})
			},
			wantStep: VerdictMismatch, wantRow: "matched", wantVersion: "mismatch",
			category: b11aVersionState, section: "dependencies",
		},
		{
			name: "the stored title and edges both differ",
			setup: func(env *fakeEnv) {
				env.versioned = b11aStored(env, func(i *types.Issue) {
					i.Title = "T-stored"
					i.Dependencies = []*types.Dependency{{IssueID: "x-1", DependsOnID: "y-1", Type: types.DepBlocks}}
				})
			},
			wantStep: VerdictMismatch, wantRow: "matched", wantVersion: "mismatch",
			category: b11aVersionState, section: "issue+dependencies",
		},
		{
			name: "only stamps differ",
			setup: func(env *fakeEnv) {
				later := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
				env.versioned = b11aStored(env, func(i *types.Issue) {
					i.CreatedAt, i.UpdatedAt = later, later
					i.StartedAt, i.ClosedAt, i.CompactedAt = &later, &later, &later
					i.LeaseExpiresAt, i.HeartbeatAt = &later, &later
					i.LeaseGrantedNode = "node-7"
					i.ClosedBySession = "sess-1"
					i.IsBlocked = true
				})
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: "matched",
		},
		{
			name:     "only the work clone's rows differ",
			setup:    otherRow,
			wantStep: VerdictMismatch, wantRow: "mismatch", wantVersion: "matched", rowMismatch: true,
		},
		{
			name: "both axes differ",
			setup: func(env *fakeEnv) {
				otherRow(env)
				env.versioned = b11aStored(env, func(i *types.Issue) { i.Title = "T-stored" })
			},
			wantStep: VerdictMismatch, wantRow: "mismatch", wantVersion: "mismatch", rowMismatch: true,
			category: b11aVersionState, section: "issue",
		},
		{
			name: "the issue has no stored version and is not legacy",
			setup: func(env *fakeEnv) {
				env.versioned = b11aFixed(env, State{Version: &versions.Reading{}})
			},
			wantStep: VerdictMismatch, wantRow: "matched", wantVersion: "mismatch",
			category: b11aVersionState, section: "record",
		},
		{
			name: "a legacy record the write fence correctly left unversioned",
			setup: func(env *fakeEnv) {
				env.versioned = b11aFixed(env, State{Version: &versions.Reading{Legacy: true}})
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: b11aLegacy,
		},
		{
			name: "versioning was refused, so the read did not run",
			setup: func(env *fakeEnv) {
				env.versioned = b11aFixed(env, State{NotRun: "enable-refused"})
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: b11aRefused,
		},
		{
			name: "the oracle has no row for the issue",
			setup: func(env *fakeEnv) {
				env.plan = &translate.StepPlan{Actions: []translate.Action{action(translate.KindDelete, "x-1", "delete", "x-1")}}
				env.oracle["x-1"], env.candidate["x-1"] = nil, nil
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: b11aNoRow,
		},
		{
			name: "an ephemeral issue is never versioned",
			setup: func(env *fakeEnv) {
				env.oracle["x-1"] = b11aView("x-1", "T", txt("ephemeral", "1"))
				env.candidate["x-1"] = b11aView("x-1", "T", txt("ephemeral", "1"))
				// A document that disagrees with the rows would be found, were it compared.
				env.versioned = b11aStored(env, func(i *types.Issue) { i.Title = "T-stored" })
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: b11aWisp,
		},
		{
			name: "a no-history issue is never versioned",
			setup: func(env *fakeEnv) {
				env.oracle["x-1"] = b11aView("x-1", "T", txt("no_history", "1"))
				env.candidate["x-1"] = b11aView("x-1", "T", txt("no_history", "1"))
				env.versioned = b11aStored(env, func(i *types.Issue) { i.Title = "T-stored" })
			},
			wantStep: VerdictMatched, wantRow: "matched", wantVersion: b11aWisp,
		},
		{
			name: "the stored version holds a number the gate refuses",
			setup: func(env *fakeEnv) {
				env.versioned = b11aStored(env, storedBig)
			},
			wantStep: VerdictUncomparable, wantRow: "matched", wantVersion: "uncomparable",
			category: compare.CategoryNumberFidelity,
		},
		{
			name: "the rows differ and the stored version holds a number the gate refuses",
			setup: func(env *fakeEnv) {
				otherRow(env)
				env.versioned = b11aStored(env, storedBig)
			},
			wantStep: VerdictMismatch, wantRow: "mismatch", wantVersion: "uncomparable", rowMismatch: true,
			category: compare.CategoryNumberFidelity,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := b11aEnv()
			if tc.setup != nil {
				tc.setup(env)
			}
			got := b11aReplay(t, env)

			if got.Result.Verdict != tc.wantStep {
				t.Errorf("step verdict = %q, want %q", got.Result.Verdict, tc.wantStep)
			}
			if got.Result.Matched != (tc.wantStep == VerdictMatched) {
				t.Errorf("Matched = %v for step verdict %q", got.Result.Matched, got.Result.Verdict)
			}
			if string(got.Result.RowVerdict) != tc.wantRow {
				t.Errorf("row axis = %q, want %q", got.Result.RowVerdict, tc.wantRow)
			}
			if string(got.Result.VersionVerdict) != tc.wantVersion {
				t.Errorf("version axis = %q, want %q", got.Result.VersionVerdict, tc.wantVersion)
			}
			if tc.rowMismatch != (got.Mismatch != nil) {
				t.Errorf("row mismatch = %+v, want one: %v", got.Mismatch, tc.rowMismatch)
			}
			switch vm := got.VersionMismatch; {
			case tc.category == "" && vm != nil:
				t.Errorf("a version axis of %q stored a mismatch: %+v", tc.wantVersion, vm)
			case tc.category != "" && vm == nil:
				t.Errorf("no version mismatch stored, want category %q", tc.category)
			case vm != nil:
				if vm.Category != tc.category {
					t.Errorf("version mismatch category = %q, want %q", vm.Category, tc.category)
				}
				if vm.Section != tc.section {
					t.Errorf("version mismatch section = %q, want %q", vm.Section, tc.section)
				}
			}
		})
	}
}

// A step whose environment offers no versioned reader reports the row axis alone:
// the axis fields stay empty, so a row says only what was read.
func TestB11aNoVersionedReaderReportsTheRowAxisAlone(t *testing.T) {
	env := b11aEnv()
	env.versioned = nil
	got := b11aReplay(t, env)
	if got.Result.Verdict != VerdictMatched {
		t.Errorf("step verdict = %q, want matched", got.Result.Verdict)
	}
	if got.Result.RowVerdict != "" || got.Result.VersionVerdict != "" {
		t.Errorf("axes = %q and %q, want neither set when only the row reader ran", got.Result.RowVerdict, got.Result.VersionVerdict)
	}
	if got.VersionMismatch != nil {
		t.Errorf("a version mismatch with no versioned reader: %+v", got.VersionMismatch)
	}
}

// The readers are asked in the order the seam names, issue then ref, and with
// the work clone's data directory; every read of the work clone follows the last
// write to it, at the head the step left, exactly as the row reads always did.
func TestB11aReadersGetTheArgumentsTheSeamNames(t *testing.T) {
	env := b11aEnv()
	env.plan = &translate.StepPlan{Actions: []translate.Action{
		action(translate.KindUpdate, "x-1", "update", "x-1"),
		action(translate.KindUpdate, "x-1", "update", "x-1", "--title", "T"),
	}}
	b11aReplay(t, env)

	reads := env.callsWithPrefix("versioned ")
	if len(reads) != 1 || env.calls[reads[0]] != "versioned workdata workhead x-1" {
		t.Fatalf("stored-version reads at calls %v of %q, want one: the data directory, the head the step left, the issue", reads, env.calls)
	}
	if !reflect.DeepEqual(env.readDirs, []string{"workdata"}) {
		t.Errorf("the row reader was asked to look in %q, want the work clone's data directory", env.readDirs)
	}
	execs := env.callsWithPrefix("exec ")
	if len(execs) != 2 {
		t.Fatalf("calls = %q, want two executions", env.calls)
	}
	for _, i := range append(reads, env.callsWithPrefix("candidate ")...) {
		if i < execs[len(execs)-1] {
			t.Errorf("the work clone was read before the last write: %q", env.calls)
		}
	}
	for _, i := range reads {
		if head := env.callsWithPrefix("head"); len(head) != 1 || head[0] > i {
			t.Errorf("a stored version was read before the work head was: %q", env.calls)
		}
	}
}

// A reader that cannot read is a failed step, never a verdict about the build.
func TestB11aReaderFailureFailsTheStep(t *testing.T) {
	env := b11aEnv()
	env.versioned = b11aReader{env: env, name: "versioned", read: func(_, _, _ string) (State, error) {
		return State{}, errors.New("dolt is gone")
	}}
	_, err := replayStep(context.Background(), env, quarantined{}, stepFromTo("from", "to"))
	if err == nil || !strings.Contains(err.Error(), "dolt is gone") {
		t.Fatalf("err = %v, want the reader's failure", err)
	}
}

// B11a.QuarantineSkipsOracleEquality: an issue the replay stopped following is not
// held to the oracle on the version axis either. Its stored versions are not what
// the oracle's rows imply, because the change that quarantined it never
// happened, so finding them different would be a finding about the harness. Later
// steps record it skipped, read nothing for it, and compare nothing; the issues
// around it are still compared on both axes.
func TestB11aQuarantineSkipsOracleEquality(t *testing.T) {
	causes := []struct {
		name  string
		first func(env *fakeEnv)
	}{
		{"untranslatable", func(env *fakeEnv) {
			env.plan = &translate.StepPlan{
				Actions:        []translate.Action{action(translate.KindUpdate, "y-1", "update", "y-1")},
				Untranslatable: []*translate.Untranslatable{{Issue: "x-1", Columns: []string{"owner"}, Reasons: []string{"owner has no bd form"}}},
			}
		}},
		{"rejected", func(env *fakeEnv) {
			env.plan = &translate.StepPlan{Actions: []translate.Action{
				action(translate.KindUpdate, "x-1", "update", "x-1"),
				action(translate.KindUpdate, "y-1", "update", "y-1"),
			}}
			env.execErr = func(a translate.Action) error {
				if a.Issue == "x-1" {
					return &translate.ExecError{Argv: a.Argv, ExitCode: 1, Output: "refusing"}
				}
				return nil
			}
		}},
	}
	for _, tc := range causes {
		t.Run(tc.name, func(t *testing.T) {
			env := &fakeEnv{
				oracle:    map[string]*oracle.View{"x-1": b11aView("x-1", "T"), "y-1": b11aView("y-1", "T")},
				candidate: map[string]*oracle.View{"x-1": b11aView("x-1", "T"), "y-1": b11aView("y-1", "T")},
				head:      "workhead",
			}
			// The trap: x-1's stored version differs from its rows, so a comparison
			// that ran for it would find the difference.
			env.versioned = b11aByIssue(env, b11aStored(env, nil), map[string]CandidateReader{
				"x-1": b11aStored(env, func(i *types.Issue) { i.Title = "trap" }),
			})
			q := quarantined{}
			ctx := context.Background()

			tc.first(env)
			one, err := replayStep(ctx, env, q, stepFromTo("c0", "c1"))
			if err != nil {
				t.Fatalf("the first step: %v", err)
			}
			if q["x-1"] != "c1" {
				t.Fatalf("quarantine = %v, want x-1 quarantined by the first step", q)
			}
			env.plan = &translate.StepPlan{Actions: []translate.Action{
				action(translate.KindUpdate, "x-1", "update", "x-1"),
				action(translate.KindUpdate, "y-1", "update", "y-1"),
			}}
			env.execErr = nil
			two, err := replayStep(ctx, env, q, stepFromTo("c1", "c2"))
			if err != nil {
				t.Fatalf("the second step: %v", err)
			}

			if got := stepResult(t, two, "x-1").Result.Verdict; got != VerdictSkippedQuarantined {
				t.Errorf("x-1 in the second step = %q, want skipped-quarantined", got)
			}
			for step, out := range []*stepOutcome{one, two} {
				x := stepResult(t, out, "x-1")
				switch x.Result.VersionVerdict {
				case "matched", "mismatch", "uncomparable":
					t.Errorf("step %d: quarantined x-1 has version axis %q, want it neither compared nor passed", step+1, x.Result.VersionVerdict)
				}
				if x.VersionMismatch != nil || x.Mismatch != nil {
					t.Errorf("step %d: quarantined x-1 has mismatches: row %+v, version %+v", step+1, x.Mismatch, x.VersionMismatch)
				}
				y := stepResult(t, out, "y-1")
				if y.Result.Verdict != VerdictMatched || y.Result.RowVerdict != "matched" || y.Result.VersionVerdict != "matched" {
					t.Errorf("step %d: y-1 = %q, row %q, version %q, want it compared on both axes and matched",
						step+1, y.Result.Verdict, y.Result.RowVerdict, y.Result.VersionVerdict)
				}
			}
			for _, c := range env.calls {
				for _, reader := range []string{"candidate ", "versioned "} {
					if strings.HasPrefix(c, reader) && strings.HasSuffix(c, " x-1") {
						t.Errorf("the work clone was read for a quarantined issue: %q", c)
					}
				}
			}
		})
	}
}
