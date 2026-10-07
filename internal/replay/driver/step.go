package driver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/replay/compare"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
)

// stepEnv is everything one step needs from outside the process: the oracle's
// history, the work clone and the bd under test. The loop calls the real one;
// the tests call one with nothing behind it.
type stepEnv interface {
	// Plan reads the oracle's change from one commit to another and returns what
	// replays it. Nothing has run when it returns.
	Plan(ctx context.Context, from, to string) (*translate.StepPlan, error)
	// OracleView is the oracle's view of an issue as of ref; nil when it had no row.
	OracleView(ctx context.Context, ref, issue string) (*oracle.View, error)
	// Execute runs one action through the bd under test. A bd that refuses it
	// returns a *translate.ExecError; any other error is a failure of the harness.
	Execute(ctx context.Context, action translate.Action) error
	// WorkHead is the work clone's current head commit.
	WorkHead(ctx context.Context) (string, error)
	// CandidateView is the work clone's view of an issue as of ref, read the same
	// way as the oracle's, so the two sides are typed identically.
	CandidateView(ctx context.Context, ref, issue string) (*oracle.View, error)
}

// quarantined maps an issue to the commit of the step that quarantined it. An
// issue is quarantined when a step could not be replayed for it, because its
// work-clone state is then no longer the oracle's, and replaying or comparing
// anything more for it would only report the harness's own divergence.
type quarantined map[string]string

// note quarantines the issue of a result if the result is what quarantines one: a
// step that could not be replayed for the issue, or bd's refusal of an action for
// it. An issue that is held keeps the commit it was first held at. The step loop
// notes every result it makes, and a resume notes the results the run had already
// written, so that both arrive at the same set.
func (q quarantined) note(res CommitReplayResult) {
	switch res.Verdict {
	case VerdictUntranslatable, VerdictRejected:
		if _, held := q[res.IssueID]; !held {
			q[res.IssueID] = res.SourceCommit
		}
	}
}

// issueOutcome is what one step came to for one issue.
type issueOutcome struct {
	// Result is the row for the (step, issue); RunID is left for the loop to set.
	Result CommitReplayResult
	// Mismatch is the comparison's stored context, set for a mismatch and for an
	// uncomparable pair.
	Mismatch *compare.Mismatch
	// WriteLatency is the time spent running the issue's actions.
	WriteLatency time.Duration
}

// stepOutcome is what replaying one step came to.
type stepOutcome struct {
	// Issues has one outcome per issue the step touched, sorted by issue id.
	Issues []issueOutcome
	// Gaps are the tables the step changed that the replay does not cover.
	Gaps []translate.CoverageGap
	// Derived are the tables bd rewrites on its own that the step changed.
	Derived []string
	// Skew is the schema skew the step's comparisons saw.
	Skew compare.Skew
}

// mutationKind is what a step did to one issue: the action kinds, sorted,
// de-duplicated and joined with +. A merge step prefixes merge:. A step that
// spans commits is net, never a single kind, because it stands for several
// commits' changes at once.
func mutationKind(st Step, actions []translate.Action) string {
	if st.Net {
		return "net"
	}
	set := map[string]bool{}
	for _, a := range actions {
		if a.Kind != translate.KindNoop {
			set[a.Kind.String()] = true
		}
	}
	kinds := make([]string, 0, len(set))
	for k := range set {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	joined := strings.Join(kinds, "+")
	switch {
	case st.Merge && joined == "":
		return "merge"
	case st.Merge:
		return "merge:" + joined
	}
	return joined
}

// replayStep replays one step and compares it, in the order the isolation check
// fixes: the plan is read, then the oracle's view of every issue that will be
// compared, and only then is anything written to the work clone, so a read of
// the oracle can be neither affected by nor blamed on the replay. The work
// clone's views are read last, the same way.
//
// A step is atomic per issue. An issue the translator could not express is
// untranslatable and runs nothing; an issue bd refused an action for is
// rejected, and none of its later actions in the step run; either way it joins q.
// Neither stops the step, nor the run. A failure of the harness itself, such as
// bd not starting, does, and leaves q as it was.
func replayStep(ctx context.Context, env stepEnv, q quarantined, st Step) (*stepOutcome, error) {
	plan, err := env.Plan(ctx, st.From.Hash, st.To.Hash)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	actionsBy := map[string][]translate.Action{}
	touched := map[string]bool{}
	for _, a := range plan.Actions {
		if a.Kind == translate.KindNoop {
			continue
		}
		actionsBy[a.Issue] = append(actionsBy[a.Issue], a)
		touched[a.Issue] = true
	}
	untranslatable := map[string]*translate.Untranslatable{}
	for _, u := range plan.Untranslatable {
		untranslatable[u.Issue] = u
		touched[u.Issue] = true
	}
	ids := make([]string, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := &stepOutcome{Gaps: plan.Gaps, Derived: plan.Derived, Skew: compare.Skew{}}
	outcomes := make(map[string]*issueOutcome, len(ids))
	var runnable []string
	for _, id := range ids {
		oc := &issueOutcome{Result: CommitReplayResult{
			FromCommit:   st.From.Hash,
			SourceCommit: st.To.Hash,
			IssueID:      id,
			MutationKind: mutationKind(st, actionsBy[id]),
		}}
		outcomes[id] = oc
		switch since, held := q[id]; {
		case held:
			oc.Result.Verdict = VerdictSkippedQuarantined
			oc.Result.Detail = &ResultDetail{QuarantinedAt: since}
		case untranslatable[id] != nil:
			u := untranslatable[id]
			oc.Result.Verdict = VerdictUntranslatable
			oc.Result.Detail = &ResultDetail{Columns: copyStrings(u.Columns), Reasons: copyStrings(u.Reasons)}
		default:
			runnable = append(runnable, id)
		}
	}

	oracleViews := make(map[string]*oracle.View, len(runnable))
	for _, id := range runnable {
		v, err := env.OracleView(ctx, st.To.Hash, id)
		if err != nil {
			return nil, fmt.Errorf("read the oracle's %s at %s: %w", id, st.To.Hash, err)
		}
		oracleViews[id] = v
	}

	runs := make(map[string]bool, len(runnable))
	for _, id := range runnable {
		runs[id] = true
	}
	rejected := map[string]*ResultDetail{}
	for _, a := range plan.Actions {
		if a.Kind == translate.KindNoop || !runs[a.Issue] || rejected[a.Issue] != nil {
			continue
		}
		start := time.Now()
		err := env.Execute(ctx, a)
		outcomes[a.Issue].WriteLatency += time.Since(start)
		if err == nil {
			continue
		}
		// A bd that was interrupted along with the run exits non-zero too, and that is
		// not a refusal: the run is stopping, and the step is redone on a resume.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("run %s for %s: %w", strings.Join(a.Argv, " "), a.Issue, ctxErr)
		}
		var refused *translate.ExecError
		if !errors.As(err, &refused) {
			return nil, fmt.Errorf("run %s for %s: %w", strings.Join(a.Argv, " "), a.Issue, err)
		}
		rejected[a.Issue] = &ResultDetail{Argv: refused.Argv, ExitCode: refused.ExitCode, Output: refused.Output}
	}

	var compared []string
	for _, id := range runnable {
		if d := rejected[id]; d != nil {
			outcomes[id].Result.Verdict = VerdictRejected
			outcomes[id].Result.Detail = d
			continue
		}
		compared = append(compared, id)
	}
	if len(compared) > 0 {
		head, err := env.WorkHead(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the work clone's head: %w", err)
		}
		for _, id := range compared {
			cand, err := env.CandidateView(ctx, head, id)
			if err != nil {
				return nil, fmt.Errorf("read the work clone's %s at %s: %w", id, head, err)
			}
			res, skew, err := compare.CompareViews(oracleViews[id], cand)
			if err != nil {
				return nil, fmt.Errorf("compare %s at %s: %w", id, st.To.Hash, err)
			}
			mergeSkew(out.Skew, skew)
			oc := outcomes[id]
			oc.Result.Matched = res.Matched
			oc.Result.OracleHash = res.OracleHash
			oc.Result.CandidateHash = res.CandidateHash
			switch {
			case res.Matched:
				oc.Result.Verdict = VerdictMatched
			case res.Uncomparable():
				oc.Result.Verdict = VerdictUncomparable
				oc.Mismatch = res.Mismatch
			default:
				oc.Result.Verdict = VerdictMismatch
				oc.Mismatch = res.Mismatch
			}
		}
	}

	for _, id := range ids {
		out.Issues = append(out.Issues, *outcomes[id])
		q.note(outcomes[id].Result)
	}
	return out, nil
}

// copyStrings copies s, keeping nil as nil, so a row never shares a slice with the plan.
func copyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return append([]string(nil), s...)
}

// mergeSkew adds src's skew to dst: per table, the columns either side lacked.
func mergeSkew(dst, src compare.Skew) {
	for table, s := range src {
		cur := dst[table]
		cur.OracleOnly = unionSorted(cur.OracleOnly, s.OracleOnly)
		cur.CandidateOnly = unionSorted(cur.CandidateOnly, s.CandidateOnly)
		dst[table] = cur
	}
}

// unionSorted returns the sorted union of two string lists, nil when both are empty.
func unionSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		set[s] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
