package versions

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// The categories of a finding Evaluate reports. The check of a stored version against
// an oracle's issue (version-state) is the compare package's and is not repeated here.
const (
	CategoryFence       = "version-fence"
	CategoryCount       = "version-count"
	CategoryChain       = "version-chain"
	CategoryAttribution = "version-attribution"
	CategoryEpoch       = "version-epoch"
	CategoryTime        = "version-time"
	CategorySurvival    = "version-survival"
	// CategoryGap is a real change that minted nothing for a cause the product's own
	// completeness test accepts. It is counted and never a mismatch.
	CategoryGap = "version-gap"
)

// The sub-cases of a finding: which way the rule was broken.
const (
	subRowMinted           = "row-minted"
	subPromoted            = "promoted"
	subRevisionMoved       = "revision-moved"
	subOverMint            = "over-mint"
	subUnderMint           = "under-mint"
	subPhantom             = "phantom"
	subRevisionGap         = "revision-gap"
	subCurrentRevision     = "current-revision"
	subChangeActor         = "change-actor"
	subAttributionStatus   = "attribution-status"
	subRowEpoch            = "row-epoch"
	subUndeclaredBump      = "undeclared-bump"
	subDeclaredBumpMissing = "declared-bump-missing"
	subOutsideWindow       = "outside-window"
	subDecreasing          = "decreasing"
	subRowsLost            = "rows-lost"
	subRowsAltered         = "rows-altered"
)

// The sections of an issue's durable projection a real change can be in, in the order
// a delta lists them. Each is a table of the Dolt database.
const (
	sectionIssue        = "issue"
	sectionLabels       = "labels"
	sectionDependencies = "dependencies"
)

// The attribution_status a row carries: claimed when the command ran as an identity
// the harness supplied, unknown when it supplied none.
const (
	statusClaimed = "claimed"
	statusUnknown = "unknown"
)

// Version is one row of issue_versions that an action added.
type Version struct {
	Revision int64
	Epoch    int
	Actor    string
	Status   string
	// At is the row's change_at, in UTC.
	At time.Time
}

// IssueDelta is what one action did to one issue, from Dolt's own diff of the work
// clone between the head before the action and the head after it.
type IssueDelta struct {
	ID string
	// Existed and Exists say whether the issues table has a row for the issue at the
	// head before the action and at the head after it.
	Existed, Exists bool
	// Wisp reports an issue that is not versioned by design: ephemeral, or kept out
	// of history. It is read from the head after the action, and from the head before
	// it for an issue the action deleted.
	Wisp bool
	// GenerationBefore and GenerationAfter are the issue's participation_generation
	// at the two heads; nil is NULL, and also a store or a row that is not there.
	GenerationBefore, GenerationAfter *int64
	// RevisionBefore and RevisionAfter are the issue's current_revision at the two
	// heads, 0 when the issue has no row.
	RevisionBefore, RevisionAfter int64
	// Real reports a change in the durable projection of the issue: a diff of its
	// compared columns, its labels or its dependency edges (see Ledger).
	Real bool
	// Sections lists where the real change is, in the order issue, labels,
	// dependencies.
	Sections []string
	// AddedEdges and RemovedEdges are the targets of the dependency edges the issue
	// owns that the action added and removed.
	AddedEdges, RemovedEdges []string
	// MaxRevisionBefore and MaxRevisionAfter are the highest revision of the issue in
	// issue_versions at the two heads, 0 when it has none.
	MaxRevisionBefore, MaxRevisionAfter int64
	// PreviousChangeAt is the change_at of the issue's highest revision at the head
	// before the action; the zero time when it had none.
	PreviousChangeAt time.Time
	// Rows are the versions the action added, by revision.
	Rows []Version
	// Lost counts the rows of issue_versions the action deleted, and Altered the
	// rows it changed other than by marking them removed.
	Lost, Altered int
	// Marked reports that the action set a removal mark on a row.
	Marked bool
}

// Delta is what one action did to the work clone, per issue.
type Delta struct {
	From, To string
	// EpochBefore and Epoch are the store's epoch at the two heads; a store with no
	// epoch row yet is at epoch 1.
	EpochBefore, Epoch int
	// Fence reports that the issues table has the participation_generation column
	// at the head before the action: the store has legacy records.
	Fence bool
	// Issues are the issues the action touched, by id.
	Issues []IssueDelta
}

// Issue returns the delta of one issue.
func (d Delta) Issue(id string) (IssueDelta, bool) {
	for _, i := range d.Issues {
		if i.ID == id {
			return i, true
		}
	}
	return IssueDelta{}, false
}

// Legacy reports whether the issue was a legacy record before the action: its row was
// there, in a store with the write fence, and had no participation_generation. A
// store without the fence has no such record, and an issue the action created was
// not there to be one.
func (d Delta) Legacy(i IssueDelta) bool {
	return d.Fence && i.Existed && i.GenerationBefore == nil
}

// Action is the one bd command a delta belongs to.
type Action struct {
	// Kind is the translator's name for the action: create, update, delete, dep_add,
	// dep_remove, close or noop.
	Kind  string
	Issue string
	// Actor is the identity the command ran as; every row it mints names it.
	Actor string
	// Start and End bracket the command by the host's clock, in UTC. Both zero means
	// no command ran.
	Start, End time.Time
}

// Options are what a scenario tells the evaluation about the run.
type Options struct {
	// Tolerance is how far outside the action's window the clock of the store may
	// put a row's change_at.
	Tolerance time.Duration
	// EpochBump declares that the epoch is expected to change during the action.
	EpochBump bool
}

// Finding is one rule broken for one issue. Expected and Actual are the facts the
// rule compared, as JSON.
type Finding struct {
	Category string
	Sub      string
	Issue    string
	Expected string
	Actual   string
}

// Result is what Evaluate found for one action.
type Result struct {
	Findings []Finding
	// Legacy and Participating count the action's real changes by the state of their
	// issue before it ran: a legacy record, held back by the write fence, or one that
	// takes part in history. An issue the action did not really change is in neither.
	Legacy, Participating int
	// RemovedMarked reports that a row of issue_versions got a removal mark.
	RemovedMarked bool
}

// Evaluate holds one action's delta to the version-accounting rules and reports what
// breaks them. It reads nothing but its arguments.
//
// A legacy record, an issue of a store with the write fence whose row had no
// participation_generation before the action, is held to the fence alone: whatever it
// was given in breach of that is one finding, and the rules for a participating
// issue, which would only repeat it, do not apply. Every issue is held to the rule
// that history is only added to.
func Evaluate(a Action, d Delta, o Options) Result {
	j := judge{action: a, delta: d, options: o}
	// An epoch that moved without being declared is the action's finding. The rows
	// of the action are not also held to an epoch that moved under them.
	j.judgeRowEpochs = d.EpochBefore == d.Epoch || o.EpochBump
	for _, i := range d.Issues {
		j.issue(i)
	}
	j.epoch()
	j.result.Findings = j.findings
	return j.result
}

// judge accumulates the findings of one Evaluate.
type judge struct {
	action         Action
	delta          Delta
	options        Options
	judgeRowEpochs bool
	findings       []Finding
	result         Result
}

func (j *judge) issue(i IssueDelta) {
	j.survival(i)
	legacy := j.delta.Legacy(i)
	if i.Real {
		if legacy {
			j.result.Legacy++
		} else {
			j.result.Participating++
		}
	}
	if legacy {
		j.fence(i)
		return
	}
	j.count(i)
	j.chain(i)
	j.attribution(i)
	j.rowEpochs(i)
	j.window(i)
}

// fence is V0. A legacy record gets no row, no generation and no new revision from
// any action. A record the action deleted has no issues row left to hold to the
// second and third clauses, and the NULL and the zero it then reads as are not a
// stamp or a move.
func (j *judge) fence(i IssueDelta) {
	var failed []string
	if len(i.Rows) > 0 {
		failed = append(failed, subRowMinted)
	}
	if i.Exists && i.GenerationAfter != nil {
		failed = append(failed, subPromoted)
	}
	if i.Exists && i.RevisionAfter != i.RevisionBefore {
		failed = append(failed, subRevisionMoved)
	}
	if len(failed) == 0 {
		return
	}
	j.add(CategoryFence, strings.Join(failed, "+"), i.ID,
		map[string]any{"rows": 0, "participation_generation": nil, "current_revision": i.RevisionBefore},
		map[string]any{
			"rows": len(i.Rows), "revisions": revisions(i.Rows), "exists": i.Exists,
			"participation_generation": generationFact(i.GenerationAfter), "current_revision": i.RevisionAfter,
		})
}

// count is V1. A participating issue shows exactly one new row if it really changed
// and is there afterwards, and none otherwise.
func (j *judge) count(i IssueDelta) {
	expected := 0
	if i.Real && i.Exists && !i.Wisp {
		expected = 1
	}
	observed := len(i.Rows)
	if observed == expected {
		return
	}
	want := map[string]any{"rows": expected, "real_change": i.Real, "exists_after": i.Exists, "wisp": i.Wisp}
	got := map[string]any{"rows": observed, "revisions": revisions(i.Rows), "sections": nonNil(i.Sections)}
	switch {
	case expected == 0:
		j.add(CategoryCount, subPhantom, i.ID, want, got)
	case observed > expected:
		j.add(CategoryCount, subOverMint, i.ID, want, got)
	default:
		cause, ok := j.exempt(i)
		if !ok {
			j.add(CategoryCount, subUnderMint, i.ID, want, got)
			return
		}
		got["cause"] = cause.Name
		got["removed_edges"] = nonNil(i.RemovedEdges)
		got["action"] = j.action.Kind
		got["action_issue"] = j.action.Issue
		j.add(CategoryGap, cause.Name, i.ID, want, got)
	}
}

// exempt returns the known cause that excuses a real change with no row, if the
// issue's change is exactly of its shape.
func (j *judge) exempt(i IssueDelta) (Cause, bool) {
	for _, e := range exemptions {
		if e.applies(j.action, i) {
			return e.Cause, true
		}
	}
	return Cause{}, false
}

// chain is V2. New revisions follow the highest there was, one after another, and the
// revision counter is the highest revision there is, or 1 for an issue that has none.
func (j *judge) chain(i IssueDelta) {
	if len(i.Rows) > 0 {
		next := i.MaxRevisionBefore + 1
		for _, r := range i.Rows {
			if r.Revision != next {
				j.add(CategoryChain, subRevisionGap, i.ID,
					map[string]any{"first_revision": i.MaxRevisionBefore + 1, "consecutive": true},
					map[string]any{"revisions": revisions(i.Rows)})
				break
			}
			next++
		}
	}
	if !i.Exists {
		return
	}
	want := max(i.MaxRevisionAfter, 1)
	if i.RevisionAfter != want {
		j.add(CategoryChain, subCurrentRevision, i.ID,
			map[string]any{"current_revision": want},
			map[string]any{"current_revision": i.RevisionAfter, "highest_revision": i.MaxRevisionAfter})
	}
}

// attribution is V5. A new row names the identity the command ran as, and its status
// says so.
func (j *judge) attribution(i IssueDelta) {
	status := statusUnknown
	if j.action.Actor != "" {
		status = statusClaimed
	}
	var actors, statuses []string
	wrongActor, wrongStatus := false, false
	for _, r := range i.Rows {
		actors = append(actors, r.Actor)
		statuses = append(statuses, r.Status)
		wrongActor = wrongActor || r.Actor != j.action.Actor
		wrongStatus = wrongStatus || r.Status != status
	}
	if wrongActor {
		j.add(CategoryAttribution, subChangeActor, i.ID,
			map[string]any{"change_actor": j.action.Actor},
			map[string]any{"change_actors": nonNil(actors)})
	}
	if wrongStatus {
		j.add(CategoryAttribution, subAttributionStatus, i.ID,
			map[string]any{"attribution_status": status},
			map[string]any{"attribution_statuses": nonNil(statuses)})
	}
}

// rowEpochs is the per-row half of V6: every new row carries the epoch the store is
// in after the action.
func (j *judge) rowEpochs(i IssueDelta) {
	if !j.judgeRowEpochs {
		return
	}
	var epochs []int
	wrong := false
	for _, r := range i.Rows {
		epochs = append(epochs, r.Epoch)
		wrong = wrong || r.Epoch != j.delta.Epoch
	}
	if wrong {
		j.add(CategoryEpoch, subRowEpoch, i.ID,
			map[string]any{"epoch": j.delta.Epoch},
			map[string]any{"epochs": nonNil(epochs)})
	}
}

// epoch is the per-action half of V6. The epoch belongs to the store and not to an
// issue, so a bump nobody declared, or a declared one that did not happen, is one
// finding for the action, filed under the issue it ran for.
func (j *judge) epoch() {
	moved := j.delta.EpochBefore != j.delta.Epoch
	got := map[string]any{"epoch_before": j.delta.EpochBefore, "epoch": j.delta.Epoch}
	switch {
	case moved && !j.options.EpochBump:
		j.add(CategoryEpoch, subUndeclaredBump, j.action.Issue,
			map[string]any{"epoch_before": j.delta.EpochBefore, "epoch": j.delta.EpochBefore}, got)
	case !moved && j.options.EpochBump:
		j.add(CategoryEpoch, subDeclaredBumpMissing, j.action.Issue,
			map[string]any{"epoch_before": j.delta.EpochBefore, "bumped": true}, got)
	}
}

// window is V7. A row is written inside the command's window, give or take the clock
// tolerance, and an issue's rows never go back in time, the row it had before the
// action included.
func (j *judge) window(i IssueDelta) {
	if len(i.Rows) == 0 {
		return
	}
	if !j.action.Start.IsZero() || !j.action.End.IsZero() {
		notBefore := j.action.Start.Add(-j.options.Tolerance)
		notAfter := j.action.End.Add(j.options.Tolerance)
		outside := false
		for _, r := range i.Rows {
			outside = outside || r.At.Before(notBefore) || r.At.After(notAfter)
		}
		if outside {
			j.add(CategoryTime, subOutsideWindow, i.ID,
				map[string]any{"not_before": timeFact(notBefore), "not_after": timeFact(notAfter)},
				map[string]any{"change_at": changeTimes(i.Rows)})
		}
	}
	previous := i.PreviousChangeAt
	for _, r := range i.Rows {
		if !previous.IsZero() && r.At.Before(previous) {
			j.add(CategoryTime, subDecreasing, i.ID,
				map[string]any{"not_before": timeFact(i.PreviousChangeAt), "in_order": true},
				map[string]any{"change_at": changeTimes(i.Rows)})
			break
		}
		previous = r.At
	}
}

// survival is V8. History is only added to: a row that was there before the action is
// there after it and unchanged. A removal mark on a row is recorded, not judged.
func (j *judge) survival(i IssueDelta) {
	if i.Marked {
		j.result.RemovedMarked = true
	}
	if i.Lost > 0 {
		j.add(CategorySurvival, subRowsLost, i.ID, map[string]any{"rows_lost": 0}, map[string]any{"rows_lost": i.Lost})
	}
	if i.Altered > 0 {
		j.add(CategorySurvival, subRowsAltered, i.ID, map[string]any{"rows_altered": 0}, map[string]any{"rows_altered": i.Altered})
	}
}

func (j *judge) add(category, sub, issue string, expected, actual map[string]any) {
	j.findings = append(j.findings, Finding{
		Category: category, Sub: sub, Issue: issue,
		Expected: facts(expected), Actual: facts(actual),
	})
}

// Cause is a known reason a real change may mint no version.
type Cause struct {
	Name string `json:"name"`
	// Reason says what happens and why the product mints nothing for it.
	Reason string `json:"reason"`
	// Citation names the product code and tests the reason rests on.
	Citation string `json:"citation"`
}

// exemption is a Cause and the shape of change it excuses.
type exemption struct {
	Cause
	applies func(Action, IssueDelta) bool
}

// exemptions is the static table of known causes, cascade first. A real change with
// no row that none of them excuses is an under-mint: an unknown cause is never exempt.
var exemptions = []exemption{
	{
		Cause: Cause{
			Name: "cascade-edge-removal",
			Reason: "an issue that owns a dependency edge to a deleted issue has the edge removed along with " +
				"the delete, and the product mints no version for the owner unless the delete also rewrites " +
				"the owner's text, which it does only when the text cites the deleted id; whether minting " +
				"nothing there is by design is a question for the feature's authors",
			Citation: "internal/storage/issueops/version_completeness_test.go:183-184 (the delete family's " +
				"cleanup of edges whose target is gone), internal/storage/issueops/delete.go:346 (the edge " +
				"removals are journaled before the rows go), internal/storage/issueops/delete_role.go:312-360 " +
				"(only a neighbor whose text cites the deleted id is rewritten), and migrations 0041 to 0043 " +
				"(dependency edges cascade with the issue that owns them)",
		},
		applies: cascadeEdgeRemoval,
	},
}

// cascadeEdgeRemoval is the shape of the owner of an edge to an issue that a delete
// removed: another issue than the deleted one, whose only change is in its
// dependencies, that removed edges and added none, every removed edge pointing at the
// deleted issue. An issue whose text cites the deleted id is rewritten as well and is
// also changed in its issue section, so it is not of this shape.
func cascadeEdgeRemoval(a Action, i IssueDelta) bool {
	return a.Kind == "delete" && i.ID != a.Issue &&
		slices.Equal(i.Sections, []string{sectionDependencies}) &&
		len(i.AddedEdges) == 0 && len(i.RemovedEdges) > 0 &&
		!slices.ContainsFunc(i.RemovedEdges, func(target string) bool { return target != a.Issue })
}

// ExemptCauses returns the table of known causes, first the cascade.
func ExemptCauses() []Cause {
	causes := make([]Cause, 0, len(exemptions))
	for _, e := range exemptions {
		causes = append(causes, e.Cause)
	}
	return causes
}

// facts renders the facts of a finding as one JSON object. The values are the plain
// numbers, strings and lists this file builds them from, which always marshal.
func facts(values map[string]any) string {
	out, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(out)
}

func revisions(rows []Version) []int64 {
	out := []int64{}
	for _, r := range rows {
		out = append(out, r.Revision)
	}
	return out
}

func changeTimes(rows []Version) []any {
	out := []any{}
	for _, r := range rows {
		out = append(out, timeFact(r.At))
	}
	return out
}

// timeFact is a time as a fact: RFC 3339 in UTC, or null for the zero time.
func timeFact(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func generationFact(g *int64) any {
	if g == nil {
		return nil
	}
	return *g
}

// nonNil keeps an empty list a list: a nil slice marshals as null, and a fact that is a
// list should read as one.
func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
