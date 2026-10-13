package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/replay/versions"
	"github.com/steveyegge/beads/internal/types"
)

// ---- a run over four issues, one for each thing the version axis can say ----------

// b11aSummaryRun is one step over x-1, y-1, z-1 and w-1 in a fake environment.
// x-1's stored version is the oracle's. y-1's differs in its title. z-1 is a legacy
// record, which the write fence leaves unversioned. w-1 is an ephemeral issue,
// which is never versioned. The work clone's rows equal the oracle's for all four.
type b11aSummaryRun struct {
	store *Store
	steps []Step
	walk  *Walk
	r     *runner
	sum   Summary
}

func (s *b11aSummaryRun) summarize() Summary {
	return s.r.summarize(ReplayRun{ID: "run-1", Status: "completed", Mode: "exhaustive"}, s.walk, s.steps, nil)
}

func newB11aSummaryRun(t *testing.T) *b11aSummaryRun {
	t.Helper()
	env := &fakeEnv{
		plan:      &translate.StepPlan{},
		oracle:    map[string]*oracle.View{},
		candidate: map[string]*oracle.View{},
		head:      "workhead",
	}
	for _, id := range []string{"x-1", "y-1", "z-1", "w-1"} {
		env.plan.Actions = append(env.plan.Actions, action(translate.KindUpdate, id, "update", id))
		for _, side := range []map[string]*oracle.View{env.oracle, env.candidate} {
			if id == "w-1" {
				side[id] = b11aView(id, "T", txt("ephemeral", "1"))
			} else {
				side[id] = b11aView(id, "T")
			}
		}
	}
	env.versioned = b11aByIssue(env, b11aStored(env, nil), map[string]CandidateReader{
		"y-1": b11aStored(env, func(i *types.Issue) { i.Title = "stored" }),
		"z-1": b11aFixed(env, State{Version: &versions.Reading{Legacy: true}}),
	})

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &b11aSummaryRun{
		store: store,
		steps: []Step{{Index: 0, From: Commit{Hash: "c0"}, To: Commit{Hash: "c1"}}},
		walk: newWalk([]Commit{
			{Hash: "c0", Message: "bd init", Date: "2026-01-01 00:00:00.000"},
			{Hash: "c1", Parents: []string{"c0"}, Message: "bd: update", Date: "2026-01-02 00:00:00.000"},
		}, 2),
		r: newRunner("run-1", store, nil),
	}
	if err := s.r.runSteps(context.Background(), env, s.steps); err != nil {
		t.Fatalf("runSteps: %v", err)
	}
	s.sum = s.summarize()
	return s
}

// b11aSummaryDoc is the summary as the JSON document a reader sees, so that a
// test pins the names in the document and not the ones in the code.
func b11aSummaryDoc(t *testing.T, sum Summary) map[string]any {
	t.Helper()
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal the summary: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the summary is not an object: %v\n%s", err, raw)
	}
	return doc
}

// b11aCounts reads a JSON object of integer counts.
func b11aCounts(t *testing.T, v any) map[string]int {
	t.Helper()
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object of counts", v)
	}
	out := make(map[string]int, len(obj))
	for k, n := range obj {
		f, ok := n.(float64)
		if !ok || f != float64(int(f)) {
			t.Fatalf("count %q = %v, want an integer", k, n)
		}
		out[k] = int(f)
	}
	return out
}

// B11a, summary: the versioned reads are counted on both axes, every value the
// version axis can take listed with a zero where it did not occur. A legacy record
// and an ephemeral issue are counted as what they are, not as passes.
func TestB11aSummaryCountsTheVersionedReads(t *testing.T) {
	doc := b11aSummaryDoc(t, newB11aSummaryRun(t).sum)
	reads, ok := doc["versioned_reads"].(map[string]any)
	if !ok {
		t.Fatalf("the summary has no versioned_reads: %v", doc)
	}
	wantRow := map[string]int{"matched": 4, "mismatch": 0, "uncomparable": 0}
	if got := b11aCounts(t, reads["row"]); !reflect.DeepEqual(got, wantRow) {
		t.Errorf("row reads = %v, want %v", got, wantRow)
	}
	wantVersion := map[string]int{
		"matched": 1, "mismatch": 1, "uncomparable": 0,
		b11aLegacy: 1, b11aRefused: 0, b11aNoRow: 0, b11aWisp: 1,
	}
	if got := b11aCounts(t, reads["version"]); !reflect.DeepEqual(got, wantVersion) {
		t.Errorf("version reads = %v, want %v", got, wantVersion)
	}
	fence, present := reads["write_fence"]
	if _, isBool := fence.(bool); !present || !isBool {
		t.Errorf("write_fence = %v (present=%v), want a boolean that is always there", fence, present)
	}
}

// A value only the version axis has stays on that axis: the step verdicts keep
// their six names, and no row's verdict is one of them.
func TestB11aAxisValuesStayOffTheVerdicts(t *testing.T) {
	s := newB11aSummaryRun(t)
	if len(s.sum.Verdicts) != 6 {
		t.Errorf("verdicts = %v, want the six names", s.sum.Verdicts)
	}
	for name := range s.sum.Verdicts {
		if strings.HasPrefix(name, "version-") {
			t.Errorf("verdicts names %q, a value only the version axis has", name)
		}
	}
	rows := readJSONLTyped[CommitReplayResult](t, filepath.Join(s.store.dir, "commit_replay_results.jsonl"))
	if len(rows) != 4 {
		t.Fatalf("%d result rows, want 4", len(rows))
	}
	known := map[Verdict]bool{}
	for _, v := range verdicts {
		known[v] = true
	}
	for _, row := range rows {
		if !known[row.Verdict] {
			t.Errorf("%s: verdict %q is not one of the six", row.IssueID, row.Verdict)
		}
	}

	// And each row says what each axis found, under the names the summary counts.
	want := map[string]string{"x-1": "matched", "y-1": "mismatch", "z-1": b11aLegacy, "w-1": b11aWisp}
	lines, _, err := readJSONLines(filepath.Join(s.store.dir, "commit_replay_results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatalf("a result row is not an object: %v\n%s", err, line)
		}
		id, _ := row["issue_id"].(string)
		if got := row["row_verdict"]; got != "matched" {
			t.Errorf("%s: row_verdict = %v, want matched", id, got)
		}
		if got := row["version_verdict"]; got != want[id] {
			t.Errorf("%s: version_verdict = %v, want %s", id, got, want[id])
		}
	}
}

// B11a.NullVsEmptyRules, reported: durable_state_lossy is a documented fidelity
// boundary, with its basis and the fields it covers and why each one is covered,
// and it is reported once for the run. It is not a finding, so no row repeats it.
func TestB11aSummaryReportsTheFidelityBoundaryOnce(t *testing.T) {
	s := newB11aSummaryRun(t)
	doc := b11aSummaryDoc(t, s.sum)

	raw, err := json.Marshal(doc["durable_state_lossy"])
	if err != nil {
		t.Fatal(err)
	}
	var lossy struct {
		Basis  string                `json:"basis"`
		Fields []versions.LossyField `json:"fields"`
	}
	if err := unmarshalStrict(raw, &lossy); err != nil {
		t.Fatalf("durable_state_lossy is not {basis, fields}: %v\n%s", err, raw)
	}
	if lossy.Basis != versions.LossyBasis {
		t.Errorf("basis = %q, want %q", lossy.Basis, versions.LossyBasis)
	}
	if len(lossy.Fields) == 0 || !reflect.DeepEqual(lossy.Fields, versions.Lossy()) {
		t.Errorf("fields = %+v\nwant the drift table's %+v", lossy.Fields, versions.Lossy())
	}

	raw, err = json.Marshal(doc["durable_state_excludes"])
	if err != nil {
		t.Fatal(err)
	}
	var excludes []versions.Exclude
	if err := unmarshalStrict(raw, &excludes); err != nil {
		t.Fatalf("durable_state_excludes is not a list of {name, reason}: %v\n%s", err, raw)
	}
	if !reflect.DeepEqual(excludes, versions.Excludes()) {
		t.Errorf("excludes = %+v\nwant the drift table's %+v", excludes, versions.Excludes())
	}
	names := map[string]bool{}
	for _, e := range excludes {
		names[e.Name] = true
	}
	for _, want := range []string{"comments", "product-read-surface", "is_blocked", "wisps", "labels"} {
		if !names[want] {
			t.Errorf("durable_state_excludes does not name %q (names: %v)", want, names)
		}
	}

	// Once for the run: not on any row.
	data, err := os.ReadFile(filepath.Join(s.store.dir, "commit_replay_results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"durable_state_lossy", "durable_state_excludes"} {
		if strings.Contains(string(data), key) {
			t.Errorf("a result row carries %s; it is reported once, in the summary", key)
		}
	}
}

// The summary is written as summary.json and read back as the same document, the
// new members included.
func TestB11aSummaryRoundTrips(t *testing.T) {
	s := newB11aSummaryRun(t)
	if err := s.store.WriteSummary(s.sum); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(s.store.dir, "summary.json"))
	if err != nil {
		t.Fatalf("reading summary.json: %v", err)
	}
	var out Summary
	if err := unmarshalStrict(data, &out); err != nil {
		t.Fatalf("summary.json is not the summary: %v\n%s", err, data)
	}
	if !reflect.DeepEqual(s.sum, out) {
		t.Errorf("summary read back differs:\n got  %+v\n want %+v", out, s.sum)
	}
}

// A resume counts the rows the stopped run had written through the same fold as
// the loop does, so the versioned reads of a resumed run equal those of a run that
// was not stopped.
func TestB11aResumeReproducesTheVersionedReads(t *testing.T) {
	s := newB11aSummaryRun(t)
	fresh := newRunner("run-1", s.store, nil)
	for _, row := range readJSONLTyped[CommitReplayResult](t, filepath.Join(s.store.dir, "commit_replay_results.jsonl")) {
		fresh.foldResult(row)
	}
	again := &b11aSummaryRun{store: s.store, steps: s.steps, walk: s.walk, r: fresh}

	want := b11aSummaryDoc(t, s.sum)["versioned_reads"]
	got := b11aSummaryDoc(t, again.summarize())["versioned_reads"]
	if want == nil || !reflect.DeepEqual(got, want) {
		t.Errorf("a resumed run counts versioned_reads %v, the run counted %v", got, want)
	}
}

// The version axis's mismatch is stored as a row of its own, in the category the
// summary of a run reads it by, with the section it differs in and both documents.
func TestB11aVersionMismatchIsStoredAsItsOwnRow(t *testing.T) {
	s := newB11aSummaryRun(t)
	rows := readJSONLTyped[Mismatch](t, filepath.Join(s.store.dir, "mismatches.jsonl"))
	if len(rows) != 1 {
		t.Fatalf("%d mismatch rows, want the one the stored title makes: %+v", len(rows), rows)
	}
	m := rows[0]
	if m.IssueID != "y-1" || m.Category != b11aVersionState || m.Section != "issue" || m.SourceCommit != "c1" || m.RunID != "run-1" {
		t.Errorf("mismatch = %+v, want a version-state row for y-1 at c1, section issue", m)
	}
	if got, _ := issueMember(t, m.ExpectedJSON, "title"); got != "T" {
		t.Errorf("expected document title = %v, want the oracle's T", got)
	}
	if got, _ := issueMember(t, m.ActualJSON, "title"); got != "stored" {
		t.Errorf("actual document title = %v, want the stored one", got)
	}
}
