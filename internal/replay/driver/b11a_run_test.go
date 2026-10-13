package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// ---- helpers ----------------------------------------------------------------

// versionMismatchFor returns the version axis's mismatch of an issue at a step: the
// row of category version-state. It fails the test when there is not exactly one.
func versionMismatchFor(t *testing.T, f *fixtureRun, commit, issue string) Mismatch {
	t.Helper()
	var found []Mismatch
	for _, m := range f.mismatches() {
		if m.SourceCommit == commit && m.IssueID == issue && m.Category == compare.CategoryVersionState {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d version-state mismatches for %s at %s, want exactly 1 (all: %+v)", len(found), issue, commit, f.mismatches())
	}
	return found[0]
}

// b11aReads is the versioned_reads member of a summary, as the document spells it.
type b11aReads struct {
	WriteFence *bool          `json:"write_fence"`
	Row        map[string]int `json:"row"`
	Version    map[string]int `json:"version"`
}

func b11aReadsOf(t *testing.T, summary []byte) b11aReads {
	t.Helper()
	var doc struct {
		VersionedReads *b11aReads `json:"versioned_reads"`
	}
	if err := json.Unmarshal(summary, &doc); err != nil {
		t.Fatalf("summary.json: %v\n%s", err, summary)
	}
	if doc.VersionedReads == nil {
		t.Fatalf("summary.json has no versioned_reads:\n%s", summary)
	}
	return *doc.VersionedReads
}

// b11aWriteFence reports whether a store's schema has the write fence: the column
// that marks a row as taking part in versioned history.
func b11aWriteFence(t testing.TB, dataDir string) bool {
	t.Helper()
	return seedQueryInt(t, dataDir,
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_name = 'issues' AND column_name = 'participation_generation'") == 1
}

// ---- a faithful run ---------------------------------------------------------

// B11a.StateMismatchDetected, its negative control: a real replay of a real
// history, with the bd under test left alone, agrees with the oracle on both axes
// for every issue at every step. The stored versions are the oracle's state,
// whatever the rows hold, and the summary says so.
func TestB11aFaithfulRunAgreesOnBothAxes(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	f := newFixtureRun(t, o, o.bin)
	mustComplete(t, f)

	for i, commit := range o.heads {
		row := f.resultFor(commit, o.ids[i])
		if row.Verdict != VerdictMatched || string(row.RowVerdict) != "matched" || string(row.VersionVerdict) != "matched" {
			t.Errorf("%s at %s: verdict %q, row axis %q, version axis %q, want all matched",
				o.ids[i], commit, row.Verdict, row.RowVerdict, row.VersionVerdict)
		}
	}
	if ms := f.mismatches(); len(ms) != 0 {
		t.Errorf("a faithful run stored mismatches: %+v", ms)
	}

	raw, err := os.ReadFile(filepath.Join(f.outDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	reads := b11aReadsOf(t, raw)
	if want := map[string]int{"matched": 2, "mismatch": 0, "uncomparable": 0}; !reflect.DeepEqual(reads.Row, want) {
		t.Errorf("row reads = %v, want %v", reads.Row, want)
	}
	wantVersion := map[string]int{
		"matched": 2, "mismatch": 0, "uncomparable": 0,
		b11aLegacy: 0, b11aRefused: 0, b11aNoRow: 0, b11aWisp: 0,
	}
	if !reflect.DeepEqual(reads.Version, wantVersion) {
		t.Errorf("version reads = %v, want %v", reads.Version, wantVersion)
	}
	if reads.WriteFence == nil || *reads.WriteFence != b11aWriteFence(t, f.workData) {
		t.Errorf("write_fence = %v, want whether the work clone's schema has participation_generation (%v)",
			reads.WriteFence, b11aWriteFence(t, f.workData))
	}
}

// B11a.StateMismatchDetected: one stored field altered, and the version axis names
// the section it differs in while the row axis, which reads other tables, still
// agrees. The bd under test is a stand-in that does what a build with a defect in
// what it stores would: after the real bd creates an issue, it rewrites the title
// in the issue's stored version and commits. The rows are not touched.
func TestB11aStateMismatchDetected(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	o := faultOracle(t)
	alpha, beta := o.ids[0], o.ids[1]
	f := newFixtureRun(t, o, o.bin)
	dolt, err := doltcli.Path()
	if err != nil {
		t.Fatalf("dolt: %v", err)
	}
	// The closing quote in the pattern keeps the rewrite from matching its own
	// result, and an issue whose title is something else is left as it was.
	rewrite := `UPDATE issue_versions SET durable_state = CAST(REPLACE(CAST(durable_state AS CHAR), '"title":"Alpha"', '"title":"Alpha-stored"') AS BINARY)`
	bin, _ := bdStandInAfter(t, o.bin, "", `if [ "$rc" -eq 0 ] && [ "$cmd" = create ]; then
	cd `+shellQuote(f.workData)+` || exit 1
	export DOLT_ROOT_PATH=`+shellQuote(os.Getenv("DOLT_ROOT_PATH"))+` DOLT_DISABLE_EVENT_FLUSH=1
	`+shellQuote(dolt)+` sql -q `+shellQuote(rewrite)+` || exit 1
	`+shellQuote(dolt)+` add -A || exit 1
	`+shellQuote(dolt)+` commit --allow-empty -m "stand-in: a stored title that is not the row's" || exit 1
fi`)
	f.cfg.Tools.IntegrationBin = bin
	mustComplete(t, f)

	row := f.resultFor(o.heads[0], alpha)
	if row.Verdict != VerdictMismatch || row.Matched {
		t.Fatalf("%s at %s = %q matched=%v, want a mismatch", alpha, o.heads[0], row.Verdict, row.Matched)
	}
	if string(row.RowVerdict) != "matched" || string(row.VersionVerdict) != "mismatch" {
		t.Errorf("%s: row axis %q and version axis %q, want the rows to agree and the stored version not to",
			alpha, row.RowVerdict, row.VersionVerdict)
	}
	m := versionMismatchFor(t, f, o.heads[0], alpha)
	if m.Section != "issue" {
		t.Errorf("section = %q, want issue: the title is the only thing that differs", m.Section)
	}
	if got, _ := issueMember(t, m.ExpectedJSON, "title"); got != "Alpha" {
		t.Errorf("expected document title = %v, want the oracle's, Alpha", got)
	}
	if got, _ := issueMember(t, m.ActualJSON, "title"); got != "Alpha-stored" {
		t.Errorf("actual document title = %v, want the stored one, Alpha-stored", got)
	}
	for _, mm := range f.mismatches() {
		if mm.IssueID == alpha && mm.Category != compare.CategoryVersionState {
			t.Errorf("a row-axis mismatch for %s although its rows are the oracle's: %+v", alpha, mm)
		}
	}

	// The issue after it is unaffected: the stand-in's rewrite found nothing in its
	// stored version.
	next := f.resultFor(o.heads[1], beta)
	if next.Verdict != VerdictMatched || string(next.RowVerdict) != "matched" || string(next.VersionVerdict) != "matched" {
		t.Errorf("%s at %s: verdict %q, row axis %q, version axis %q, want all matched",
			beta, o.heads[1], next.Verdict, next.RowVerdict, next.VersionVerdict)
	}

	raw, err := os.ReadFile(filepath.Join(f.outDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	reads := b11aReadsOf(t, raw)
	if reads.Version["mismatch"] != 1 || reads.Version["matched"] != 1 || reads.Row["matched"] != 2 {
		t.Errorf("versioned reads = %+v, want one version mismatch, one match, and two row matches", reads)
	}
	if got := f.summary().Verdicts["mismatch"]; got != 1 {
		t.Errorf("the summary counts %d mismatches, want the one step", got)
	}
}

// ---- a seeded run -----------------------------------------------------------

// B11a.SeededBaseline: a base built with versioned history off and then turned
// on holds issues no version was ever recorded for. Whether a replayed change to
// one of them records a version depends on whether the build has a write fence.
//
// With the fence, the change to a seeded issue records nothing, which is right, and
// the version axis reports it as legacy: neither a pass nor a mismatch. An issue the
// replay creates is versioned from its first revision and equals the oracle's. Without
// the fence, the change to a seeded issue records its first revision and the issue
// reads as equal. The test takes the expectation from the schema it ran against, and
// it runs on every build: a skip here would hide the one case that matters.
func TestB11aSeededBaseline(t *testing.T) {
	f := requireSeededRun(t, defaultSeedVariant)
	replaytest.Isolate(t)
	data := f.runCfg.WorkDataDir
	fence := b11aWriteFence(t, data)

	if len(f.results) != 3 {
		t.Fatalf("the run wrote %d result rows, want one for each of the tail's three commits", len(f.results))
	}
	byIssue := map[string]CommitReplayResult{}
	for _, r := range f.results {
		byIssue[r.IssueID] = r
		if r.Verdict != VerdictMatched {
			t.Errorf("%s at %s: step verdict %q, want matched", r.IssueID, r.SourceCommit, r.Verdict)
		}
		if string(r.RowVerdict) != "matched" {
			t.Errorf("%s at %s: row axis %q, want matched", r.IssueID, r.SourceCommit, r.RowVerdict)
		}
	}
	revisions := func(id string) int {
		return seedQueryInt(t, data, "SELECT COUNT(*) FROM issue_versions WHERE issue_id = '"+id+"'")
	}

	seeded := []string{f.o.ids[0], f.o.ids[1]} // the tail changes these two
	for _, id := range seeded {
		r, ok := byIssue[id]
		if !ok {
			t.Fatalf("no row for the seeded issue %s: %+v", id, f.results)
		}
		if fence {
			if string(r.VersionVerdict) != b11aLegacy {
				t.Errorf("seeded %s: version axis %q, want %q: the fence mints nothing for a legacy record", id, r.VersionVerdict, b11aLegacy)
			}
			if n := revisions(id); n != 0 {
				t.Errorf("seeded %s has %d stored versions, want none", id, n)
			}
			continue
		}
		if string(r.VersionVerdict) != "matched" {
			t.Errorf("seeded %s: version axis %q, want matched: without a fence the change records revision 1", id, r.VersionVerdict)
		}
		if n := revisions(id); n != 1 {
			t.Errorf("seeded %s has %d stored versions, want revision 1", id, n)
		}
	}

	created, ok := byIssue[f.o.tailID]
	if !ok {
		t.Fatalf("no row for the issue the tail creates, %s: %+v", f.o.tailID, f.results)
	}
	if string(created.VersionVerdict) != "matched" {
		t.Errorf("created %s: version axis %q, want matched: it is versioned from its first revision", f.o.tailID, created.VersionVerdict)
	}
	if n := revisions(f.o.tailID); n != 1 {
		t.Errorf("created %s has %d stored versions, want revision 1", f.o.tailID, n)
	}

	reads := b11aReadsOf(t, f.summaryRaw)
	if reads.WriteFence == nil || *reads.WriteFence != fence {
		t.Errorf("write_fence = %v, want %v: the schema's participation_generation column decides it", reads.WriteFence, fence)
	}
	wantMatched, wantLegacy := 1, 2
	if !fence {
		wantMatched, wantLegacy = 3, 0
	}
	if reads.Version["matched"] != wantMatched || reads.Version[b11aLegacy] != wantLegacy || reads.Version["mismatch"] != 0 {
		t.Errorf("version reads = %v, want %d matched, %d %s and no mismatch", reads.Version, wantMatched, wantLegacy, b11aLegacy)
	}
	if len(f.mismatches) != 0 {
		t.Errorf("the run stored %d mismatches, want none: %+v", len(f.mismatches), f.mismatches)
	}
}

// B11a.CandidateSeam, versioning off: when the product refuses to turn versioned
// history on over the seeded rows, the run goes on without it, and the version axis
// reports that the read did not run. It is never a pass: nothing was compared.
func TestB11aEnableRefusedIsNotAPass(t *testing.T) {
	f := requireSeededRun(t, bigNumberVariant)
	replaytest.Isolate(t)

	if f.summary.EnableRefused == nil {
		t.Fatalf("the summary does not record the refusal")
	}
	if len(f.results) != 3 {
		t.Fatalf("the run wrote %d result rows, want 3", len(f.results))
	}
	for _, r := range f.results {
		if string(r.VersionVerdict) != b11aRefused {
			t.Errorf("%s at %s: version axis %q, want %q", r.IssueID, r.SourceCommit, r.VersionVerdict, b11aRefused)
		}
		if r.Verdict != VerdictMatched {
			t.Errorf("%s at %s: step verdict %q, want matched: the rows agree", r.IssueID, r.SourceCommit, r.Verdict)
		}
	}
	reads := b11aReadsOf(t, f.summaryRaw)
	if reads.Version["matched"] != 0 || reads.Version[b11aRefused] != 3 {
		t.Errorf("version reads = %v, want no match and 3 %s", reads.Version, b11aRefused)
	}
	for _, m := range f.mismatches {
		if m.Category == compare.CategoryVersionState {
			t.Errorf("a read that did not run stored a mismatch: %+v", m)
		}
	}
}

// ---- the real readers --------------------------------------------------------

// The real environment hands the driver two readers, named as the seam names them,
// and each takes the issue and then the ref, as the seam is specified. The
// library calls under them take the ref first, so a swap is easy to make: it
// reads the issue named by a commit hash at a ref that is an issue id, and finds
// nothing or fails.
func TestB11aRealReadersTakeTheIssueThenTheRef(t *testing.T) {
	replaytest.Require(t, replaytest.NeedDolt)
	ctx := context.Background()

	dir := replaytest.NewDoltDB(t, "readers")
	replaytest.RunDolt(t, dir, "sql", "-q", "CREATE TABLE issues (id VARCHAR(64) PRIMARY KEY, title VARCHAR(255), status VARCHAR(32), participation_generation INT)")
	replaytest.RunDolt(t, dir, "sql", "-q", "CREATE TABLE issue_versions ("+
		"issue_id VARCHAR(255) NOT NULL, revision BIGINT NOT NULL, epoch INT NOT NULL, durable_state LONGBLOB, "+
		"change_actor VARCHAR(255), attribution_status VARCHAR(20) NOT NULL, change_at DATETIME(6) NOT NULL, "+
		"PRIMARY KEY (issue_id, revision))")
	replaytest.RunDolt(t, dir, "sql", "-q", "INSERT INTO issues VALUES ('x-1', 'Widget', 'open', 1), ('z-1', 'Old', 'open', NULL)")
	replaytest.RunDolt(t, dir, "sql", "-q", "INSERT INTO issue_versions VALUES "+
		"('x-1', 1, 1, CAST('{\"id\":\"x-1\",\"title\":\"Widget\"}' AS BINARY), 'replay', 'claimed', '2026-10-12 10:00:00.000000')")
	replaytest.RunDolt(t, dir, "add", "-A")
	replaytest.RunDolt(t, dir, "commit", "-m", "seed")
	head := replaytest.HeadCommit(t, dir)

	readers := realEnv{cfg: RunConfig{OracleDataDir: dir, WorkDataDir: dir}}.Readers()
	if len(readers) != 2 {
		t.Fatalf("the real environment offers %d readers, want the row reader and the versioned one", len(readers))
	}
	row := b11aReaderNamed(t, readers, "row")
	versioned := b11aReaderNamed(t, readers, "versioned")

	state, err := row.Read(ctx, dir, "x-1", head)
	if err != nil {
		t.Fatalf("the row reader: %v", err)
	}
	if state.View == nil || state.Version != nil || state.NotRun != "" {
		t.Fatalf("row state = %+v, want only a view", state)
	}
	if got, ok := state.View.Issue.Cell("title"); !ok || got.Text != "Widget" {
		t.Errorf("the row reader found title %q (present=%v) for x-1, want Widget", got.Text, ok)
	}

	state, err = versioned.Read(ctx, dir, "x-1", head)
	if err != nil {
		t.Fatalf("the versioned reader: %v", err)
	}
	if state.View != nil || state.NotRun != "" || state.Version == nil || state.Version.Record == nil {
		t.Fatalf("versioned state = %+v, want a stored version", state)
	}
	rec := state.Version.Record
	if rec.IssueID != "x-1" || rec.Revision != 1 {
		t.Errorf("record = %+v, want revision 1 of x-1", rec)
	}
	issue, err := rec.Issue()
	if err != nil {
		t.Fatalf("decoding the stored version: %v", err)
	}
	if issue.Title != "Widget" {
		t.Errorf("the stored title = %q, want Widget", issue.Title)
	}

	// An issue with a row and no stored version is legacy; one with neither is not.
	state, err = versioned.Read(ctx, dir, "z-1", head)
	if err != nil {
		t.Fatalf("the versioned reader on a legacy row: %v", err)
	}
	if state.Version == nil || state.Version.Record != nil || !state.Version.Legacy {
		t.Errorf("state of z-1 = %+v, want legacy: a row, no generation, no version", state)
	}
	state, err = versioned.Read(ctx, dir, "y-1", head)
	if err != nil {
		t.Fatalf("the versioned reader on an absent issue: %v", err)
	}
	if state.Version == nil || state.Version.Record != nil || state.Version.Legacy {
		t.Errorf("state of y-1 = %+v, want nothing stored and not legacy", state)
	}
}
