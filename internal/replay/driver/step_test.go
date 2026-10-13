package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
)

// ---- a step environment with nothing behind it ------------------------------
//
// The step logic is exercised without dolt or bd: the plan, the two sides' views
// and the bd invocations are all the test's own, and every call is logged in
// order so a test can say what happened before what.

type fakeEnv struct {
	plan      *translate.StepPlan
	oracle    map[string]*oracle.View
	candidate map[string]*oracle.View
	// versioned is the work clone's second reader, the one over the versions the
	// product stored. A step with none reports the row axis alone.
	versioned CandidateReader
	head      string
	oracleErr error
	execErr   func(translate.Action) error
	calls     []string
	// readDirs is the directory each row read was asked to look in.
	readDirs []string
}

func (f *fakeEnv) log(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeEnv) Plan(_ context.Context, from, to string) (*translate.StepPlan, error) {
	f.log("plan %s %s", from, to)
	return f.plan, nil
}

func (f *fakeEnv) OracleView(_ context.Context, ref, issue string) (*oracle.View, error) {
	f.log("oracle %s %s", ref, issue)
	if f.oracleErr != nil {
		return nil, f.oracleErr
	}
	return f.oracle[issue], nil
}

func (f *fakeEnv) Execute(_ context.Context, a translate.Action) error {
	f.log("exec %s %s", a.Issue, strings.Join(a.Argv, " "))
	if f.execErr != nil {
		return f.execErr(a)
	}
	return nil
}

func (f *fakeEnv) WorkHead(context.Context) (string, error) {
	f.log("head")
	return f.head, nil
}

func (f *fakeEnv) WorkDataDir() string { return "workdata" }

// Readers is the work clone's readers: the row reader, always, and the versioned
// one when a test supplies it.
func (f *fakeEnv) Readers() []CandidateReader {
	readers := []CandidateReader{fakeRowReader{f}}
	if f.versioned != nil {
		readers = append(readers, f.versioned)
	}
	return readers
}

// fakeRowReader answers a row read from the fake's candidate views and logs it as
// "candidate <ref> <issue>", the order the isolation test reads.
type fakeRowReader struct{ env *fakeEnv }

func (fakeRowReader) Name() string { return "row" }

func (r fakeRowReader) Read(_ context.Context, dir, issueID, ref string) (State, error) {
	r.env.log("candidate %s %s", ref, issueID)
	r.env.readDirs = append(r.env.readDirs, dir)
	return State{View: r.env.candidate[issueID]}, nil
}

// callsWithPrefix returns the indexes of the logged calls that start with prefix.
func (f *fakeEnv) callsWithPrefix(prefix string) []int {
	var idx []int
	for i, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			idx = append(idx, i)
		}
	}
	return idx
}

// col is one column of a hand-built issue row; a null column is SQL NULL.
type col struct {
	name string
	null bool
	text string
}

func txt(name, text string) col { return col{name: name, text: text} }
func nul(name string) col       { return col{name: name, null: true} }

// viewOf builds the view of one issue from column values.
func viewOf(id string, cols ...col) *oracle.View {
	row := oracle.Row{Columns: []string{"id"}, Cells: []doltcli.Cell{{Text: id}}}
	for _, c := range cols {
		row.Columns = append(row.Columns, c.name)
		row.Cells = append(row.Cells, doltcli.Cell{Null: c.null, Text: c.text})
	}
	return &oracle.View{Issue: row}
}

func action(kind translate.ActionKind, issue string, argv ...string) translate.Action {
	return translate.Action{Kind: kind, Issue: issue, Argv: argv}
}

func stepFromTo(from, to string) Step {
	return Step{From: Commit{Hash: from}, To: Commit{Hash: to}}
}

func stepResult(t *testing.T, out *stepOutcome, issue string) issueOutcome {
	t.Helper()
	for _, io := range out.Issues {
		if io.Result.IssueID == issue {
			return io
		}
	}
	t.Fatalf("no row for issue %s among %d", issue, len(out.Issues))
	return issueOutcome{}
}

// ---- the order the AF1 isolation check fixes ---------------------------------

// The oracle's views are read for every issue before the first write to the
// work clone, and the work clone's are read only after the last one: the oracle
// can neither be affected by nor blamed on what the replay did.
func TestB6OracleViewsReadBeforeAnyWrite(t *testing.T) {
	env := &fakeEnv{
		plan: &translate.StepPlan{Actions: []translate.Action{
			action(translate.KindCreate, "x-1", "create", "--id", "x-1"),
			action(translate.KindCreate, "y-1", "create", "--id", "y-1"),
			action(translate.KindUpdate, "x-1", "update", "x-1"),
		}},
		oracle:    map[string]*oracle.View{"x-1": viewOf("x-1"), "y-1": viewOf("y-1")},
		candidate: map[string]*oracle.View{"x-1": viewOf("x-1"), "y-1": viewOf("y-1")},
		head:      "workhead",
	}
	if _, err := replayStep(context.Background(), env, quarantined{}, stepFromTo("from", "to")); err != nil {
		t.Fatalf("replayStep: %v", err)
	}
	oracleReads, execs := env.callsWithPrefix("oracle "), env.callsWithPrefix("exec ")
	if len(oracleReads) != 2 || len(execs) != 3 {
		t.Fatalf("calls = %v, want 2 oracle reads and 3 executions", env.calls)
	}
	if oracleReads[len(oracleReads)-1] > execs[0] {
		t.Errorf("an oracle view was read after the first write: %v", env.calls)
	}
	if head := env.callsWithPrefix("head"); len(head) != 1 || head[0] < execs[len(execs)-1] {
		t.Errorf("the work head was read before the last write: %v", env.calls)
	}
	for _, i := range env.callsWithPrefix("candidate ") {
		if i < execs[len(execs)-1] {
			t.Errorf("a candidate view was read before the last write: %v", env.calls)
		}
	}
	for _, c := range env.calls {
		if strings.HasPrefix(c, "oracle ") && !strings.HasPrefix(c, "oracle to ") {
			t.Errorf("the oracle was read at %q, want the step's own commit", c)
		}
		if strings.HasPrefix(c, "candidate ") && !strings.HasPrefix(c, "candidate workhead ") {
			t.Errorf("the work clone was read at %q, want its own head", c)
		}
	}
}

// A broken oracle read stops the step before anything is replayed, so it can
// never be masked by, or blamed on, a replay side effect.
func TestB6OracleReadFailureRunsNothing(t *testing.T) {
	env := &fakeEnv{
		plan:      &translate.StepPlan{Actions: []translate.Action{action(translate.KindCreate, "x-1", "create")}},
		oracleErr: errors.New("oracle is gone"),
	}
	_, err := replayStep(context.Background(), env, quarantined{}, stepFromTo("from", "to"))
	if err == nil || !strings.Contains(err.Error(), "oracle is gone") {
		t.Fatalf("err = %v, want the oracle's failure", err)
	}
	if n := len(env.callsWithPrefix("exec ")); n != 0 {
		t.Errorf("%d bd invocations ran after a failed oracle read", n)
	}
}

// ---- verdicts ---------------------------------------------------------------

// B6.ExistenceVerdicts, B6.DriverNullVsEmpty and the rest of the verdict table,
// decided by the driver on the views it reads. The driver reports whatever
// category CompareViews returns and adds none of its own.
func TestB6Verdicts(t *testing.T) {
	bigA := txt("metadata", `{"n":9007199254740993}`)
	bigB := txt("metadata", `{"n":9007199254740992}`)

	cases := []struct {
		name      string
		oracle    *oracle.View
		candidate *oracle.View
		actions   []translate.Action
		want      Verdict
		category  string // the stored mismatch category, "" when no mismatch row is stored
	}{
		{"equal", viewOf("x-1", txt("title", "T")), viewOf("x-1", txt("title", "T")), nil, VerdictMatched, ""},
		{"different title", viewOf("x-1", txt("title", "T")), viewOf("x-1", txt("title", "U")), nil, VerdictMismatch, "unclassified"},
		{"NULL against the empty string is a mismatch", viewOf("x-1", nul("spec_id")), viewOf("x-1", txt("spec_id", "")), nil, VerdictMismatch, "unclassified"},
		{"the empty string against NULL is a mismatch", viewOf("x-1", txt("spec_id", "")), viewOf("x-1", nul("spec_id")), nil, VerdictMismatch, "unclassified"},
		{"issue the candidate lacks", viewOf("x-1", txt("title", "T")), nil, nil, VerdictMismatch, compare.CategoryIssueMissing},
		{"issue the candidate should not have", nil, viewOf("x-1", txt("title", "T")),
			[]translate.Action{action(translate.KindDelete, "x-1", "delete", "x-1")}, VerdictMismatch, compare.CategoryIssueExtra},
		{"a delete both sides performed", nil, nil,
			[]translate.Action{action(translate.KindDelete, "x-1", "delete", "x-1")}, VerdictMatched, ""},
		{"a number the gate refuses", viewOf("x-1", bigA), viewOf("x-1", bigB), nil, VerdictUncomparable, compare.CategoryNumberFidelity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actions := tc.actions
			if actions == nil {
				actions = []translate.Action{action(translate.KindUpdate, "x-1", "update", "x-1")}
			}
			env := &fakeEnv{
				plan:      &translate.StepPlan{Actions: actions},
				oracle:    map[string]*oracle.View{"x-1": tc.oracle},
				candidate: map[string]*oracle.View{"x-1": tc.candidate},
				head:      "workhead",
			}
			out, err := replayStep(context.Background(), env, quarantined{}, stepFromTo("from", "to"))
			if err != nil {
				t.Fatalf("replayStep: %v", err)
			}
			got := stepResult(t, out, "x-1")
			if got.Result.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", got.Result.Verdict, tc.want)
			}
			if got.Result.Matched != (tc.want == VerdictMatched) {
				t.Errorf("Matched = %v for verdict %q", got.Result.Matched, got.Result.Verdict)
			}
			switch {
			case tc.category == "" && got.Mismatch != nil:
				t.Errorf("a %q row carries a mismatch: %+v", tc.want, got.Mismatch)
			case tc.category != "" && (got.Mismatch == nil || got.Mismatch.Category != tc.category):
				t.Errorf("mismatch = %+v, want category %q", got.Mismatch, tc.category)
			}
			if tc.want == VerdictMismatch && (got.Result.OracleHash == "" || got.Result.OracleHash == got.Result.CandidateHash) {
				t.Errorf("a mismatch must carry two different hashes: %q and %q", got.Result.OracleHash, got.Result.CandidateHash)
			}
			// An existence or a column difference is never a refused number.
			if tc.want != VerdictUncomparable && got.Result.Verdict == VerdictUncomparable {
				t.Errorf("verdict %q was read as uncomparable", tc.want)
			}
		})
	}
}

// Untranslatable: nothing for the issue runs, the row says why, and the issue is
// quarantined. The step's other issues are unaffected.
func TestB6UntranslatableIsQuarantined(t *testing.T) {
	env := &fakeEnv{
		plan: &translate.StepPlan{
			Actions:        []translate.Action{action(translate.KindUpdate, "y-1", "update", "y-1")},
			Untranslatable: []*translate.Untranslatable{{Issue: "x-1", Columns: []string{"owner"}, Reasons: []string{"owner has no bd form"}}},
		},
		oracle:    map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		candidate: map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		head:      "workhead",
	}
	q := quarantined{}
	out, err := replayStep(context.Background(), env, q, stepFromTo("from", "to"))
	if err != nil {
		t.Fatalf("replayStep: %v", err)
	}
	x := stepResult(t, out, "x-1")
	if x.Result.Verdict != VerdictUntranslatable || x.Result.Matched {
		t.Errorf("x-1 verdict = %q matched=%v, want untranslatable", x.Result.Verdict, x.Result.Matched)
	}
	if x.Result.Detail == nil || !reflect.DeepEqual(x.Result.Detail.Columns, []string{"owner"}) || len(x.Result.Detail.Reasons) == 0 {
		t.Errorf("x-1 detail = %+v, want the owner column and a reason", x.Result.Detail)
	}
	if got := stepResult(t, out, "y-1").Result.Verdict; got != VerdictMatched {
		t.Errorf("y-1 verdict = %q, want matched: an untranslatable neighbour changes nothing", got)
	}
	if q["x-1"] != "to" {
		t.Errorf("quarantine = %v, want x-1 quarantined at the step's commit", q)
	}
	for _, c := range env.callsWithPrefix("exec ") {
		if strings.Contains(env.calls[c], "x-1") {
			t.Errorf("a bd invocation ran for the untranslatable issue: %s", env.calls[c])
		}
	}
	for _, c := range env.callsWithPrefix("oracle ") {
		if strings.HasSuffix(env.calls[c], " x-1") {
			t.Errorf("the oracle was read for an issue that is not compared: %s", env.calls[c])
		}
	}
}

// B6.Quarantine: a quarantined issue is recorded skipped-quarantined in every
// later step that touches it, with nothing replayed and nothing compared, and
// the run goes on with everything else.
func TestB6QuarantineSkipsLaterSteps(t *testing.T) {
	q := quarantined{"x-1": "commit-one"}
	env := &fakeEnv{
		plan: &translate.StepPlan{Actions: []translate.Action{
			action(translate.KindUpdate, "x-1", "update", "x-1"),
			action(translate.KindUpdate, "y-1", "update", "y-1"),
		}},
		oracle:    map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		candidate: map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		head:      "workhead",
	}
	out, err := replayStep(context.Background(), env, q, stepFromTo("commit-one", "commit-two"))
	if err != nil {
		t.Fatalf("replayStep: %v", err)
	}
	x := stepResult(t, out, "x-1")
	if x.Result.Verdict != VerdictSkippedQuarantined || x.Result.Matched {
		t.Errorf("x-1 verdict = %q, want skipped-quarantined", x.Result.Verdict)
	}
	if x.Result.Detail == nil || x.Result.Detail.QuarantinedAt != "commit-one" {
		t.Errorf("x-1 detail = %+v, want the commit that quarantined it", x.Result.Detail)
	}
	if x.Result.MutationKind != "update" {
		t.Errorf("x-1 mutation kind = %q, want the planned one, update", x.Result.MutationKind)
	}
	if got := stepResult(t, out, "y-1").Result.Verdict; got != VerdictMatched {
		t.Errorf("y-1 verdict = %q, want matched", got)
	}
	for _, i := range env.callsWithPrefix("exec ") {
		if strings.Contains(env.calls[i], "x-1") {
			t.Errorf("a quarantined issue was replayed: %s", env.calls[i])
		}
	}
	if q["x-1"] != "commit-one" {
		t.Errorf("a skipped step moved the quarantine to %q; it stays at the commit that caused it", q["x-1"])
	}
}

// B6.Rejected: a bd action that exits non-zero is a finding, not a failure. The
// issue is rejected and quarantined, none of its later actions in the step run,
// and the other issues carry on.
func TestB6RejectedStopsThatIssuesActions(t *testing.T) {
	env := &fakeEnv{
		plan: &translate.StepPlan{Actions: []translate.Action{
			action(translate.KindCreate, "x-1", "create", "x-1"),
			action(translate.KindCreate, "y-1", "create", "y-1"),
			action(translate.KindClose, "x-1", "close", "x-1"),
			action(translate.KindDepAdd, "x-1", "dep", "add", "x-1", "y-1"),
			action(translate.KindUpdate, "y-1", "update", "y-1"),
		}},
		oracle:    map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		candidate: map[string]*oracle.View{"y-1": viewOf("y-1", txt("title", "T"))},
		head:      "workhead",
		execErr: func(a translate.Action) error {
			if a.Kind == translate.KindClose {
				return &translate.ExecError{Argv: a.Argv, ExitCode: 1, Output: "refusing to close"}
			}
			return nil
		},
	}
	q := quarantined{}
	out, err := replayStep(context.Background(), env, q, stepFromTo("from", "to"))
	if err != nil {
		t.Fatalf("replayStep: %v", err)
	}
	x := stepResult(t, out, "x-1")
	if x.Result.Verdict != VerdictRejected || x.Result.Matched {
		t.Fatalf("x-1 verdict = %q, want rejected", x.Result.Verdict)
	}
	d := x.Result.Detail
	if d == nil || d.ExitCode != 1 || !strings.Contains(d.Output, "refusing to close") || !reflect.DeepEqual(d.Argv, []string{"close", "x-1"}) {
		t.Errorf("x-1 detail = %+v, want the argv, exit code 1 and bd's output", d)
	}
	if q["x-1"] != "to" {
		t.Errorf("quarantine = %v, want x-1 quarantined at the step's commit", q)
	}
	var ran []string
	for _, i := range env.callsWithPrefix("exec ") {
		ran = append(ran, env.calls[i])
	}
	want := []string{"exec x-1 create x-1", "exec y-1 create y-1", "exec x-1 close x-1", "exec y-1 update y-1"}
	if !reflect.DeepEqual(ran, want) {
		t.Errorf("executed %q\nwant      %q: the dep add after the rejected close must not run, and y-1 must", ran, want)
	}
	if got := stepResult(t, out, "y-1").Result.Verdict; got != VerdictMatched {
		t.Errorf("y-1 verdict = %q, want matched", got)
	}
}

// A process that cannot start or is killed is not a rejection: the run fails.
func TestB6InfrastructureFailureFailsTheStep(t *testing.T) {
	for name, execErr := range map[string]error{
		"a plain failure":       errors.New("fork/exec: no such file or directory"),
		"a signal-killed child": fmt.Errorf("bd create: %w", errors.New("signal: killed")),
	} {
		t.Run(name, func(t *testing.T) {
			env := &fakeEnv{
				plan:   &translate.StepPlan{Actions: []translate.Action{action(translate.KindCreate, "x-1", "create")}},
				oracle: map[string]*oracle.View{"x-1": viewOf("x-1")},
				execErr: func(translate.Action) error {
					return execErr
				},
			}
			q := quarantined{}
			if _, err := replayStep(context.Background(), env, q, stepFromTo("from", "to")); err == nil {
				t.Fatal("the step succeeded although bd could not run")
			}
			if len(q) != 0 {
				t.Errorf("an infrastructure failure quarantined %v; only a finding does", q)
			}
		})
	}
}

// ---- mutation kind ----------------------------------------------------------

// B6.MergeStep, in part: the kind is the sorted, de-duplicated action kinds
// joined by +; a merge step prefixes merge:; a step that spans commits is net.
func TestB6MutationKind(t *testing.T) {
	create := action(translate.KindCreate, "x-1", "create")
	dep := action(translate.KindDepAdd, "x-1", "dep", "add")
	upd := action(translate.KindUpdate, "x-1", "update")

	cases := []struct {
		name    string
		step    Step
		actions []translate.Action
		want    string
	}{
		{"one kind", Step{}, []translate.Action{upd}, "update"},
		{"sorted and joined", Step{}, []translate.Action{upd, dep, create}, "create+dep_add+update"},
		{"repeated kinds count once", Step{}, []translate.Action{create, dep, dep}, "create+dep_add"},
		{"merge prefix", Step{Merge: true}, []translate.Action{create, dep}, "merge:create+dep_add"},
		{"merge with nothing replayed", Step{Merge: true}, nil, "merge"},
		{"net wins over everything", Step{Net: true, Merge: true}, []translate.Action{create}, "net"},
		{"net", Step{Net: true}, []translate.Action{create, upd}, "net"},
		{"nothing replayed", Step{}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mutationKind(tc.step, tc.actions); got != tc.want {
				t.Errorf("mutationKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- the step loop: rows, the Observer and the summary -----------------------

type recorder struct {
	events []Event
	err    error
}

func (r *recorder) Observe(_ context.Context, ev Event) error {
	r.events = append(r.events, ev)
	return r.err
}

func twoStepEnv() (*fakeEnv, []Step) {
	env := &fakeEnv{
		plan: &translate.StepPlan{
			Actions: []translate.Action{
				action(translate.KindCreate, "x-1", "create", "x-1"),
				action(translate.KindCreate, "y-1", "create", "y-1"),
			},
			Gaps:    []translate.CoverageGap{{Table: "labels", Reason: "not replayed yet"}, {Table: "mystery", Unknown: true, Reason: "not in the policy"}},
			Derived: []string{"events"},
		},
		oracle:    map[string]*oracle.View{"x-1": viewOf("x-1", txt("title", "T")), "y-1": viewOf("y-1", txt("title", "T"))},
		candidate: map[string]*oracle.View{"x-1": viewOf("x-1", txt("title", "T")), "y-1": viewOf("y-1", txt("title", "OTHER"))},
		head:      "workhead",
	}
	steps := []Step{
		{Index: 0, From: Commit{Hash: "c0"}, To: Commit{Hash: "c1"}},
		{Index: 1, From: Commit{Hash: "c1"}, To: Commit{Hash: "c2", Parents: []string{"c1", "s1"}}, Merge: true},
	}
	return env, steps
}

// B6.ObserverEvents: one event per (step, issue), in step order then issue
// order, each carrying the row that was written; a nil Observer is valid; an
// Observer that returns an error fails the run.
func TestB6ObserverEvents(t *testing.T) {
	t.Run("one event per step and issue", func(t *testing.T) {
		env, steps := twoStepEnv()
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		rec := &recorder{}
		r := newRunner("run-1", store, rec)
		if err := r.runSteps(context.Background(), env, steps); err != nil {
			t.Fatalf("runSteps: %v", err)
		}
		if len(rec.events) != 4 {
			t.Fatalf("got %d events, want 4 (2 steps x 2 issues): %+v", len(rec.events), rec.events)
		}
		var rowKeys []string
		for i, ev := range rec.events {
			got, ok := ev.(StepIssueResult)
			if !ok {
				t.Fatalf("event %d is %T, want StepIssueResult", i, ev)
			}
			wantStep, wantIssue := i/2, []string{"x-1", "y-1"}[i%2]
			if got.Step != wantStep || got.Result.IssueID != wantIssue {
				t.Errorf("event %d is step %d issue %s, want step %d issue %s", i, got.Step, got.Result.IssueID, wantStep, wantIssue)
			}
			if got.Result.RunID != "run-1" {
				t.Errorf("event %d row has run id %q", i, got.Result.RunID)
			}
			if got.Merge != (wantStep == 1) {
				t.Errorf("event %d Merge = %v", i, got.Merge)
			}
			rowKeys = append(rowKeys, got.Result.SourceCommit+"/"+got.Result.IssueID+"/"+string(got.Result.Verdict))
		}
		rows := readJSONLTyped[CommitReplayResult](t, filepath.Join(store.dir, "commit_replay_results.jsonl"))
		var written []string
		for _, row := range rows {
			written = append(written, row.SourceCommit+"/"+row.IssueID+"/"+string(row.Verdict))
		}
		if !reflect.DeepEqual(written, rowKeys) {
			t.Errorf("rows written %v\nevents saw   %v: they must be the same rows", written, rowKeys)
		}
	})

	t.Run("a nil Observer is valid", func(t *testing.T) {
		env, steps := twoStepEnv()
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := newRunner("run-1", store, nil).runSteps(context.Background(), env, steps); err != nil {
			t.Fatalf("runSteps with no Observer: %v", err)
		}
	})

	t.Run("an Observer error fails the run", func(t *testing.T) {
		env, steps := twoStepEnv()
		store, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		boom := errors.New("observer says stop")
		rec := &recorder{err: boom}
		err = newRunner("run-1", store, rec).runSteps(context.Background(), env, steps)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the Observer's error", err)
		}
		if len(rec.events) != 1 {
			t.Errorf("the run went on after the Observer failed: %d events", len(rec.events))
		}
	})
}

// B6.SeedEventTypes: the Observer vocabulary declares the two seed events with
// the specified field set, and nothing in the loop emits them.
func TestB6SeedEventTypes(t *testing.T) {
	var _ Event = SeedMigrating{}
	var _ Event = SeedCompleted{}
	var _ Event = StepIssueResult{}

	fields := func(v any) []string {
		raw, err := jsonKeys(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	want := []string{
		"backups_stripped", "dolt_cli_version", "ignored_rows_cleared", "ignored_schema_after",
		"ignored_schema_before", "linked_engine", "migration_commits", "migration_seconds",
		"remotes_stripped", "schema_after", "schema_before", "seed_head",
	}
	if got := fields(SeedCompleted{}); !reflect.DeepEqual(got, want) {
		t.Errorf("SeedCompleted fields = %v\nwant %v", got, want)
	}
	if got := fields(SeedRecord{}); !reflect.DeepEqual(got, want) {
		t.Errorf("SeedRecord fields = %v\nwant %v", got, want)
	}

	env, steps := twoStepEnv()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	if err := newRunner("run-1", store, rec).runSteps(context.Background(), env, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	for _, ev := range rec.events {
		switch ev.(type) {
		case SeedMigrating, SeedCompleted:
			t.Errorf("the loop emitted %T; only a seeding step may", ev)
		}
	}
}

// B6, summary: verdict counts (all six named, zeros included), coverage gaps by
// table and by column, derived tables counted, the schema skew of the run, the
// number-fidelity count and the covered range.
func TestB6SummaryCounts(t *testing.T) {
	big := func(n string) col { return txt("metadata", `{"n":`+n+`}`) }
	env := &fakeEnv{
		plan: &translate.StepPlan{
			Actions: []translate.Action{
				action(translate.KindUpdate, "x-1", "update", "x-1"),
				action(translate.KindUpdate, "y-1", "update", "y-1"),
				action(translate.KindUpdate, "z-1", "update", "z-1"),
			},
			Untranslatable: []*translate.Untranslatable{{Issue: "u-1", Columns: []string{"owner", "compaction_level"}, Reasons: []string{"no bd form"}}},
			Gaps:           []translate.CoverageGap{{Table: "labels", Reason: "not replayed"}},
			Derived:        []string{"events", "child_counters"},
		},
		oracle: map[string]*oracle.View{
			"x-1": viewOf("x-1", txt("title", "T")),
			"y-1": viewOf("y-1", txt("title", "T")),
			"z-1": viewOf("z-1", big("9007199254740993")),
		},
		candidate: map[string]*oracle.View{
			"x-1": viewOf("x-1", txt("title", "T")),
			"y-1": viewOf("y-1", txt("title", "DIFFERENT")),
			"z-1": viewOf("z-1", big("9007199254740992")),
		},
		head: "workhead",
	}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner("run-1", store, nil)
	steps := []Step{{Index: 0, From: Commit{Hash: "c0"}, To: Commit{Hash: "c1"}}}
	if err := r.runSteps(context.Background(), env, steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}

	walk := newWalk([]Commit{
		{Hash: "c0", Message: "bd init", Date: "2026-01-01 00:00:00.000"},
		{Hash: "c1", Parents: []string{"c0"}, Message: "bd: update", Date: "2026-01-02 00:00:00.000"},
	}, 7)
	sum := r.summarize(ReplayRun{ID: "run-1", Status: "completed", Mode: "exhaustive"}, walk, steps, nil)

	wantVerdicts := map[string]int{
		"matched": 1, "mismatch": 1, "uncomparable": 1, "untranslatable": 1, "rejected": 0, "skipped-quarantined": 0,
	}
	if !reflect.DeepEqual(sum.Verdicts, wantVerdicts) {
		t.Errorf("verdicts = %v, want %v", sum.Verdicts, wantVerdicts)
	}
	if sum.NumberFidelity != 1 {
		t.Errorf("number_fidelity = %d, want 1", sum.NumberFidelity)
	}
	if !reflect.DeepEqual(sum.Gaps.Tables, map[string]int{"labels": 1}) {
		t.Errorf("gaps by table = %v, want labels once", sum.Gaps.Tables)
	}
	if !reflect.DeepEqual(sum.Gaps.Columns, map[string]int{"issues.owner": 1, "issues.compaction_level": 1}) {
		t.Errorf("gaps by column = %v, want issues.owner and issues.compaction_level once each", sum.Gaps.Columns)
	}
	if !reflect.DeepEqual(sum.Derived, map[string]int{"events": 1, "child_counters": 1}) {
		t.Errorf("derived = %v, want each table counted once", sum.Derived)
	}
	if sum.SchemaSkew == nil {
		t.Error("schema_skew is nil: a run with no skew still says so with an empty object")
	}
	want := CoveredRange{
		BaseCommit: "c0", BaseDate: "2026-01-01 00:00:00.000", HeadCommit: "c1", HeadDate: "2026-01-02 00:00:00.000",
		CommitsTotal: 7, CommitsReplayed: 1,
	}
	if sum.CoveredRange != want {
		t.Errorf("covered_range = %+v, want %+v", sum.CoveredRange, want)
	}
	if sum.RunID != "run-1" || sum.Status != "completed" || sum.Mode != "exhaustive" {
		t.Errorf("summary header = %q %q %q", sum.RunID, sum.Status, sum.Mode)
	}

	// The gap rows are written per step and table, so the holes are visible one by one.
	gaps := readJSONLTyped[CoverageGapRow](t, filepath.Join(store.dir, "coverage_gaps.jsonl"))
	if len(gaps) != 1 || gaps[0].Table != "labels" || gaps[0].FromCommit != "c0" || gaps[0].SourceCommit != "c1" || gaps[0].RunID != "run-1" {
		t.Errorf("coverage gap rows = %+v, want one for labels at c0 to c1", gaps)
	}
}

// The summary is written as summary.json and read back as the same document.
func TestB6SummaryIsWrittenAsJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := Summary{
		RunID: "run-1", Status: "failed", Mode: "sampled", SampleSize: 3,
		Verdicts:   map[string]int{"matched": 2},
		SchemaSkew: compare.Skew{"issues": {OracleOnly: []string{"a_col"}}},
		Seed:       &SeedRecord{SeedHead: "abc", RemotesStripped: 3},
	}
	if err := store.WriteSummary(in); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("reading summary.json: %v", err)
	}
	var out Summary
	if err := unmarshalStrict(data, &out); err != nil {
		t.Fatalf("summary.json is not the summary: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("summary read back differs:\n got  %+v\n want %+v", out, in)
	}
}
