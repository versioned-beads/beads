package driver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/replay/versions"
	"github.com/steveyegge/beads/internal/types"
)

// ---- the second reader, scripted ---------------------------------------------
//
// The B11a seam is exercised without dolt or bd: the row reader is the fake
// environment's own, and the versioned reader is one of the readers below. They
// answer from the fake's oracle views, so a test says only how the stored
// version differs from what the oracle's rows imply.

// b11aReaderNamed returns the reader an environment offers under a name, and
// fails the test when it offers none.
func b11aReaderNamed(t *testing.T, readers []CandidateReader, name string) CandidateReader {
	t.Helper()
	var names []string
	for _, r := range readers {
		if r.Name() == name {
			return r
		}
		names = append(names, r.Name())
	}
	t.Fatalf("no reader named %q among %q", name, names)
	return nil
}

// b11aReader is a reader a test scripts. It logs every read as
// "<name> <dir> <ref> <issue>", so a test can say which reads happened, in what
// order and with what arguments, and answers from its read function.
type b11aReader struct {
	env  *fakeEnv
	name string
	read func(dir, issueID, ref string) (State, error)
}

func (r b11aReader) Name() string { return r.name }

func (r b11aReader) Read(_ context.Context, dir, issueID, ref string) (State, error) {
	r.env.log("%s %s %s %s", r.name, dir, ref, issueID)
	return r.read(dir, issueID, ref)
}

// b11aView is the row view of an open task, the shape most tests share. More
// columns are added after the common ones and must not repeat them.
func b11aView(id, title string, more ...col) *oracle.View {
	cols := []col{
		txt("title", title), txt("status", "open"), txt("priority", "2"), txt("issue_type", "task"),
		txt("created_at", "2026-10-12 10:00:00"), txt("updated_at", "2026-10-12 10:00:00"),
	}
	return viewOf(id, append(cols, more...)...)
}

// b11aReading is a stored version of an issue holding the given document.
func b11aReading(issueID string, doc []byte) *versions.Reading {
	return &versions.Reading{Record: &versions.Record{
		IssueID: issueID, Revision: 1, Epoch: 1, DurableState: doc,
		ChangeActor: "replay", AttributionStatus: "claimed", ChangeAt: "2026-10-12 10:00:00.000000",
	}}
}

// b11aStored is a faithful versioned reader: for an issue it returns the document
// the product would have stored for the oracle's rows, after edit has changed it.
// With no edit the stored version is the oracle's state exactly. An issue the
// oracle has no row for has no stored version.
func b11aStored(env *fakeEnv, edit func(*types.Issue)) CandidateReader {
	return b11aReader{env: env, name: "versioned", read: func(_, issueID, _ string) (State, error) {
		view := env.oracle[issueID]
		if view == nil {
			return State{Version: &versions.Reading{}}, nil
		}
		issue, err := versions.Project(view)
		if err != nil {
			return State{}, err
		}
		if edit != nil {
			edit(issue)
		}
		// The bytes are not canonicalized: the comparison decodes them, so their
		// order is not what is under test, and a number the gate refuses must
		// reach it with its digits.
		doc, err := json.Marshal(issue)
		if err != nil {
			return State{}, err
		}
		return State{Version: b11aReading(issueID, doc)}, nil
	}}
}

// b11aFixed is a versioned reader that answers every read with the same state.
func b11aFixed(env *fakeEnv, state State) CandidateReader {
	return b11aReader{env: env, name: "versioned", read: func(_, _, _ string) (State, error) {
		return state, nil
	}}
}

// b11aByIssue is a versioned reader that answers an issue with the reader the
// test names for it, and every other with fallback.
func b11aByIssue(env *fakeEnv, fallback CandidateReader, byIssue map[string]CandidateReader) CandidateReader {
	return b11aReader{env: env, name: "versioned", read: func(dir, issueID, ref string) (State, error) {
		r := fallback
		if own, ok := byIssue[issueID]; ok {
			r = own
		}
		return r.Read(context.Background(), dir, issueID, ref)
	}}
}

// b11aEnv is an environment with one issue, x-1, updated by the step, whose oracle
// rows, work-clone rows and stored version all agree.
func b11aEnv() *fakeEnv {
	env := &fakeEnv{
		plan:      &translate.StepPlan{Actions: []translate.Action{action(translate.KindUpdate, "x-1", "update", "x-1")}},
		oracle:    map[string]*oracle.View{"x-1": b11aView("x-1", "T")},
		candidate: map[string]*oracle.View{"x-1": b11aView("x-1", "T")},
		head:      "workhead",
	}
	env.versioned = b11aStored(env, nil)
	return env
}

// b11aReplay replays the one step of an environment and returns x-1's outcome.
func b11aReplay(t *testing.T, env *fakeEnv) issueOutcome {
	t.Helper()
	out, err := replayStep(context.Background(), env, quarantined{}, stepFromTo("from", "to"))
	if err != nil {
		t.Fatalf("replayStep: %v", err)
	}
	return stepResult(t, out, "x-1")
}
