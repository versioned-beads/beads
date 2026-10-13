// B11b: the translator reports what each action did to the work clone's history.
//
// Version accounting needs the unit of account to be the action: one bd command,
// the Dolt head before it and the Dolt head after it, who it was run as and when.
// Execute returns exactly that, and pins the two bd flags that make it true.
//
//   - TestB11bOneCommitPerAction is the premise, run on real bd: every effective
//     write of every action kind lands as exactly one Dolt commit, so a head before
//     and a head after bracket one command and nothing else.
//   - TestB11bExecutePinsTheCommitMode holds the argument vector: the commit mode
//     and the actor are pinned before the argument terminator, whatever else the
//     action carries, and the action's own arguments reach bd untouched.
//   - TestB11bExecuteNoopSpawnsNothing and TestB11bExecuteARefusalKeepsTheHeads
//     hold the two ends: an action that does nothing reports one head, and a bd
//     that refuses still reports the heads it was run between.
package translate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/replaytest"
)

// b11bActor is the identity every action in these tests is run as.
const b11bActor = "replay-actor"

// b11bCommitsBetween counts the commits reachable from after and not from before.
func b11bCommitsBetween(t *testing.T, dataDir, before, after string) int {
	t.Helper()
	stmt := "SELECT COUNT(*) FROM dolt_log(" + doltcli.SQLQuote(after) + ", " + doltcli.SQLQuote("^"+before) + ")"
	_, rows, err := doltcli.Query(context.Background(), dataDir, stmt)
	if err != nil {
		t.Fatalf("counting the commits from %s to %s: %v", before, after, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("counting the commits from %s to %s returned %v", before, after, rows)
	}
	n, err := strconv.Atoi(rows[0][0].Text)
	if err != nil {
		t.Fatalf("the commit count %q is not a number: %v", rows[0][0].Text, err)
	}
	return n
}

// B11b.OneCommitPerAction. The premise the whole accounting stands on, on real bd
// with versioned history enabled and the commit mode and actor Execute pins: each
// action that changes something is exactly one Dolt commit. The actions are the
// shapes the planner emits plus the ones it has no kind for (label add and remove,
// claim, reopen), which a scenario can still run, so every write command a replayed
// step can come to is held to it.
func TestB11bOneCommitPerAction(t *testing.T) {
	requireBd(t)
	requireDolt(t)
	project := initBdProject(t, "one-commit")
	runBd(t, project, "config", "set", "versioned-history.enabled", "true")
	data := dataDir(t, project)
	bin := replaytest.BdBin(t)

	cases := []struct {
		name   string
		action Action
	}{
		{"create", Action{Kind: KindCreate, Issue: "t-1", Argv: []string{"create", "--id", "t-1", "--force", "--title", "One", "--type", "task"}}},
		{"create-second", Action{Kind: KindCreate, Issue: "t-2", Argv: []string{"create", "--id", "t-2", "--force", "--title", "Two", "--type", "task"}}},
		{"update", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--description", "first words", "--", "t-1"}}},
		{"claim", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--claim", "--", "t-1"}}},
		{"update-add-label", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--add-label", "foo", "--", "t-1"}}},
		{"update-remove-label", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--remove-label", "foo", "--", "t-1"}}},
		{"label-add", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"label", "add", "t-1", "bar"}}},
		{"label-remove", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"label", "remove", "t-1", "bar"}}},
		{"close", Action{Kind: KindClose, Issue: "t-1", Argv: []string{"close", "--reason", "done", "--", "t-1"}}},
		{"reopen", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"reopen", "t-1"}}},
		{"dep-add", Action{Kind: KindDepAdd, Issue: "t-1", Argv: []string{"dep", "add", "--", "t-1", "t-2"}}},
		{"dep-remove", Action{Kind: KindDepRemove, Issue: "t-1", Argv: []string{"dep", "remove", "--", "t-1", "t-2"}}},
		{"composite-update", Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--priority", "1", "--title", "Renamed", "--description", "again", "--", "t-1"}}},
		{"delete", Action{Kind: KindDelete, Issue: "t-2", Argv: []string{"delete", "--force", "--", "t-2"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := headCommit(t, data)
			res, err := Execute(context.Background(), bin, project, data, c.action, b11bActor)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			after := headCommit(t, data)
			if res.HeadBefore != before {
				t.Errorf("HeadBefore = %q, want the head before the action, %q", res.HeadBefore, before)
			}
			if res.HeadAfter != after {
				t.Errorf("HeadAfter = %q, want the head after the action, %q", res.HeadAfter, after)
			}
			if res.HeadBefore == res.HeadAfter {
				t.Errorf("the action left the head where it was, %q", res.HeadAfter)
			}
			if n := b11bCommitsBetween(t, data, before, after); n != 1 {
				t.Errorf("the action is %d Dolt commits, want exactly 1", n)
			}
			if res.Kind != c.action.Kind || res.Issue != c.action.Issue {
				t.Errorf("the result names %v %q, want %v %q", res.Kind, res.Issue, c.action.Kind, c.action.Issue)
			}
			if res.Actor != b11bActor {
				t.Errorf("Actor = %q, want %q", res.Actor, b11bActor)
			}
			if res.Start.IsZero() || res.End.Before(res.Start) {
				t.Errorf("the action ran from %v to %v, want a start and an end no earlier than it", res.Start, res.End)
			}
		})
	}
}

// b11bArgvStandIn is a bd stand-in that writes the arguments of its last call, one
// per line, into a log, and exits with the given code.
func b11bArgvStandIn(t *testing.T, exit int) (bin, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "argv.log")
	bin = standInBd(t, "for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '"+log+"'\nexit "+strconv.Itoa(exit))
	return bin, log
}

// b11bLoggedArgv reads the arguments a stand-in logged.
func b11bLoggedArgv(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log) // #nosec G304 -- a log in a test directory
	if err != nil {
		t.Fatalf("the stand-in logged nothing: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// b11bWithout removes one occurrence of each flag from argv and fails the test if
// any of them is not there exactly once.
func b11bWithout(t *testing.T, argv []string, flags ...string) []string {
	t.Helper()
	rest := append([]string(nil), argv...)
	for _, flag := range flags {
		found := -1
		for i, arg := range rest {
			if arg != flag {
				continue
			}
			if found >= 0 {
				t.Fatalf("%s is in %q twice", flag, argv)
			}
			found = i
		}
		if found < 0 {
			t.Fatalf("%s is not in %q", flag, argv)
		}
		rest = append(rest[:found], rest[found+1:]...)
	}
	return rest
}

// B11b.OneCommitPerAction, the argument vector. The commit mode is pinned to on,
// because the premise holds for it and bd's other modes batch writes or defer them;
// the actor is pinned so every version row carries the identity the harness
// supplied. Both go before the first terminator, where bd reads flags, or at the
// end when there is none, and the rest of the arguments arrive in the order and with
// the content the planner gave them, the subcommand first.
func TestB11bExecutePinsTheCommitMode(t *testing.T) {
	db := replaytest.NewDoltDB(t, "pins")
	mode := "--dolt-auto-commit=on"
	actor := "--actor=" + b11bActor

	cases := []struct {
		name string
		kind ActionKind
		argv []string
	}{
		{"update", KindUpdate, []string{"update", "--title", "A title", "--", "t-1"}},
		{"create-without-terminator", KindCreate, []string{"create", "--id", "t-1", "--force", "--title", "One"}},
		{"close", KindClose, []string{"close", "--reason", "done", "--", "t-1"}},
		{"dep-add", KindDepAdd, []string{"dep", "add", "--type", "blocks", "--", "t-1", "t-2"}},
		{"dep-remove", KindDepRemove, []string{"dep", "remove", "--", "t-1", "t-2"}},
		{"delete", KindDelete, []string{"delete", "--force", "--", "t-1"}},
		{"label-add-without-terminator", KindUpdate, []string{"label", "add", "t-1", "bar"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, log := b11bArgvStandIn(t, 0)
			action := Action{Kind: c.kind, Issue: "t-1", Argv: c.argv}
			if _, err := Execute(context.Background(), bin, t.TempDir(), db, action, b11bActor); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			got := b11bLoggedArgv(t, log)
			if rest := b11bWithout(t, got, mode, actor); !reflect.DeepEqual(rest, c.argv) {
				t.Errorf("bd was run with %q; without the pinned flags that is %q, want the action's own %q", got, rest, c.argv)
			}
			if got[0] != c.argv[0] {
				t.Errorf("the subcommand is %q, want %q first", got[0], c.argv[0])
			}
			terminator := -1
			for i, arg := range got {
				if arg == "--" {
					terminator = i
					break
				}
			}
			if terminator < 0 {
				tail := got[len(got)-2:]
				if !(tail[0] == mode && tail[1] == actor || tail[0] == actor && tail[1] == mode) {
					t.Errorf("with no terminator the pinned flags go at the end, and %q does not end with both", got)
				}
				return
			}
			for i, arg := range got {
				if (arg == mode || arg == actor) && i > terminator {
					t.Errorf("%s is word %d of %q, after the terminator at %d, where bd reads it as an argument", arg, i, got, terminator)
				}
			}
		})
	}
}

// A no-op action runs no bd at all and reports the database head it found, as both
// ends of the interval, so the ledger reads an empty delta from it.
func TestB11bExecuteNoopSpawnsNothing(t *testing.T) {
	db := replaytest.NewDoltDB(t, "noop")
	bin, log := b11bArgvStandIn(t, 0)
	head := headCommit(t, db)

	res, err := Execute(context.Background(), bin, t.TempDir(), db, Action{Kind: KindNoop}, b11bActor)
	if err != nil {
		t.Fatalf("Execute(noop): %v", err)
	}
	if _, statErr := os.Stat(log); statErr == nil {
		t.Errorf("a no-op action started bd: %q", readCalls(t, log))
	}
	if res.Kind != KindNoop {
		t.Errorf("Kind = %v, want the no-op kind", res.Kind)
	}
	if res.HeadBefore != head || res.HeadAfter != head {
		t.Errorf("heads = %q and %q, want both the database head, %q", res.HeadBefore, res.HeadAfter, head)
	}
}

// A bd that refuses the action is an *ExecError, as it is for ExecuteWith, and the
// result that comes back with it still names the action and the heads: a refusal
// leaves the clone where it was, and the caller can show that rather than assume it.
func TestB11bExecuteARefusalKeepsTheHeads(t *testing.T) {
	db := replaytest.NewDoltDB(t, "refusal")
	bin := standInBd(t, "echo 'refused' >&2\nexit 3")
	head := headCommit(t, db)
	action := Action{Kind: KindUpdate, Issue: "t-1", Argv: []string{"update", "--title", "x", "--", "t-1"}}

	res, err := Execute(context.Background(), bin, t.TempDir(), db, action, b11bActor)
	var refused *ExecError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v (%T), want an *ExecError", err, err)
	}
	if refused.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", refused.ExitCode)
	}
	if res.Kind != KindUpdate || res.Issue != "t-1" {
		t.Errorf("the result names %v %q, want %v %q", res.Kind, res.Issue, KindUpdate, "t-1")
	}
	if res.HeadBefore != head || res.HeadAfter != head {
		t.Errorf("heads = %q and %q, want both the unchanged head, %q", res.HeadBefore, res.HeadAfter, head)
	}
}
