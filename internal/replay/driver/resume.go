package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
)

// begun is a run that is ready for its steps, started or picked up again.
type begun struct {
	run   ReplayRun
	rec   runRecord
	store *Store
	r     *runner
	// first is the position of the step to run first: steps before it are done.
	first int
}

// beginRun starts a run of its own. Before anything else is done, run.json names
// the run and what it is pinned to, and the run has its running row: a crash from
// here on leaves a run that says it was running and never said it stopped.
func beginRun(cfg RunConfig, walk *Walk) (begun, error) {
	if err := checkNoUnfinishedRun(cfg.OutDir, cfg.WorkDir); err != nil {
		return begun{}, err
	}
	mode := "exhaustive"
	if cfg.SampleSize > 0 {
		mode = "sampled"
	}
	run := ReplayRun{
		ID:             GenerateRunID(),
		IntegrationRef: cfg.IntegrationRef,
		IntegrationSHA: cfg.IntegrationSHA,
		Mode:           mode,
		SampleSize:     cfg.SampleSize,
		StartedAt:      time.Now().UTC(),
		Status:         "running",
	}
	store, err := NewStore(cfg.OutDir)
	if err != nil {
		return begun{run: run}, err
	}
	// A journal still in the directory is an earlier run's, one that completed.
	if err := removeFile(store.path(fileJournal)); err != nil {
		return begun{run: run}, err
	}
	rec := newRunRecord(run, walk)
	if err := writeRunRecord(cfg.OutDir, rec); err != nil {
		return begun{run: run}, err
	}
	if err := store.WriteReplayRun(run); err != nil {
		return begun{run: run}, fmt.Errorf("writing the running row: %w", err)
	}
	r := newRunner(run.ID, store, cfg.Observer)
	r.journaled = true
	return begun{run: run, rec: rec, store: store, r: r}, nil
}

// workHeadPattern is what the head of a dolt database looks like: the hash of a
// commit, 32 characters of base 32. The journal's work heads are checked against it
// before they go to dolt as an argument.
var workHeadPattern = regexp.MustCompile(`^[0-9a-v]{32}$`)

// resumeRun picks up the stopped run cfg.Resume names, from where its journal says
// it got to. It refuses a run that cannot be continued before it changes anything.
//
// What a stopped run leaves is its rows, its journal and the work clone, and the
// three can disagree about the step it was in: bd may have changed the work clone
// for it, its rows may be written, and neither is in the journal until the step is
// finished. The journal rules. Steps it says are finished are kept, and all of the
// run's state is rebuilt from them: the counts from the rows through the loop's own
// folds, and what no row says from the journal's last finished entry. A step it
// started and did not finish is undone: the work clone is rewound to the head the
// journal recorded for it, and its rows are dropped before the state is rebuilt
// from the rest.
func resumeRun(ctx context.Context, cfg RunConfig, walk *Walk, steps []Step) (begun, error) {
	rec, completed, err := checkResume(cfg.OutDir, cfg.Resume, walk, cfg.SampleSize)
	if err != nil {
		return begun{run: completed}, err
	}
	if err := checkResumeBuild(rec, cfg.IntegrationSHA); err != nil {
		return begun{}, err
	}
	js, err := readJournal(cfg.OutDir, steps)
	if err != nil {
		return begun{}, err
	}
	if js.open != nil && !workHeadPattern.MatchString(js.open.WorkHeadBefore) {
		return begun{}, fmt.Errorf("%s: step %d was started from the work head %q, which is not a commit hash", fileJournal, js.open.Step, js.open.WorkHeadBefore)
	}
	var files [3]rowFile
	for i, name := range []string{fileResults, fileMismatches, fileGaps} {
		if files[i], err = readRowFile(filepath.Join(cfg.OutDir, name), rec.ID, js.finished); err != nil {
			return begun{}, err
		}
	}
	results, mismatches, gaps := files[0], files[1], files[2]

	// Everything is checked. From here the run's files and the work clone change.
	store, err := NewStore(cfg.OutDir)
	if err != nil {
		return begun{}, err
	}
	for _, name := range []string{fileJournal, fileRuns} {
		if err := repairTornTail(store.path(name)); err != nil {
			return begun{}, err
		}
	}
	if js.open != nil {
		if err := rewindWorkClone(ctx, cfg.WorkDataDir, js.open.WorkHeadBefore); err != nil {
			return begun{}, err
		}
	}
	for _, f := range []rowFile{results, mismatches, gaps} {
		if err := f.apply(); err != nil {
			return begun{}, err
		}
	}

	r := newRunner(rec.ID, store, cfg.Observer)
	r.journaled = true
	if err := r.restore(results, gaps, js.last); err != nil {
		return begun{}, err
	}

	rec.Status = "running"
	if err := writeRunRecord(cfg.OutDir, rec); err != nil {
		return begun{}, err
	}
	run := rec.replayRun("running")
	if err := store.WriteReplayRun(run); err != nil {
		return begun{run: run}, fmt.Errorf("writing the running row: %w", err)
	}
	return begun{run: run, rec: rec, store: store, r: r, first: js.finished}, nil
}

// rewindWorkClone puts the work clone's database back to head, the commit it was
// at before a step that did not finish. Whatever the step did to it, committed or
// not, is gone.
func rewindWorkClone(ctx context.Context, dataDir, head string) error {
	if _, err := doltcli.Run(ctx, dataDir, "reset", "--hard", head); err != nil {
		return fmt.Errorf("rewinding the work clone to %s: %w", head, err)
	}
	got, err := headCommitOf(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("checking the rewound work clone: %w", err)
	}
	if got != head {
		return fmt.Errorf("rewinding the work clone to %s left it at %s", head, got)
	}
	return nil
}

// restore brings the runner to the state it was in after the step the journal
// last finished. What the rows say is folded through the same folds the loop folds
// them through as it writes them, and the quarantine is rebuilt from the same note;
// what no row says, the derived tables and the schema skew, comes from the counts
// the journal kept with that step.
func (r *runner) restore(results, gaps rowFile, last *journalEntry) error {
	for _, line := range results.own {
		var res CommitReplayResult
		if err := json.Unmarshal(line, &res); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(results.path), err)
		}
		r.foldResult(res)
		r.q.note(res)
	}
	for _, line := range gaps.own {
		var row CoverageGapRow
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(gaps.path), err)
		}
		r.foldGap(row)
	}
	if last != nil {
		r.derived = copyCounts(last.Derived)
		r.skew = copySkew(last.SchemaSkew)
	}
	return nil
}

// rowFile is one of a run's row files, read to say which of its rows stay.
type rowFile struct {
	path string
	// keep is every line that stays, in order: the rows of other runs, and the
	// rows of this one for steps the journal says are finished.
	keep [][]byte
	// own is the lines among keep that are this run's.
	own [][]byte
	// rewrite is whether the file is more than keep: a row is dropped, or the last
	// line was cut short.
	rewrite bool
}

// readRowFile reads the row file at path and sorts the rows of run runID by their
// step: the rows of a step the journal does not say is finished are dropped. A row
// of another run is not this run's to touch. A row of this run that names no step
// cannot be sorted, so it is an error, and so is a line that is not a row.
func readRowFile(path, runID string, finished int) (rowFile, error) {
	lines, torn, err := readJSONLines(path)
	if err != nil {
		return rowFile{}, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	f := rowFile{path: path, rewrite: torn}
	for i, line := range lines {
		var key struct {
			RunID string `json:"run_id"`
			Step  *int   `json:"step"`
		}
		if err := json.Unmarshal(line, &key); err != nil {
			return rowFile{}, fmt.Errorf("%s line %d: %w", filepath.Base(path), i+1, err)
		}
		switch {
		case key.RunID != runID:
			f.keep = append(f.keep, line)
		case key.Step == nil:
			return rowFile{}, fmt.Errorf("%s line %d: a row of run %s names no step, so there is no telling whether the run finished it", filepath.Base(path), i+1, runID)
		case *key.Step >= finished:
			f.rewrite = true
		default:
			f.keep = append(f.keep, line)
			f.own = append(f.own, line)
		}
	}
	return f, nil
}

// apply rewrites the file to the rows that stay, if that is a change.
func (f rowFile) apply() error {
	if !f.rewrite {
		return nil
	}
	return rewriteJSONLines(f.path, f.keep)
}

// repairTornTail makes a JSON-Lines file end at the end of a line. A write the
// process died in the middle of leaves part of a line, and the next append would
// be glued onto it.
func repairTornTail(path string) error {
	lines, torn, err := readJSONLines(path)
	if err != nil || !torn {
		return err
	}
	return rewriteJSONLines(path, lines)
}
