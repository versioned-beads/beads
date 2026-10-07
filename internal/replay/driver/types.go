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
type CommitReplayResult struct {
	RunID         string        `json:"run_id"`
	Step          int           `json:"step"`
	FromCommit    string        `json:"from_commit"`
	SourceCommit  string        `json:"source_commit"`
	IssueID       string        `json:"issue_id"`
	MutationKind  string        `json:"mutation_kind"`
	Verdict       Verdict       `json:"verdict"`
	Matched       bool          `json:"matched"`
	OracleHash    string        `json:"oracle_hash"`
	CandidateHash string        `json:"candidate_hash"`
	Detail        *ResultDetail `json:"detail,omitempty"`
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
// non-matching comparison.
type Mismatch struct {
	RunID        string          `json:"run_id"`
	Step         int             `json:"step"`
	SourceCommit string          `json:"source_commit"`
	IssueID      string          `json:"issue_id"`
	Category     string          `json:"category"`
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
