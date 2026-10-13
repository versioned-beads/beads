package translate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
)

// The two bd flags Execute pins on every action it runs.
const (
	// commitModeFlag makes each write command its own Dolt commit. bd's other modes
	// batch writes or defer them, and then a head before a command and a head after
	// it would not bracket that command alone.
	commitModeFlag = "--dolt-auto-commit=on"
	// actorFlag is the start of the flag that names who a command runs as. Every
	// version row records that identity, so the harness supplies it and checks it.
	actorFlag = "--actor="
)

// ActionResult is what one executed action did to the work clone's history, in the
// terms version accounting needs. The unit is one bd command: the Dolt head before
// it, the head after it, who it ran as and when.
type ActionResult struct {
	Kind  ActionKind
	Issue string
	// Actor is the identity the command ran as.
	Actor string
	// HeadBefore and HeadAfter are the work clone's head commits around the
	// command. A command that wrote nothing leaves them equal.
	HeadBefore string
	HeadAfter  string
	// Start and End bracket the bd process by this host's clock, in UTC. An action
	// that spawned no process has neither.
	Start time.Time
	End   time.Time
}

// Execute is ExecuteWith for an action that is to be accounted for. It pins the
// commit mode to on and the actor to actor, so that each effective write is exactly
// one Dolt commit by an identity the caller knows, and it returns the interval of
// the work clone's history the action covers: dataDir's head before the command and
// after it, with the times the command ran between.
//
// The two flags go before the first "--", where bd reads flags, or at the end of the
// arguments when there is none. The rest of the arguments reach bd in the order the
// planner gave them, the subcommand first.
//
// A bd that refuses the action is an *ExecError, as it is for ExecuteWith, and the
// result returned with it still names the action and the heads it ran between. A
// no-op action runs no bd and reports the head it found as both ends. An action that
// is to run needs an actor: an empty one is an error, because a version row without
// the identity it was made by could not be checked.
func Execute(ctx context.Context, bdPath, workDir, dataDir string, action Action, actor string) (ActionResult, error) {
	res := ActionResult{Kind: action.Kind, Issue: action.Issue, Actor: actor}
	before, err := dataHead(ctx, dataDir)
	if err != nil {
		return res, fmt.Errorf("execute: %w", err)
	}
	res.HeadBefore, res.HeadAfter = before, before
	if action.Kind == KindNoop {
		return res, nil
	}
	if actor == "" {
		return res, errors.New("execute: no actor: every action is run as an identity the harness supplies")
	}

	action.Argv = pinFlags(action.Argv, actor)
	res.Start = time.Now().UTC()
	runErr := ExecuteWith(ctx, bdPath, workDir, action)
	res.End = time.Now().UTC()

	after, headErr := dataHead(ctx, dataDir)
	switch {
	case runErr != nil:
		// The refusal is what the caller needs to hear. Where the head is after it is
		// context, and an unreadable head does not replace it.
		if headErr == nil {
			res.HeadAfter = after
		}
		return res, runErr
	case headErr != nil:
		return res, fmt.Errorf("execute: %w", headErr)
	}
	res.HeadAfter = after
	return res, nil
}

// pinFlags returns argv with the commit mode and the actor added ahead of its first
// "--", or at its end when it has none. argv itself is not changed.
func pinFlags(argv []string, actor string) []string {
	at := len(argv)
	for i, arg := range argv {
		if arg == "--" {
			at = i
			break
		}
	}
	pinned := make([]string, 0, len(argv)+2)
	pinned = append(pinned, argv[:at]...)
	pinned = append(pinned, commitModeFlag, actorFlag+actor)
	return append(pinned, argv[at:]...)
}

// dataHead is the head commit of the dolt database in dataDir, as dolt names it.
func dataHead(ctx context.Context, dataDir string) (string, error) {
	_, rows, err := doltcli.Query(ctx, dataDir, "SELECT hashof('HEAD')")
	if err != nil {
		return "", fmt.Errorf("head commit of %s: %w", dataDir, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text == "" {
		return "", fmt.Errorf("head commit of %s: dolt returned no head", dataDir)
	}
	return rows[0][0].Text, nil
}
