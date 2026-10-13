package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/versions"
	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// The names of the two readers a step reads the work clone with. The row reader is
// the one every run has always had; the versioned reader reads what the product
// stored in issue_versions.
const (
	readerRow       = "row"
	readerVersioned = "versioned"
)

// CandidateReader is one way of reading what the build under test left in the work
// clone for an issue. The driver defines the interface and reads every issue it
// compares with each reader the environment offers, then holds what each one found
// to the oracle on an axis of its own.
type CandidateReader interface {
	// Name is the axis the reader stands for: "row" or "versioned".
	Name() string
	// Read returns what the reader finds for the issue as of ref in the database at
	// dir. A reader that cannot read returns an error, which fails the step: it is
	// never a verdict about the build.
	Read(ctx context.Context, dir, issueID, ref string) (State, error)
}

// State is what a CandidateReader found; each reader sets the member that is its
// own.
type State struct {
	// View is the issue's rows, from the row reader. It is nil when the issue has no
	// row at the ref.
	View *oracle.View
	// Version is what the store holds for the issue, from the versioned reader.
	Version *versions.Reading
	// NotRun says why a reader that was asked did not look. It is one of the reasons
	// the version axis knows, and it is never a match.
	NotRun string
}

// AxisVerdict is what one reader's state of an issue came to when it was held to
// the oracle's. It is a type of its own because the version axis has values a step
// verdict has not: a step is judged by its Verdict, which the axes feed, and these
// stay on the axes.
type AxisVerdict string

const (
	// AxisMatched: the reader's state is the oracle's.
	AxisMatched AxisVerdict = "matched"
	// AxisMismatch: it is not. A finding about the build under test.
	AxisMismatch AxisVerdict = "mismatch"
	// AxisUncomparable: a number in the pair is outside what can be compared
	// exactly, so neither a match nor a mismatch can be claimed.
	AxisUncomparable AxisVerdict = "uncomparable"
	// AxisLegacy is the version axis's report for an issue that is a legacy record:
	// its row was there before versioned history was turned on, and a store with the
	// write fence mints nothing for a change to one. The missing version is what the
	// store is meant to do, so it is not a mismatch, and nothing was compared, so it
	// is not a pass.
	AxisLegacy AxisVerdict = "version-legacy"
)

// The reasons the version axis gives for a read that was not compared. They are a
// closed set, and a reader that gives another fails the step.
const (
	// notRunEnableRefused: the product would not turn versioned history on over the
	// seeded rows, so nothing was versioned.
	notRunEnableRefused = "enable-refused"
	// notRunNoRow: the oracle has no row for the issue at the step, as when the step
	// deleted it, so there are no rows to project.
	notRunNoRow = "no-row"
	// notRunWisp: the issue is a wisp, or is marked no-history, and such an issue is
	// never versioned.
	notRunWisp = "wisp"
)

var notRunReasons = []string{notRunEnableRefused, notRunNoRow, notRunWisp}

// notRun is the version axis's value for a read that was not compared, and why.
func notRun(reason string) AxisVerdict { return AxisVerdict("version-not-run(" + reason + ")") }

// rowAxisValues and versionAxisValues are every value each axis can take, in the
// order a summary lists them.
var (
	rowAxisValues     = []AxisVerdict{AxisMatched, AxisMismatch, AxisUncomparable}
	versionAxisValues = []AxisVerdict{
		AxisMatched, AxisMismatch, AxisUncomparable, AxisLegacy,
		notRun(notRunEnableRefused), notRun(notRunNoRow), notRun(notRunWisp),
	}
)

// verdict is what an axis value says about the step. Only a finding, or a pair that
// could not be decided, makes a step worse: a legacy record and a read that did not
// run leave the step to the axis that did compare, and are counted on their own
// axis, where they cannot be taken for a pass.
func (v AxisVerdict) verdict() Verdict {
	switch v {
	case AxisMismatch:
		return VerdictMismatch
	case AxisUncomparable:
		return VerdictUncomparable
	}
	return VerdictMatched
}

// worse returns the more serious of two verdicts: a mismatch, then a pair that could
// not be compared, then a match.
func worse(a, b Verdict) Verdict {
	if seriousness(b) > seriousness(a) {
		return b
	}
	return a
}

func seriousness(v Verdict) int {
	switch v {
	case VerdictMismatch:
		return 2
	case VerdictUncomparable:
		return 1
	}
	return 0
}

// rowAxis is what a comparison of two views came to, as an axis value.
func rowAxis(res compare.Result) AxisVerdict {
	switch {
	case res.Matched:
		return AxisMatched
	case res.Uncomparable():
		return AxisUncomparable
	}
	return AxisMismatch
}

// rowReader reads the work clone's rows of an issue the way the oracle's are read,
// so the two sides are typed identically. oracle.ReadView takes the ref before the
// issue; the seam takes the issue first.
type rowReader struct{}

func (rowReader) Name() string { return readerRow }

func (rowReader) Read(ctx context.Context, dir, issueID, ref string) (State, error) {
	view, err := oracle.ReadView(ctx, dir, ref, issueID)
	if err != nil {
		return State{}, err
	}
	return State{View: view}, nil
}

// versionedReader reads the newest version the store holds for an issue. versions.Read
// takes the ref before the issue; the seam takes the issue first.
type versionedReader struct{}

func (versionedReader) Name() string { return readerVersioned }

func (versionedReader) Read(ctx context.Context, dir, issueID, ref string) (State, error) {
	reading, err := versions.Read(ctx, dir, ref, issueID)
	if err != nil {
		return State{}, err
	}
	return State{Version: reading}, nil
}

// notRunReader stands in for the versioned reader in a run that has no versions to
// read. It looks at nothing, and says why.
type notRunReader struct{ reason string }

func (notRunReader) Name() string { return readerVersioned }

func (r notRunReader) Read(context.Context, string, string, string) (State, error) {
	return State{NotRun: r.reason}, nil
}

// errNoRowReader reports an environment that offers no row reader. Every step is
// held to the oracle on its rows, so there is nothing to run without one.
var errNoRowReader = errors.New("the environment offers no row reader")

// readerNamed returns the reader that stands for the axis called name.
func readerNamed(readers []CandidateReader, name string) (CandidateReader, bool) {
	for _, r := range readers {
		if r.Name() == name {
			return r, true
		}
	}
	return nil, false
}

// checkReaders refuses a set of readers the step cannot make sense of: a name that is
// not an axis, a name given twice, or no row reader. A reader whose answer is not
// used would be paid for and never heard.
func checkReaders(readers []CandidateReader) error {
	seen := map[string]bool{}
	for _, r := range readers {
		name := r.Name()
		if name != readerRow && name != readerVersioned {
			return fmt.Errorf("the environment offers a reader named %q; the seam names %q and %q", name, readerRow, readerVersioned)
		}
		if seen[name] {
			return fmt.Errorf("the environment offers two readers named %q", name)
		}
		seen[name] = true
	}
	if !seen[readerRow] {
		return errNoRowReader
	}
	return nil
}

// rowView is the work clone's view of an issue as of ref, read with the row reader.
func rowView(ctx context.Context, env stepEnv, ref, issueID string) (*oracle.View, error) {
	reader, ok := readerNamed(env.Readers(), readerRow)
	if !ok {
		return nil, errNoRowReader
	}
	state, err := reader.Read(ctx, env.WorkDataDir(), issueID, ref)
	if err != nil {
		return nil, err
	}
	return state.View, nil
}

// readCandidate reads one issue of the work clone with every reader, in the order
// the environment lists them, and returns what each found by name. dir is the work
// clone's database and ref the head the step left it at.
func readCandidate(ctx context.Context, readers []CandidateReader, dir, issueID, ref string) (map[string]State, error) {
	states := make(map[string]State, len(readers))
	for _, r := range readers {
		state, err := r.Read(ctx, dir, issueID, ref)
		if err != nil {
			return nil, fmt.Errorf("read the work clone's %s at %s with the %s reader: %w", issueID, ref, r.Name(), err)
		}
		states[r.Name()] = state
	}
	return states, nil
}

// versionAxis holds what the versioned reader found to the oracle's rows. want is
// the oracle's view of the issue at the step, nil when the oracle has no row.
//
// The questions are asked in an order that matters. A reader that did not run has
// nothing to compare, whatever the oracle holds. With no oracle row there is nothing
// to project. An issue the oracle's flags make a wisp is never versioned, so any
// version found for it is not held to anything. Past those, the issue has a stored
// version, or it does not: a legacy record is meant to have none, and any other
// issue without one is a record-level mismatch.
//
// A stored version whose document cannot be read is an error and not a verdict: it
// fails the step and is told apart from a document that was read and differs.
func versionAxis(want *oracle.View, state State) (AxisVerdict, *compare.Mismatch, error) {
	if state.NotRun != "" {
		for _, reason := range notRunReasons {
			if state.NotRun == reason {
				return notRun(reason), nil, nil
			}
		}
		return "", nil, fmt.Errorf("the versioned reader did not run, for a reason the driver does not know: %q", state.NotRun)
	}
	if want == nil {
		return notRun(notRunNoRow), nil, nil
	}
	projected, err := versions.Project(want)
	if err != nil {
		return "", nil, fmt.Errorf("project the oracle's rows: %w", err)
	}
	if issueops.IsWisp(projected) {
		return notRun(notRunWisp), nil, nil
	}
	if state.Version == nil {
		return "", nil, errors.New("the versioned reader returned neither a version nor a reason it did not run")
	}

	var stored *types.Issue
	switch record := state.Version.Record; {
	case record != nil:
		if stored, err = record.Issue(); err != nil {
			return "", nil, err
		}
	case state.Version.Legacy:
		return AxisLegacy, nil, nil
	}
	res, err := compare.CompareDurable(projected, stored)
	if err != nil {
		return "", nil, err
	}
	axis := rowAxis(res)
	if axis == AxisMatched {
		return axis, nil, nil
	}
	return axis, res.Mismatch, nil
}
