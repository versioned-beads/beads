package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Why a run is not started or resumed. Each is wrapped with what the caller needs
// to act on it, so errors.Is is for the code that must tell them apart and the
// message is for the person who reads it.
var (
	// ErrRunUnfinished: the output directory holds a run that never completed. A
	// new run does not start over it, because that would bury it; it is resumed or
	// its directories are removed.
	ErrRunUnfinished = errors.New("the output directory holds an unfinished run")
	// ErrWorkDirNotEmpty: a run starts into a work directory that is empty or not
	// there, since the work project is the one thing a run keeps outside its output
	// directory and nothing else may have written to it.
	ErrWorkDirNotEmpty = errors.New("the work directory is not empty")
	// ErrRunCompleted: the run to resume completed. Nothing is applied twice, so it
	// is not run again.
	ErrRunCompleted = errors.New("the run has already completed")
	// ErrResumeRefused: the run cannot be resumed, because it is not the run the
	// output directory holds or because what it was begun over has changed.
	ErrResumeRefused = errors.New("the run cannot be resumed")
)

// runRecord is run.json: what a run was pinned to when it began. A resume
// continues a run only over the same oracle, the same base, the same integration
// build and the same sample, so it checks each against this before it does
// anything.
type runRecord struct {
	ID             string    `json:"id"`
	IntegrationRef string    `json:"integration_ref"`
	IntegrationSHA string    `json:"integration_sha"`
	OracleHead     string    `json:"oracle_head"`
	BaseCommit     string    `json:"base_commit"`
	Mode           string    `json:"mode"`
	SampleSize     int       `json:"sample_size,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	// Status is how the run stood when this file was last written. The last row of
	// the run in replay_runs.jsonl is what says whether it finished.
	Status string `json:"status"`
}

// newRunRecord is the record of a run about to begin.
func newRunRecord(run ReplayRun, walk *Walk) runRecord {
	return runRecord{
		ID:             run.ID,
		IntegrationRef: run.IntegrationRef,
		IntegrationSHA: run.IntegrationSHA,
		OracleHead:     walk.Head().Hash,
		BaseCommit:     walk.Base().Hash,
		Mode:           run.Mode,
		SampleSize:     run.SampleSize,
		StartedAt:      run.StartedAt,
		Status:         run.Status,
	}
}

// replayRun is the ReplayRun the record stands for, with the status the caller
// gives it.
func (rec runRecord) replayRun(status string) ReplayRun {
	return ReplayRun{
		ID:             rec.ID,
		IntegrationRef: rec.IntegrationRef,
		IntegrationSHA: rec.IntegrationSHA,
		Mode:           rec.Mode,
		SampleSize:     rec.SampleSize,
		StartedAt:      rec.StartedAt,
		Status:         status,
	}
}

// readRunRecord reads run.json from outDir; nil, with no error, when there is
// none.
func readRunRecord(outDir string) (*runRecord, error) {
	data, err := os.ReadFile(filepath.Join(outDir, fileRunRecord)) // #nosec G304 -- a fixed file name in the output directory
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", fileRunRecord, err)
	}
	var rec runRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s in %s is not a run record: %w", fileRunRecord, outDir, err)
	}
	if rec.ID == "" {
		return nil, fmt.Errorf("%s in %s names no run", fileRunRecord, outDir)
	}
	return &rec, nil
}

// writeRunRecord writes run.json into outDir, replacing an earlier one whole.
func writeRunRecord(outDir string, rec runRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", fileRunRecord, err)
	}
	return writeFileAtomic(filepath.Join(outDir, fileRunRecord), append(data, '\n'))
}

// lastRunRow is the newest row replay_runs.jsonl has for the run: rows are only
// ever appended and the last one for an id is the run's state. It is nil when
// there is none, as for a run that stopped before its first row.
func lastRunRow(outDir, id string) (*ReplayRun, error) {
	lines, _, err := readJSONLines(filepath.Join(outDir, fileRuns))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", fileRuns, err)
	}
	var last *ReplayRun
	for i, line := range lines {
		var row ReplayRun
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", fileRuns, i+1, err)
		}
		if row.ID == id {
			last = &row
		}
	}
	return last, nil
}

// checkNoUnfinishedRun refuses a new run over the output directory of one that
// never completed. A run that was killed has a running row and no other, a run
// that failed or was interrupted has a failed one: either way it is for --resume
// to continue.
func checkNoUnfinishedRun(outDir, workDir string) error {
	rec, err := readRunRecord(outDir)
	if err != nil || rec == nil {
		return err
	}
	last, err := lastRunRow(outDir, rec.ID)
	if err != nil {
		return err
	}
	if last != nil && last.Status == "completed" {
		return nil
	}
	return fmt.Errorf("%w: run %s is unfinished: continue it with --resume %s, or remove %s and %s to start over", ErrRunUnfinished, rec.ID, rec.ID, outDir, workDir)
}

// checkWorkDirEmpty refuses a new run into a work directory that holds anything.
func checkWorkDirEmpty(workDir string) error {
	entries, err := os.ReadDir(workDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the work directory %s: %w", workDir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s holds %d entries; a run starts into an empty directory, so remove it, or continue a run in it with --resume", ErrWorkDirNotEmpty, workDir, len(entries))
	}
	return nil
}

// checkResume finds the run to resume in outDir and checks that nothing it was
// begun over has changed: the oracle's head, the base commit and the sample size.
// It returns the run's record. A run that completed comes back as ErrRunCompleted,
// with the row it ended with. The integration build is checked once it has been
// made, by checkResumeBuild. Nothing is read or written beyond the run's own files.
func checkResume(outDir, id string, walk *Walk, sampleSize int) (runRecord, ReplayRun, error) {
	rec, err := readRunRecord(outDir)
	switch {
	case err != nil:
		return runRecord{}, ReplayRun{}, err
	case rec == nil:
		return runRecord{}, ReplayRun{}, fmt.Errorf("%w: %s holds no run, so there is no run %s to resume", ErrResumeRefused, outDir, id)
	case rec.ID != id:
		return runRecord{}, ReplayRun{}, fmt.Errorf("%w: %s holds run %s, not run %s", ErrResumeRefused, outDir, rec.ID, id)
	}

	last, err := lastRunRow(outDir, id)
	if err != nil {
		return runRecord{}, ReplayRun{}, err
	}
	if last != nil && last.Status == "completed" {
		return runRecord{}, *last, fmt.Errorf("%w: run %s ended completed, and nothing is applied twice", ErrRunCompleted, id)
	}

	for _, c := range []struct{ key, recorded, now string }{
		{"oracle_head", rec.OracleHead, walk.Head().Hash},
		{"base_commit", rec.BaseCommit, walk.Base().Hash},
		{"sample_size", fmt.Sprint(rec.SampleSize), fmt.Sprint(sampleSize)},
	} {
		if c.recorded != c.now {
			return runRecord{}, ReplayRun{}, fmt.Errorf("%w: run %s was begun with %s %s, and it is %s now", ErrResumeRefused, id, c.key, c.recorded, c.now)
		}
	}
	return *rec, ReplayRun{}, nil
}

// checkResumeBuild checks the integration build a resume made against the one the
// run began with. A run measures one build, so a resume over another is refused.
func checkResumeBuild(rec runRecord, sha string) error {
	if rec.IntegrationSHA != sha {
		return fmt.Errorf("%w: run %s was begun with integration_sha %s, and the integration ref is at %s now", ErrResumeRefused, rec.ID, rec.IntegrationSHA, sha)
	}
	return nil
}
