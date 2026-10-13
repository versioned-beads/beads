package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
	"github.com/steveyegge/beads/internal/replay/versions"
)

// The version accounting: every command the replay runs is one Dolt commit by an
// identity the harness chose, and the history between the head before it and the head
// after it is held to the rules of the versions package, over Dolt's own diff of the
// work clone. A finding is a mismatch row of its own, filed under the issue it is
// about, and makes the row of the step it was found in a mismatch.

// replayActor is who a command of the replay runs as when the oracle has no one to
// name for it. Every version row a command mints is held to the actor the harness gave
// it, so one actor for all the commands that change an issue keeps the rows' attribution
// a fact the run can check.
const replayActor = "replay-actor"

// accountTolerance is how far outside the window of its command the clock of the store
// may put the change_at of a version row: the clock is the host's own, so what it
// allows for is the store's rounding and the time a process takes to start and stop.
const accountTolerance = 5 * time.Second

// accountedEnv is a step environment whose commands are accounted for. A step
// environment without it runs its actions plainly and is not held to the accounting.
type accountedEnv interface {
	// ExecuteAs runs one action through the bd under test as actor, with the commit
	// mode pinned so that the action is one Dolt commit, and returns the heads it ran
	// between and the interval it ran in. A bd that refuses the action returns a
	// *translate.ExecError, as stepEnv.Execute does.
	ExecuteAs(ctx context.Context, action translate.Action, actor string) (translate.ActionResult, error)
	// Delta is what the commits after from, up to and including to, did to the work
	// clone, by issue.
	Delta(ctx context.Context, from, to string) (versions.Delta, error)
	// Accounts reports whether there are versions to account for. A product that would
	// not turn versioned history on mints none.
	Accounts() bool
}

// actorFor is the identity an action runs as. A create runs as the issue's creator in
// the oracle, because the creator is a column the two sides compare and the actor of the
// create is the only way to set it; every other action runs as the replay's own.
func actorFor(a translate.Action, want *oracle.View) string {
	if a.Kind != translate.KindCreate || want == nil {
		return replayActor
	}
	if creator, ok := want.Issue.Cell("created_by"); ok && !creator.Null && creator.Text != "" {
		return creator.Text
	}
	return replayActor
}

// execute runs one action of a step. An environment that accounts for its commands runs
// it pinned, as the actor the oracle names for it; any other runs it as it always has.
func execute(ctx context.Context, env stepEnv, acct accountedEnv, a translate.Action, want *oracle.View) (translate.ActionResult, error) {
	if acct == nil {
		return translate.ActionResult{}, env.Execute(ctx, a)
	}
	return acct.ExecuteAs(ctx, a, actorFor(a, want))
}

// latencyOf is how long a command took: the interval it ran in when the run measured
// one, and otherwise the time the caller waited for it.
func latencyOf(res translate.ActionResult, waited time.Duration) time.Duration {
	if d := res.End.Sub(res.Start); !res.Start.IsZero() && d >= 0 {
		return d
	}
	return waited
}

// accountedFinding is one finding of the accounting. Issue is the issue it is about,
// which is not always the issue of the command it was found after.
type accountedFinding struct {
	Issue    string
	Category string
	Section  string
	Expected json.RawMessage
	Actual   json.RawMessage
	// fromState marks a version-state finding of the check that follows each command, as
	// against the one the step's own read makes of an issue's last version.
	fromState bool
}

// hostAccount is what the accounting found after the commands run for one issue, the
// host of the findings and of the counts.
type hostAccount struct {
	legacy, participating int
	gaps                  int
	removedMarked         bool
	found                 []accountedFinding
}

// stepAccount collects the accounting of one step as its actions run, and puts it into
// the step's outcomes once the step has been read.
type stepAccount struct {
	env      accountedEnv
	step     stepEnv
	readers  []CandidateReader
	versions bool
	hosts    map[string]*hostAccount
	// stated is the issues the check of stored states has found wrong in this step, of
	// which it reports the first finding only.
	stated map[string]bool
}

func newStepAccount(step stepEnv, env accountedEnv, readers []CandidateReader) *stepAccount {
	_, versioned := readerNamed(readers, readerVersioned)
	return &stepAccount{
		env: env, step: step, readers: readers, versions: versioned,
		hosts:  map[string]*hostAccount{},
		stated: map[string]bool{},
	}
}

func (s *stepAccount) host(issue string) *hostAccount {
	h := s.hosts[issue]
	if h == nil {
		h = &hostAccount{}
		s.hosts[issue] = h
	}
	return h
}

// after accounts for one action that ran: what it changed, held to the rules, and the
// state every issue it minted a version for was left in. An action that left the head
// where it was changed nothing.
func (s *stepAccount) after(ctx context.Context, res translate.ActionResult) error {
	if res.Kind == translate.KindNoop || res.HeadBefore == res.HeadAfter {
		return nil
	}
	delta, err := s.env.Delta(ctx, res.HeadBefore, res.HeadAfter)
	if err != nil {
		return fmt.Errorf("account for %s of %s between %s and %s: %w", res.Kind, res.Issue, res.HeadBefore, res.HeadAfter, err)
	}
	act := versions.Action{Kind: res.Kind.String(), Issue: res.Issue, Actor: res.Actor, Start: res.Start, End: res.End}
	found := versions.Evaluate(act, delta, versions.Options{Tolerance: accountTolerance})

	h := s.host(res.Issue)
	h.legacy += found.Legacy
	h.participating += found.Participating
	h.removedMarked = h.removedMarked || found.RemovedMarked
	for _, f := range found.Findings {
		if f.Category == versions.CategoryGap {
			h.gaps++
			continue
		}
		h.found = append(h.found, accountedFinding{
			Issue: f.Issue, Category: f.Category, Section: f.Sub,
			Expected: rawJSON(f.Expected), Actual: rawJSON(f.Actual),
		})
	}
	if !s.versions {
		return nil
	}
	return s.states(ctx, h, delta, res)
}

// states holds the version each issue was given by the action to the issue itself: the
// newest stored version is the projection of the issue's rows as the action left them.
// Nothing of the oracle is in it, so it sees a stored state that is wrong even when a
// later command in the step stores a right one.
//
// A legacy record is held to the write fence alone, and an issue the action minted no
// version for has none to hold; a missing one is the count check's finding.
func (s *stepAccount) states(ctx context.Context, h *hostAccount, delta versions.Delta, res translate.ActionResult) error {
	for _, i := range delta.Issues {
		if len(i.Rows) == 0 || !i.Exists || i.Wisp || delta.Legacy(i) || s.stated[i.ID] {
			continue
		}
		got, err := readCandidate(ctx, s.readers, s.step.WorkDataDir(), i.ID, res.HeadAfter)
		if err != nil {
			return err
		}
		axis, mismatch, err := versionAxis(got[readerRow].View, got[readerVersioned])
		if err != nil {
			return fmt.Errorf("hold the version stored for %s by %s to the issue as it was left: %w", i.ID, res.Kind, err)
		}
		if axis != AxisMismatch || mismatch == nil {
			continue
		}
		s.stated[i.ID] = true
		h.found = append(h.found, accountedFinding{
			Issue: i.ID, Category: mismatch.Category, Section: mismatch.Section,
			Expected: mismatch.ExpectedJSON, Actual: mismatch.ActualJSON, fromState: true,
		})
	}
	return nil
}

// apply puts the step's accounting into its outcomes. ids are the issues of the step,
// sorted; compared is the ones that were read.
//
// A finding about an issue the step read makes that issue's row a mismatch on the
// version axis. A finding about one it did not read makes the row of the issue whose
// command it was found after the mismatch, because a step with a finding in it is not
// a matched step. A finding of the write fence replaces what the single read of the
// issue made of its version: the issue is a legacy record and that read has nothing to
// say about a record, and the breach is the whole of what is wrong.
//
// The check of stored states gives way to the single read where both object to the same
// issue, so one fault is one finding. The single read's own objections are counted
// here too, in the row of the issue it was about.
func (s *stepAccount) apply(ids []string, outcomes map[string]*issueOutcome, compared map[string]bool) {
	fenced := map[string]bool{}
	for _, id := range ids {
		if h := s.hosts[id]; h != nil {
			for _, f := range h.found {
				if f.Category == versions.CategoryFence {
					fenced[f.Issue] = true
				}
			}
		}
	}
	for _, id := range ids {
		h := s.hosts[id]
		if h == nil {
			continue
		}
		host := outcomes[id]
		acc := host.accounting()
		acc.LegacyChanges += h.legacy
		acc.ParticipatingChanges += h.participating
		acc.RemovedMarked = acc.RemovedMarked || h.removedMarked
		acc.count(versions.CategoryGap, h.gaps)
		for _, f := range h.found {
			about := outcomes[f.Issue]
			if f.fromState && (fenced[f.Issue] || (compared[f.Issue] && about.objectsToVersion())) {
				continue
			}
			host.Accounted = append(host.Accounted, f)
			acc.count(f.Category, 1)
			target := about
			if !compared[f.Issue] {
				target = nil
				if compared[id] {
					target = host
				}
			}
			switch {
			case target == nil:
			case f.Category == versions.CategoryFence:
				target.replaceVersionObjection()
			default:
				target.objectToVersion()
			}
		}
		host.keepAccounting()
	}
	for _, id := range ids {
		if oc := outcomes[id]; compared[id] && oc.objectsToVersion() {
			oc.accounting().count(oc.VersionMismatch.Category, 1)
			oc.keepAccounting()
		}
	}
}

// objectsToVersion reports a version axis that is a mismatch because of the single
// read of the issue's last version.
func (oc *issueOutcome) objectsToVersion() bool {
	return oc != nil && oc.VersionMismatch != nil && oc.Result.VersionVerdict == AxisMismatch
}

// objectToVersion makes the row a mismatch on its version axis. A number the axis could
// not compare stays what it was, so that it is still counted as the hole it is.
func (oc *issueOutcome) objectToVersion() {
	if oc.Result.VersionVerdict != AxisUncomparable {
		oc.Result.VersionVerdict = AxisMismatch
	}
	oc.Result.Verdict = VerdictMismatch
	oc.Result.Matched = false
}

// replaceVersionObjection makes the row a mismatch on its version axis and takes the
// single read's objection to the issue out of it: the finding that replaces it is the
// row's.
func (oc *issueOutcome) replaceVersionObjection() {
	oc.VersionMismatch = nil
	oc.Result.VersionVerdict = AxisMismatch
	oc.Result.Verdict = VerdictMismatch
	oc.Result.Matched = false
}

// accounting is the row's accounting, made when the row first has something to keep.
func (oc *issueOutcome) accounting() *VersionAccounting {
	if oc.Result.Accounting == nil {
		oc.Result.Accounting = &VersionAccounting{}
	}
	return oc.Result.Accounting
}

// keepAccounting drops the row's accounting if nothing came of it.
func (oc *issueOutcome) keepAccounting() {
	if oc.Result.Accounting != nil && oc.Result.Accounting.empty() {
		oc.Result.Accounting = nil
	}
}

// count adds n findings of the category.
func (a *VersionAccounting) count(category string, n int) {
	if n == 0 {
		return
	}
	if a.Findings == nil {
		a.Findings = map[string]int{}
	}
	a.Findings[category] += n
}

// rawJSON is a document the accounting built, as a member of a row. An empty one is
// null, so that a row never holds a document that cannot be decoded.
func rawJSON(doc string) json.RawMessage {
	if doc == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(doc)
}

// findingCategories are the categories the summary counts findings under, in the order
// of the checks. version-state is the single read's category and the check that follows
// each command shares it; version-gap is the one that is a recorded fact and not a
// failure.
var findingCategories = []string{
	versions.CategoryFence, versions.CategoryCount, versions.CategoryChain, compare.CategoryVersionState,
	versions.CategoryAttribution, versions.CategoryEpoch, versions.CategoryTime, versions.CategorySurvival,
	versions.CategoryGap,
}

// The values of the summary's removed_marking. The product does not mark the versions
// of a deleted issue removed, so a run that watched for it and saw nothing says so,
// and a green run cannot be read as having covered the restriction that a removed
// version is read-only; a run that saw a mark says that instead.
const (
	removedMarkingNotInProduction = "not-in-production"
	removedMarkingObserved        = "observed"
)
