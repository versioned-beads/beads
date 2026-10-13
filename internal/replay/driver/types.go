// Package driver is the replay-with-oracle driver: AF1 single-commit
// orchestration and the UC2 full replay loop. It walks the oracle's history along
// first parents, replays each commit step through the mutation translator and
// the integration build under test, compares the work clone's view of every
// touched issue with the oracle's, and persists results. The oracle read side
// and the translator are called in process.
package driver

import (
	"encoding/json"
	"time"
)

// ReplayRun is the ERD's REPLAY_RUN entity: one invocation of
// the harness, pinning the exact integration build under test (NFR4).
type ReplayRun struct {
	ID             string    `json:"id"`
	IntegrationRef string    `json:"integration_ref"`
	IntegrationSHA string    `json:"integration_sha"`
	Mode           string    `json:"mode"` // "exhaustive" | "sampled"
	SampleSize     int       `json:"sample_size,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at,omitzero"`
	Status         string    `json:"status"` // "running" | "completed" | "failed"
}

// Verdict is what replaying one issue at one step came to.
type Verdict string

const (
	// VerdictMatched: the replay produced the oracle's state for the issue.
	VerdictMatched Verdict = "matched"
	// VerdictMismatch: it produced a different state. A finding about the build
	// under test.
	VerdictMismatch Verdict = "mismatch"
	// VerdictUncomparable: a number in the pair is outside what can be compared
	// exactly, so neither a match nor a mismatch can be claimed.
	VerdictUncomparable Verdict = "uncomparable"
	// VerdictUntranslatable: the step changed the issue in a way the translator has
	// no bd form for. Nothing ran for it; the issue is quarantined.
	VerdictUntranslatable Verdict = "untranslatable"
	// VerdictRejected: bd refused an action for the issue. A finding about the
	// build under test; the issue is quarantined.
	VerdictRejected Verdict = "rejected"
	// VerdictSkippedQuarantined: the issue was quarantined by an earlier step, so
	// nothing ran for it and nothing was compared.
	VerdictSkippedQuarantined Verdict = "skipped-quarantined"
)

// verdicts lists every verdict, in the order a summary reports them.
var verdicts = []Verdict{
	VerdictMatched, VerdictMismatch, VerdictUncomparable,
	VerdictUntranslatable, VerdictRejected, VerdictSkippedQuarantined,
}

// CommitReplayResult is the ERD's COMMIT_REPLAY_RESULT entity: the outcome of
// replaying one historical commit step for one issue and comparing it against
// the oracle. Written for every (step, issue) pair the run visits, matched or
// not. SourceCommit is the commit the step ends at, FromCommit the one it starts
// from, and Step is the step's position in the run, from 0: the journal names a
// step the same way, which is how a resume tells whose rows are whose.
//
// A compared issue is held to the oracle on two axes, and Verdict is the worse of
// them: RowVerdict for the rows the work clone holds, VersionVerdict for the version
// the store minted. They are set when the run reads both, and empty for an issue
// nothing was compared for, and for a run that reads the rows alone. OracleHash and
// CandidateHash are the row axis's.
//
// A run that accounts for the versions its commands mint also keeps, in Accounting,
// what it made of the commands it ran for the issue. A finding of that accounting
// about an issue the step did not read is carried by the row of the issue whose
// command it was found after, and makes that row a mismatch.
type CommitReplayResult struct {
	RunID          string             `json:"run_id"`
	Step           int                `json:"step"`
	FromCommit     string             `json:"from_commit"`
	SourceCommit   string             `json:"source_commit"`
	IssueID        string             `json:"issue_id"`
	MutationKind   string             `json:"mutation_kind"`
	Verdict        Verdict            `json:"verdict"`
	Matched        bool               `json:"matched"`
	RowVerdict     AxisVerdict        `json:"row_verdict,omitempty"`
	VersionVerdict AxisVerdict        `json:"version_verdict,omitempty"`
	OracleHash     string             `json:"oracle_hash"`
	CandidateHash  string             `json:"candidate_hash"`
	Detail         *ResultDetail      `json:"detail,omitempty"`
	Accounting     *VersionAccounting `json:"version_accounting,omitempty"`
}

// VersionAccounting is what the version accounting made of the commands the replay ran
// for one (step, issue) row. It is kept in the row, and the summary's totals are folded
// from the rows, so that a run carried on from its journal totals what a run that was
// never stopped totals.
type VersionAccounting struct {
	// LegacyChanges and ParticipatingChanges count the real changes the commands made,
	// to the issue or, by cascade, to others: one for each command and issue with a
	// change, by whether the issue was a legacy record before the command or took part
	// in history. A command that changed nothing for an issue counts nothing for it.
	LegacyChanges        int `json:"legacy_changes,omitempty"`
	ParticipatingChanges int `json:"participating_changes,omitempty"`
	// Findings counts what was found wrong with the versions, by category. version-gap
	// is counted here and is not a mismatch: it is a real change the product is known
	// to mint nothing for.
	Findings map[string]int `json:"findings,omitempty"`
	// RemovedMarked is set when a command put a removal mark on a row of issue_versions.
	RemovedMarked bool `json:"removed_marked,omitempty"`
}

// empty reports an accounting with nothing in it, which a row leaves out.
func (a VersionAccounting) empty() bool {
	return a.LegacyChanges == 0 && a.ParticipatingChanges == 0 && len(a.Findings) == 0 && !a.RemovedMarked
}

// ResultDetail says more about a result than its verdict does, for the verdicts
// that have more to say. Only the members of the verdict in question are set.
type ResultDetail struct {
	// Columns and Reasons: an untranslatable issue's columns that have no bd form,
	// and why the step could not be replayed.
	Columns []string `json:"columns,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	// Argv, ExitCode and Output: a rejected action, as bd refused it. Output is
	// what bd printed, standard output and standard error together.
	Argv     []string `json:"argv,omitempty"`
	ExitCode int      `json:"exit_code,omitempty"`
	Output   string   `json:"output,omitempty"`
	// QuarantinedAt: the commit of the step that quarantined a skipped issue.
	QuarantinedAt string `json:"quarantined_at,omitempty"`
}

// CoverageGapRow records that a step changed a table the replay does not cover:
// a table that is unsupported, or one the table policy does not name at all.
// The run goes on, and the hole is visible, one row per step and table.
type CoverageGapRow struct {
	RunID        string `json:"run_id"`
	Step         int    `json:"step"`
	FromCommit   string `json:"from_commit"`
	SourceCommit string `json:"source_commit"`
	Table        string `json:"table"`
	// Unknown is set when the table is not in the table policy at all.
	Unknown bool   `json:"unknown,omitempty"`
	Reason  string `json:"reason"`
}

// Mismatch is the ERD's MISMATCH entity: full repro context for a
// non-matching comparison. A step that differs on both axes has one for each, told
// apart by Category: the version axis's is version-state, and Section says where in
// the issue its document differs. A finding of the version accounting is a row of its
// own: Category is the rule that was broken, Section the way it was broken, and
// IssueID the issue the finding is about, which a step does not always read.
type Mismatch struct {
	RunID        string          `json:"run_id"`
	Step         int             `json:"step"`
	SourceCommit string          `json:"source_commit"`
	IssueID      string          `json:"issue_id"`
	Category     string          `json:"category"`
	Section      string          `json:"section,omitempty"`
	ExpectedJSON json.RawMessage `json:"expected_json"`
	ActualJSON   json.RawMessage `json:"actual_json"`
}

// MetricSample is the ERD's METRIC_SAMPLE entity: one raw measurement. The step
// loop takes none; a measuring Observer writes them through the Store, and
// percentile aggregation is a downstream consumer's job, not this package's.
type MetricSample struct {
	RunID     string    `json:"run_id"`
	Name      string    `json:"name"` // "storage_bytes" | "write_latency_ms"
	Value     float64   `json:"value"`
	SampledAt time.Time `json:"sampled_at"`
}
