package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// The files a run keeps in its output directory. run.json and journal.jsonl are
// the run's own bookkeeping, see runrecord.go and journal.go; the others are its
// results.
const (
	fileRunRecord  = "run.json"
	fileJournal    = "journal.jsonl"
	fileRuns       = "replay_runs.jsonl"
	fileResults    = "commit_replay_results.jsonl"
	fileMismatches = "mismatches.jsonl"
	fileGaps       = "coverage_gaps.jsonl"
	fileSummary    = "summary.json"
)

// Store is a dependency-free, append-only JSON-Lines persistence layer for the
// ERD's entities and the run's coverage-gap rows, which also holds the run's
// summary. JSONL rather than a SQL engine because NFR1/NFR2 forbid ever opening
// a write-capable connection to the shared Dolt server, and a SQL-engine choice
// here could be mistaken for a step toward one.
type Store struct {
	dir string
}

// NewStore creates dir (including any missing parents) if needed and
// returns a Store that writes into it.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating store directory %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) appendJSONLine(filename string, v any) error {
	return s.appendLine(filename, v, false)
}

// appendLine appends v to the file as one JSON line. With durable it does not
// return until the line is on disk, which is what a journal entry needs: the line
// is there before what it announces happens.
func (s *Store) appendLine(filename string, v any, durable bool) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling %s entry: %w", filename, err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- filename is always one of this file's own *.jsonl literals, never external input
	if err != nil {
		return fmt.Errorf("opening %s: %w", filename, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing %s: %w", filename, err)
	}
	if durable {
		if err := f.Sync(); err != nil {
			return fmt.Errorf("syncing %s: %w", filename, err)
		}
	}
	return nil
}

func (s *Store) WriteReplayRun(run ReplayRun) error {
	return s.appendJSONLine(fileRuns, run)
}

func (s *Store) WriteCommitReplayResult(crr CommitReplayResult) error {
	return s.appendJSONLine(fileResults, crr)
}

func (s *Store) WriteMismatch(mm Mismatch) error {
	return s.appendJSONLine(fileMismatches, mm)
}

func (s *Store) WriteMetricSample(ms MetricSample) error {
	return s.appendJSONLine("metric_samples.jsonl", ms)
}

// WriteCoverageGap appends one (step, table) coverage-gap row.
func (s *Store) WriteCoverageGap(row CoverageGapRow) error {
	return s.appendJSONLine(fileGaps, row)
}

// WriteSummary writes the run's summary as summary.json, replacing an earlier one.
func (s *Store) WriteSummary(sum Summary) error {
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling the summary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, fileSummary), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing summary.json: %w", err)
	}
	return nil
}

// path is where the store keeps the named file.
func (s *Store) path(filename string) string { return filepath.Join(s.dir, filename) }

// writeJournal appends an entry to the run's journal and returns once it is on
// disk.
func (s *Store) writeJournal(e journalEntry) error {
	return s.appendLine(fileJournal, e, true)
}

// syncRows forces the files a step's rows go to onto disk. The journal calls a
// step finished only after this, so that a finished step never lacks a row.
func (s *Store) syncRows() error {
	for _, name := range []string{fileResults, fileMismatches, fileGaps} {
		if err := syncFile(s.path(name)); err != nil {
			return err
		}
	}
	return nil
}
