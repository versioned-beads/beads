package driver

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/steveyegge/beads/internal/replay/compare"
)

// The events a run's journal records for a step.
const (
	journalStarted  = "started"
	journalFinished = "finished"
)

// journalEntry is one line of journal.jsonl, the run's write-ahead log. A step
// writes a started entry, forced to disk, before it does anything to the work
// clone, and a finished entry once its rows are written. A run that stops with a
// started entry and no finished one for it stopped inside that step.
type journalEntry struct {
	Event string `json:"event"`
	Step  int    `json:"step"`

	// From, To and WorkHeadBefore are on a started entry: the oracle commits the
	// step goes between, and the head of the work clone's database before it. The
	// head is what a resume rewinds the work clone to.
	From           string `json:"from,omitempty"`
	To             string `json:"to,omitempty"`
	WorkHeadBefore string `json:"work_head_before,omitempty"`

	// Derived and SchemaSkew are on a finished entry: what the run had counted
	// through this step that no stored row says. A resume starts from the last
	// ones.
	Derived    map[string]int `json:"derived,omitempty"`
	SchemaSkew compare.Skew   `json:"schema_skew,omitempty"`
}

// journalState is what a journal says about how far its run got.
type journalState struct {
	// finished is the number of steps the journal says are done, which are steps 0
	// to finished-1; the run goes on from step finished.
	finished int
	// last is the entry that finished step finished-1, with the counts as they
	// stood after it. It is nil when no step finished.
	last *journalEntry
	// open is the started entry of a step that was not finished, nil when the run
	// stopped between steps. When a step was started more than once, a redo after
	// a resume, it is the latest.
	open *journalEntry
}

// readJournal reads the journal of the run in outDir and checks it against the
// steps the run is made of. A journal that is not there is one with no entries. An
// entry that does not make sense, or a step the oracle does not have, is an error:
// a resume that went by such a journal could not tell what had been done.
func readJournal(outDir string, steps []Step) (journalState, error) {
	var js journalState
	lines, _, err := readJSONLines(filepath.Join(outDir, fileJournal))
	if err != nil {
		return js, fmt.Errorf("reading %s: %w", fileJournal, err)
	}
	for i, line := range lines {
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return journalState{}, fmt.Errorf("%s line %d: %w", fileJournal, i+1, err)
		}
		switch e.Event {
		case journalStarted:
			if e.Step != js.finished || e.Step >= len(steps) {
				return journalState{}, fmt.Errorf("%s line %d: step %d was started when the step to run is %d of %d", fileJournal, i+1, e.Step, js.finished, len(steps))
			}
			if want := steps[e.Step]; e.From != want.From.Hash || e.To != want.To.Hash {
				return journalState{}, fmt.Errorf("%s line %d: step %d goes from %s to %s, and the oracle's step %d goes from %s to %s", fileJournal, i+1, e.Step, e.From, e.To, e.Step, want.From.Hash, want.To.Hash)
			}
			js.open = &e
		case journalFinished:
			if js.open == nil || js.open.Step != e.Step {
				return journalState{}, fmt.Errorf("%s line %d: step %d was finished without being started", fileJournal, i+1, e.Step)
			}
			js.finished++
			js.last = &e
			js.open = nil
		default:
			return journalState{}, fmt.Errorf("%s line %d: unknown event %q", fileJournal, i+1, e.Event)
		}
	}
	return js, nil
}
