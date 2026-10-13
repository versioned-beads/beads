// Package translate turns a historical dolt_log commit step's row-diff into the
// equivalent bd CLI invocations and executes them against a working clone --
// never a hand-written row insert.
//
// Plan reads one step and returns every issue's actions in the order they must
// run, plus what the step changed that cannot be replayed (see StepPlan and the
// table policy). Classify is the same plan seen from one issue. ExecuteWith runs
// an action through a bd binary the caller names; nothing here finds bd on PATH.
// Execute is that run for an action to be accounted for: it pins the commit mode
// and the actor, and reports the work clone's history from before the action to
// after it.
package translate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// ActionKind identifies which bd CLI operation an Action represents.
type ActionKind int

const (
	KindCreate ActionKind = iota
	KindUpdate
	KindDelete
	KindDepAdd
	KindDepRemove
	KindClose
	KindNoop
)

// Action is one bd CLI invocation that replays a single historical mutation
// against a working clone. KindNoop carries no invocation at all.
type Action struct {
	Kind ActionKind
	// Issue is the issue the action belongs to. A dependency action belongs to the
	// issue that owns the edge: an edge is owned by its issue_id end, so an edge
	// pointing at an issue is planned under the issue that declares it, never
	// under the one it points at.
	Issue string
	Argv  []string
}

// fieldFlag pairs an issues-table column with the bd flag that sets it on
// create/update. Order is fixed so generated argv is deterministic.
type fieldFlag struct {
	column string
	flag   string
}

// settableFields lists every issues-table column this translator can express
// as a bd create/update flag. A changed column not in this list makes its issue
// untranslatable at that step (never a silently dropped mutation).
var settableFields = []fieldFlag{
	{"title", "--title"},
	{"description", "--description"},
	{"design", "--design"},
	{"acceptance_criteria", "--acceptance"},
	{"notes", "--notes"},
	{"priority", "--priority"},
	{"issue_type", "--type"},
	{"assignee", "--assignee"},
	{"external_ref", "--external-ref"},
	{"spec_id", "--spec-id"},
	{"estimated_minutes", "--estimate"},
	{"status", "--status"},
}

// ignoredColumns change as a side effect of any write (content_hash,
// updated_at, row_lock), as a side effect of a dependency-edge change
// specifically (is_blocked), or are dolt_commit_diff_issues' own range
// metadata rather than real issues-table columns (commit, commit_date) --
// confirmed empirically rather than assumed from the schema alone.
var ignoredColumns = map[string]bool{
	"content_hash": true,
	"updated_at":   true,
	"row_lock":     true,
	"is_blocked":   true,
	"commit":       true,
	"commit_date":  true,
}

// closingColumns are excluded from a modified-row diff once a close
// transition has already been captured as its own KindClose action --
// otherwise close_reason/closed_at/closed_by_session would spuriously
// surface as unsupported field changes on every close.
var closingColumns = map[string]bool{
	"status":            true,
	"close_reason":      true,
	"closed_at":         true,
	"closed_by_session": true,
}

// Classify is Plan seen from one issue: the actions that replay what the step
// from (exclusive) to to (inclusive) did to issueID, or a single KindNoop when
// nothing material changed for it. An issue the step changed in a way the
// translator cannot express is returned as an *Untranslatable error. It plans
// the whole step on every call, so a caller with many issues in one step should
// call Plan once.
//
// Coverage gaps and derived tables are not visible through this view: a caller
// that must record them uses Plan.
func Classify(ctx context.Context, dataDir, from, to, issueID string) ([]Action, error) {
	if err := issueops.ValidateRef(issueID); err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}
	p, err := Plan(ctx, dataDir, from, to)
	if err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}
	for _, u := range p.Untranslatable {
		if u.Issue == issueID {
			return nil, u
		}
	}
	actions := p.ActionsFor(issueID)
	if len(actions) == 0 {
		return []Action{{Kind: KindNoop}}, nil
	}
	return actions, nil
}

// ExecuteWith runs one Action's bd CLI invocation through the bd binary at
// bdPath, with workDir as its working directory. It never looks bd up on PATH:
// the code under test is the binary the caller names, not whichever bd happens
// to be first. A relative bdPath is taken relative to the caller's directory,
// because bd runs with workDir as its working directory, which would otherwise
// change what the path means. KindNoop returns immediately without spawning a
// process.
func ExecuteWith(ctx context.Context, bdPath, workDir string, action Action) error {
	if action.Kind == KindNoop {
		return nil
	}
	absBd, err := filepath.Abs(bdPath)
	if err != nil {
		return fmt.Errorf("execute: resolving %s: %w", bdPath, err)
	}
	cmd := exec.CommandContext(ctx, absBd, action.Argv...)
	cmd.Dir = workDir
	cmd.Env = doltcli.SanitizedEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return execFailure(action.Argv, err, out)
	}
	return nil
}
