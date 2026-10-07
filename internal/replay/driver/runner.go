package driver

import (
	"context"
	"fmt"

	"github.com/steveyegge/beads/internal/replay/compare"
)

// runner holds the state of one run's step loop: where its rows go, who hears
// about them, which issues are quarantined, and the counts the summary reports.
type runner struct {
	runID    string
	store    *Store
	observer Observer
	q        quarantined

	verdicts       map[Verdict]int
	gapTables      map[string]int
	gapColumns     map[string]int
	derived        map[string]int
	skew           compare.Skew
	numberFidelity int

	// baseline and enableRefused are what a seeded run found before its first step:
	// how the work clone compared with the oracle at the base, and whether the
	// product refused to turn versioned history on. Both stay nil on a run that did
	// not find them.
	baseline      *BaselineFinding
	enableRefused *EnableRefusal
}

func newRunner(runID string, store *Store, observer Observer) *runner {
	return &runner{
		runID:      runID,
		store:      store,
		observer:   observer,
		q:          quarantined{},
		verdicts:   map[Verdict]int{},
		gapTables:  map[string]int{},
		gapColumns: map[string]int{},
		derived:    map[string]int{},
		skew:       compare.Skew{},
	}
}

// runSteps replays the steps in order. A step that fails stops the run, and
// writes nothing for itself: its rows are written only once it has finished.
func (r *runner) runSteps(ctx context.Context, env stepEnv, steps []Step) error {
	for _, st := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := replayStep(ctx, env, r.q, st)
		if err != nil {
			return fmt.Errorf("step %d (%s to %s): %w", st.Index, st.From.Hash, st.To.Hash, err)
		}
		if err := r.record(ctx, st, out); err != nil {
			return fmt.Errorf("step %d (%s to %s): %w", st.Index, st.From.Hash, st.To.Hash, err)
		}
	}
	return nil
}

// record writes a finished step's rows and tells the Observer, one issue at a
// time. An Observer that returns an error stops the run.
func (r *runner) record(ctx context.Context, st Step, out *stepOutcome) error {
	for _, g := range out.Gaps {
		row := CoverageGapRow{
			RunID: r.runID, FromCommit: st.From.Hash, SourceCommit: st.To.Hash,
			Table: g.Table, Unknown: g.Unknown, Reason: g.Reason,
		}
		if err := r.store.WriteCoverageGap(row); err != nil {
			return fmt.Errorf("write coverage gap for %s: %w", g.Table, err)
		}
		r.gapTables[g.Table]++
	}
	for _, table := range out.Derived {
		r.derived[table]++
	}
	mergeSkew(r.skew, out.Skew)

	for _, oc := range out.Issues {
		res := oc.Result
		res.RunID = r.runID
		if err := r.store.WriteCommitReplayResult(res); err != nil {
			return fmt.Errorf("write result for %s: %w", res.IssueID, err)
		}
		if m := oc.Mismatch; m != nil {
			row := Mismatch{
				RunID: r.runID, SourceCommit: res.SourceCommit, IssueID: res.IssueID,
				Category: m.Category, ExpectedJSON: m.ExpectedJSON, ActualJSON: m.ActualJSON,
			}
			if err := r.store.WriteMismatch(row); err != nil {
				return fmt.Errorf("write mismatch for %s: %w", res.IssueID, err)
			}
		}
		r.verdicts[res.Verdict]++
		if res.Verdict == VerdictUncomparable {
			r.numberFidelity++
		}
		if d := res.Detail; d != nil && res.Verdict == VerdictUntranslatable {
			for _, col := range d.Columns {
				r.gapColumns["issues."+col]++
			}
		}
		if r.observer != nil {
			ev := StepIssueResult{Step: st.Index, Merge: st.Merge, Net: st.Net, WriteLatency: oc.WriteLatency, Result: res}
			if err := r.observer.Observe(ctx, ev); err != nil {
				return fmt.Errorf("observer: %w", err)
			}
		}
	}
	return nil
}

// Summary is what a run says about itself, so that a green run cannot be read as
// whole-history coverage: how much of the history it covered, what each issue at
// each step came to, what it could not replay or compare and why.
type Summary struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Mode       string `json:"mode"`
	SampleSize int    `json:"sample_size,omitempty"`
	// CoveredRange is the part of the history the steps stand for.
	CoveredRange CoveredRange `json:"covered_range"`
	// Verdicts counts the (step, issue) rows by verdict; every verdict is listed,
	// the ones that did not occur with a zero.
	Verdicts map[string]int `json:"verdicts"`
	// Gaps are the holes in what was replayed.
	Gaps Gaps `json:"gaps"`
	// Derived counts the steps that changed each table bd rewrites on its own.
	Derived map[string]int `json:"derived"`
	// SchemaSkew lists, per table, the columns that only one side's schema had.
	// Skew is recorded once per run and is never a mismatch.
	SchemaSkew compare.Skew `json:"schema_skew"`
	// NumberFidelity counts the pairs that held a number outside what can be
	// compared exactly. They are uncomparable, neither matched nor mismatched.
	NumberFidelity int `json:"number_fidelity"`
	// Seed describes the seeding that gave the work clone the oracle's base state;
	// absent when there was none.
	Seed *SeedRecord `json:"seed,omitempty"`
	// Baseline counts how many of the seeded issues the work clone holds exactly as
	// the oracle did at the base, before anything was replayed; absent when the run
	// was not seeded.
	Baseline *BaselineFinding `json:"baseline,omitempty"`
	// EnableRefused is set when the product would not turn versioned history on over
	// the seeded rows, and the run went on without it; absent otherwise.
	EnableRefused *EnableRefusal `json:"enable_refused,omitempty"`
}

// CoveredRange is the part of the oracle's history a run replayed. History
// before the base is not replayed, a merge step covers its side branch by its
// net diff, and a sampled run replays only the sampled steps, so the commit
// counts are what lets a reader see how much of the whole a run stands for.
type CoveredRange struct {
	BaseCommit string `json:"base_commit"`
	BaseDate   string `json:"base_date"`
	HeadCommit string `json:"head_commit"`
	HeadDate   string `json:"head_date"`
	// CommitsTotal is every commit reachable from the oracle's head.
	CommitsTotal int `json:"commits_total"`
	// CommitsReplayed is the number of steps the run replayed.
	CommitsReplayed int `json:"commits_replayed"`
}

// Gaps are what the run could not replay, counted. A divergence the harness
// caused is a coverage gap, not a product mismatch, and is reported apart.
type Gaps struct {
	// Tables counts the steps that changed each table the replay does not cover.
	Tables map[string]int `json:"tables"`
	// Columns counts the untranslatable (step, issue) rows by the column that had
	// no bd form, as table.column.
	Columns map[string]int `json:"columns"`
}

// summarize assembles the run's summary from what the loop counted.
func (r *runner) summarize(run ReplayRun, w *Walk, steps []Step, seed *SeedRecord) Summary {
	byVerdict := make(map[string]int, len(verdicts))
	for _, v := range verdicts {
		byVerdict[string(v)] = r.verdicts[v]
	}
	return Summary{
		RunID:      run.ID,
		Status:     run.Status,
		Mode:       run.Mode,
		SampleSize: run.SampleSize,
		CoveredRange: CoveredRange{
			BaseCommit:      w.Base().Hash,
			BaseDate:        w.Base().Date,
			HeadCommit:      w.Head().Hash,
			HeadDate:        w.Head().Date,
			CommitsTotal:    w.Total,
			CommitsReplayed: len(steps),
		},
		Verdicts:       byVerdict,
		Gaps:           Gaps{Tables: copyCounts(r.gapTables), Columns: copyCounts(r.gapColumns)},
		Derived:        copyCounts(r.derived),
		SchemaSkew:     copySkew(r.skew),
		NumberFidelity: r.numberFidelity,
		Seed:           seed,
		Baseline:       r.baseline,
		EnableRefused:  r.enableRefused,
	}
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copySkew(s compare.Skew) compare.Skew {
	out := make(compare.Skew, len(s))
	for table, cs := range s {
		out[table] = compare.ColumnSkew{
			OracleOnly:    copyStrings(cs.OracleOnly),
			CandidateOnly: copyStrings(cs.CandidateOnly),
		}
	}
	return out
}
