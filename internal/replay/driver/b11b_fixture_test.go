package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// The fixtures of the version-accounting tests. A negative control is a bd
// stand-in that runs the real bd and then does exactly one thing wrong in the work
// clone, as a build with one defect of its own would, so each check is shown to
// fire for the reason it names and for no other.

// The categories a version finding is filed under, as the summary spells them. The
// first is the write fence's, the last is the one kind of finding that is a
// recorded fact and not a failure; version-state is the category the single-read
// comparison already used.
const (
	b11bFence       = "version-fence"
	b11bCount       = "version-count"
	b11bChain       = "version-chain"
	b11bState       = "version-state"
	b11bAttribution = "version-attribution"
	b11bEpoch       = "version-epoch"
	b11bTime        = "version-time"
	b11bSurvival    = "version-survival"
	b11bGap         = "version-gap"
)

var b11bCategories = []string{
	b11bFence, b11bCount, b11bChain, b11bState, b11bAttribution,
	b11bEpoch, b11bTime, b11bSurvival, b11bGap,
}

// The steps of the history b11bOracle builds, as indexes into its heads.
const (
	b11bCreateA = iota
	b11bCreateB
	b11bUpdateA
	b11bUpdateB
	b11bCloseA
	b11bSteps
)

// ---- oracles ------------------------------------------------------------------

// b11bOracle is the history the controls replay: two issues, a change to each, and
// a close. Every step is one command that changes one issue, so a step is one
// action and one real change.
func b11bOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "b11b", func(o *oracleHistory) {
		a := o.create("Alpha")
		b := o.create("Beta")
		o.run("update", a, "--description", "alpha description")
		o.run("update", b, "--title", "Beta renamed")
		o.run("close", a, "--reason", "done")
		if len(o.heads) != b11bSteps {
			o.t.Fatalf("the history has %d commits, want %d: one for each command", len(o.heads), b11bSteps)
		}
	})
}

// b11bDeleteOracle ends in the delete of an issue that has a version row.
func b11bDeleteOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "b11b-delete", func(o *oracleHistory) {
		o.create("Alpha")
		b := o.create("Beta")
		o.run("delete", "--force", "--", b)
	})
}

// b11bCascadeOracle ends in the delete of an issue another issue has an edge to.
// Whether the edge's owner gets a version row for losing it is the product's to
// say; the tests record what it does.
func b11bCascadeOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "b11b-cascade", func(o *oracleHistory) {
		a := o.create("Alpha")
		o.create("Beta", "--deps", a)
		o.run("delete", "--force", "--", a)
	})
}

// b11bCreator is who made the issue of b11bCreatorOracle.
const b11bCreator = "creator-one"

// b11bCreatorOracle is an issue made by a named creator, then changed once. The
// creator is a column the two sides compare, so a replay has to make the issue as
// that creator; the change after it is the replay's own.
func b11bCreatorOracle(t testing.TB) *oracleHistory {
	return sharedOracle(t, "b11b-creator", func(o *oracleHistory) {
		a := o.create("Alpha", "--actor="+b11bCreator)
		o.run("update", a, "--description", "a later change")
	})
}

// ---- stand-ins that change what bd left -----------------------------------------

// b11bDoltPrelude defines wdolt, which runs the dolt CLI in the work clone under
// the test's own dolt environment and ends the stand-in when dolt fails. The
// snippets of a stand-in share one shell, so the function is there for both.
func b11bDoltPrelude(t testing.TB, workData string) string {
	t.Helper()
	dolt, err := doltcli.Path()
	if err != nil {
		t.Fatalf("dolt: %v", err)
	}
	return "wdolt() {\n" +
		"\t( cd " + shellQuote(workData) + " && DOLT_ROOT_PATH=" + shellQuote(os.Getenv("DOLT_ROOT_PATH")) +
		" DOLT_DISABLE_EVENT_FLUSH=1 " + shellQuote(dolt) + " \"$@\" >/dev/null ) ||\n" +
		"\t\t{ echo \"stand-in: dolt $* failed\" >&2; exit 1; }\n" +
		"}\n"
}

// b11bStandIn is a bd that runs body, then the real bd, then after. It is built
// once the test has its isolated environment, because the snippets run dolt in it.
func b11bStandIn(t testing.TB, realBd, workData, body, after string) string {
	t.Helper()
	bin, _ := bdStandInAfter(t, realBd, b11bDoltPrelude(t, workData)+body, after)
	return bin
}

// b11bWhen runs snippet for the bd command cmd whose arguments contain match: an
// issue id, which every replayed command carries.
func b11bWhen(cmd, match, snippet string) string {
	return "if [ \"$cmd\" = " + shellQuote(cmd) + " ]; then\n" +
		"case \"$*\" in\n*" + shellQuote(match) + "*)\n" + snippet + ";;\nesac\n" +
		"fi\n"
}

// b11bAfter is b11bWhen for a command the real bd carried out.
func b11bAfter(cmd, match, snippet string) string {
	return "if [ \"$rc\" -eq 0 ]; then\n" + b11bWhen(cmd, match, snippet) + "fi\n"
}

// b11bSQL runs statements in the work clone and commits them, which is how the
// stand-in's change is part of the history the ledger reads.
func b11bSQL(message string, stmts ...string) string {
	var b strings.Builder
	for _, s := range stmts {
		b.WriteString("wdolt sql -q " + shellQuote(s) + "\n")
	}
	b.WriteString("wdolt add -A\n")
	b.WriteString("wdolt commit --allow-empty -m " + shellQuote("stand-in: "+message) + "\n")
	return b.String()
}

// The statements the stand-ins run. A version row has the columns of the product's
// table; a statement that sets a counter keeps updated_at as it was, as the
// product's own bookkeeping does, so that it is not a change to the issue.

// b11bDuplicateNewest writes a second row for the issue: its newest one again, at
// the next revision.
func b11bDuplicateNewest(id string) string {
	q := doltcli.SQLQuote(id)
	return "INSERT INTO issue_versions (issue_id, revision, epoch, durable_state, change_actor, change_agent, change_message, change_at, attribution_status) " +
		"SELECT issue_id, revision + 1, epoch, durable_state, change_actor, change_agent, change_message, UTC_TIMESTAMP(6), attribution_status " +
		"FROM issue_versions WHERE issue_id = " + q + " AND revision = (SELECT MAX(revision) FROM issue_versions WHERE issue_id = " + q + ")"
}

// b11bSyncCounter sets the issue's counter to its newest revision, which is where
// the product leaves it.
func b11bSyncCounter(id string) string {
	q := doltcli.SQLQuote(id)
	return "UPDATE issues SET current_revision = (SELECT MAX(revision) FROM issue_versions WHERE issue_id = " + q + "), updated_at = updated_at WHERE id = " + q
}

// b11bSetCounter sets the issue's counter to a value.
func b11bSetCounter(id, value string) string {
	return "UPDATE issues SET current_revision = " + value + ", updated_at = updated_at WHERE id = " + doltcli.SQLQuote(id)
}

// b11bSetVersion sets a column of one version row.
func b11bSetVersion(id string, revision int, assignment string) string {
	return "UPDATE issue_versions SET " + assignment + " WHERE issue_id = " + doltcli.SQLQuote(id) + " AND revision = " + strconv.Itoa(revision)
}

// b11bDropVersions removes every row of the issue.
func b11bDropVersions(id string) string {
	return "DELETE FROM issue_versions WHERE issue_id = " + doltcli.SQLQuote(id)
}

// ---- what a run left ------------------------------------------------------------

// b11bOutcome is a finished run's output, read into memory: its result rows, its
// mismatch rows and its summary document.
type b11bOutcome struct {
	t          *testing.T
	results    []CommitReplayResult
	mismatches []Mismatch
	raw        []byte
}

func b11bRead(t *testing.T, outDir string) *b11bOutcome {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(outDir, "summary.json")) // #nosec G304 -- this test's own output
	if err != nil {
		t.Fatalf("reading summary.json: %v", err)
	}
	return &b11bOutcome{t: t, results: readCommitReplayResults(t, outDir), mismatches: readMismatches(t, outDir), raw: raw}
}

// b11bReadsOf is the versioned_reads member of a summary document.
func b11bReadsOf(t *testing.T, summary []byte) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		VersionedReads map[string]json.RawMessage `json:"versioned_reads"`
	}
	if err := json.Unmarshal(summary, &doc); err != nil {
		t.Fatalf("summary.json: %v", err)
	}
	if doc.VersionedReads == nil {
		t.Fatalf("summary.json has no versioned_reads")
	}
	return doc.VersionedReads
}

// b11bMembers names the members of a decoded object, sorted, for a failure message
// that says what is there without printing the document.
func b11bMembers(m map[string]json.RawMessage) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// b11bInt reads a member of versioned_reads that must be a JSON integer. A member
// that is absent, null or not an integer fails the test: a counter that is zero is
// written as 0, and nothing here reads a missing one as zero.
func b11bInt(t *testing.T, summary []byte, key string) int {
	t.Helper()
	reads := b11bReadsOf(t, summary)
	raw, ok := reads[key]
	if !ok {
		t.Fatalf("versioned_reads has no %q; it has %v", key, b11bMembers(reads))
	}
	var n int
	if string(bytes.TrimSpace(raw)) == "null" || json.Unmarshal(raw, &n) != nil {
		t.Fatalf("versioned_reads.%s is %s, want an integer", key, raw)
	}
	return n
}

// b11bFindingsOf reads versioned_reads.findings: a count for every category,
// zeros included.
func b11bFindingsOf(t *testing.T, summary []byte) map[string]int {
	t.Helper()
	reads := b11bReadsOf(t, summary)
	raw, ok := reads["findings"]
	if !ok {
		t.Fatalf("versioned_reads has no findings; it has %v", b11bMembers(reads))
	}
	var counts map[string]*int
	if err := json.Unmarshal(raw, &counts); err != nil {
		t.Fatalf("versioned_reads.findings is %s: %v", raw, err)
	}
	got := map[string]int{}
	for _, c := range b11bCategories {
		n, ok := counts[c]
		if !ok || n == nil {
			t.Fatalf("versioned_reads.findings has no count for %s: %s", c, raw)
		}
		got[c] = *n
	}
	return got
}

// under is the same outcome read by a subtest: a failure is reported by, and stops,
// the test that made the assertion and not the one that read the run.
func (o *b11bOutcome) under(t *testing.T) *b11bOutcome {
	c := *o
	c.t = t
	return &c
}

func (o *b11bOutcome) findings() map[string]int {
	o.t.Helper()
	return b11bFindingsOf(o.t, o.raw)
}

func (o *b11bOutcome) count(key string) int {
	o.t.Helper()
	return b11bInt(o.t, o.raw, key)
}

// of is the mismatch rows of a category.
func (o *b11bOutcome) of(category string) []Mismatch {
	var found []Mismatch
	for _, m := range o.mismatches {
		if m.Category == category {
			found = append(found, m)
		}
	}
	return found
}

// one is the only mismatch row of a category, which must be the one for the issue
// at the step, carry the sub-case as its section and carry both sides as JSON.
func (o *b11bOutcome) one(category, section, commit, issue string) Mismatch {
	o.t.Helper()
	found := o.of(category)
	if len(found) != 1 {
		o.t.Fatalf("%d %s mismatches, want exactly 1: %+v", len(found), category, found)
	}
	m := found[0]
	if m.SourceCommit != commit || m.IssueID != issue {
		o.t.Errorf("the %s mismatch is for %s at %s, want %s at %s", category, m.IssueID, m.SourceCommit, issue, commit)
	}
	if m.Section != section {
		o.t.Errorf("the %s mismatch has the section %q, want the sub-case %q", category, m.Section, section)
	}
	if !json.Valid(m.ExpectedJSON) || !json.Valid(m.ActualJSON) {
		o.t.Errorf("the %s mismatch does not carry both sides as JSON: expected %q, actual %q", category, m.ExpectedJSON, m.ActualJSON)
	}
	return m
}

// row is the result row of an issue at a step: exactly one.
func (o *b11bOutcome) row(commit, issue string) CommitReplayResult {
	o.t.Helper()
	var found []CommitReplayResult
	for _, r := range o.results {
		if r.SourceCommit == commit && r.IssueID == issue {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		o.t.Fatalf("%d rows for %s at %s, want exactly 1: %+v", len(found), issue, commit, o.results)
	}
	return found[0]
}

// stepIsMismatch reports whether any row of the step is a mismatch. A finding is
// filed under the issue it is about, which a step does not always read; the step's
// verdict is where it shows.
func (o *b11bOutcome) stepIsMismatch(commit string) bool {
	for _, r := range o.results {
		if r.SourceCommit == commit && r.Verdict == VerdictMismatch {
			return true
		}
	}
	return false
}

// onlyFinding asserts that the summary counts one finding of the category and none
// of any other, bar the categories the caller names.
func (o *b11bOutcome) onlyFinding(category string, besides ...string) {
	o.t.Helper()
	for c, n := range o.findings() {
		switch {
		case c == category:
			if n != 1 {
				o.t.Errorf("findings[%s] = %d, want 1", c, n)
			}
		case slices.Contains(besides, c):
		default:
			if n != 0 {
				o.t.Errorf("findings[%s] = %d, want none beside the one the stand-in caused", c, n)
			}
		}
	}
}

// requireVersionMismatch asserts that the issue's row at the step is a mismatch on
// its version axis, which is where a finding about its versions lands.
func (o *b11bOutcome) requireVersionMismatch(commit, issue string) {
	o.t.Helper()
	r := o.row(commit, issue)
	if r.Verdict != VerdictMismatch || r.Matched || string(r.VersionVerdict) != "mismatch" {
		o.t.Errorf("%s at %s: verdict %q matched=%v version axis %q, want a mismatch on the version axis", issue, commit, r.Verdict, r.Matched, r.VersionVerdict)
	}
}

// clean asserts that the run found nothing: no finding in the summary, no
// mismatch row, and every step matched.
func (o *b11bOutcome) clean() {
	o.t.Helper()
	for c, n := range o.findings() {
		if n != 0 {
			o.t.Errorf("findings[%s] = %d, want none", c, n)
		}
	}
	for _, m := range o.mismatches {
		o.t.Errorf("a mismatch row on a faithful run: %+v", m)
	}
	for _, r := range o.results {
		if r.Verdict != VerdictMatched {
			o.t.Errorf("%s at %s: verdict %q on a faithful run", r.IssueID, r.SourceCommit, r.Verdict)
		}
	}
}

// b11bVersions is how many version rows the issue has.
func b11bVersions(t testing.TB, dataDir, id string) int {
	t.Helper()
	return seedQueryInt(t, dataDir, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = "+doltcli.SQLQuote(id))
}

// ---- replays ---------------------------------------------------------------------

// b11bTampered replays the oracle through a bd that runs body before the real one
// and after after it, and reads what the run left. The run has to complete: a
// finding is a row in the output, not a failure of the run.
func b11bTampered(t *testing.T, o *oracleHistory, body, after string) (*fixtureRun, *b11bOutcome) {
	t.Helper()
	f := newFixtureRun(t, o, o.bin)
	f.cfg.Tools.IntegrationBin = b11bStandIn(t, o.bin, f.workData, body, after)
	mustComplete(t, f)
	return f, b11bRead(t, f.outDir)
}

// b11bSeededCopy replays the tail of the shared seeded flow again, on a private
// copy of the seeded clone, through the bd that build returns. build is given the
// copy's data directory, to read what the stand-in needs, before the replay starts.
func b11bSeededCopy(t *testing.T, f *seededFlow, build func(data string) string) (data string, out *b11bOutcome) {
	t.Helper()
	replaytest.Isolate(t)
	copyDir := filepath.Join(t.TempDir(), "work")
	copyProject(t, f.cfg.WorkDir, copyDir)
	data = replaytest.DataDir(t, copyDir)
	cfg := f.runCfg
	cfg.WorkDir, cfg.WorkDataDir = copyDir, data
	cfg.OutDir = filepath.Join(t.TempDir(), "out")
	cfg.Tools = Tools{IntegrationBin: build(data)}
	rec := f.rec
	cfg.Seed = &rec
	cfg.Observer = nil
	run, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the run on the seeded copy: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("the run on the seeded copy ended %q, want completed", run.Status)
	}
	return data, b11bRead(t, cfg.OutDir)
}

// b11bSeededStep is the commit and the issue of a step of the seeded tail. The
// tail is three commands, in this order: a change to the first seeded issue, a
// change to the second, and a create.
func b11bSeededStep(t *testing.T, f *seededFlow, step int) (commit, issue string) {
	t.Helper()
	if len(f.results) != 3 {
		t.Fatalf("the seeded run has %d rows, want the tail's three", len(f.results))
	}
	r := f.results[step]
	if r.Step != step {
		t.Fatalf("row %d of the seeded run is for step %d", step, r.Step)
	}
	return r.SourceCommit, r.IssueID
}
