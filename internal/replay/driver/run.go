package driver

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/steveyegge/beads/internal/replay/doltcli"
	"github.com/steveyegge/beads/internal/replay/oracle"
	"github.com/steveyegge/beads/internal/replay/translate"
)

// Tools pins the binary a Run invocation replays through: the integration
// build under test (NFR4). The read side and the translator are libraries, so
// there is nothing else to pin.
type Tools struct {
	IntegrationBin string
}

// RunConfig is everything one Run invocation needs: which integration build to
// test (IntegrationRef/IntegrationRepo/IntegrationSHA), which historical corpus
// is the oracle (OracleDataDir), which fresh project to replay mutations into
// (WorkDir/WorkDataDir), where to persist results (OutDir), and the binary to
// replay through. SampleSize <= 0 means UC2's exhaustive loop; SampleSize > 0
// selects that many evenly-spaced commits (AF1 sampled mode).
type RunConfig struct {
	IntegrationRef  string
	IntegrationRepo string
	// IntegrationSHA is the commit the integration binary was built from, as
	// BuildIntegration returned it. The run records this commit and does not
	// resolve IntegrationRef again, so what it records is what was built, whatever
	// the ref has done since. It is required.
	IntegrationSHA string
	OracleDataDir  string
	WorkDir        string
	WorkDataDir    string
	OutDir         string
	SampleSize     int
	Tools          Tools
	// Resume, when set, is the id of a run that stopped in OutDir. Run carries that
	// run on from the step its journal says it got to instead of starting one.
	Resume string
	// Observer, when set, is told about every (step, issue) result as it is
	// written. It may be nil. A resumed run does not tell it again about the steps
	// the stopped run finished.
	Observer Observer
	// Seed is the record of the seeding that gave the work clone the oracle's base
	// state, nil when the work clone was not seeded. A base that holds issues is
	// refused without one.
	Seed *SeedRecord
}

// ErrLegacyRowsPresent reports a work clone that holds issues created while
// versioned history was off. Such a row never records versions, so a run that
// finds one has not measured what it set out to. It is a fault of the harness
// and is never a finding about the build under test.
var ErrLegacyRowsPresent = errors.New("the work clone holds issues created while versioned history was off")

// Run replays the oracle's history into the work project, step by step along the
// first parent of each commit, and compares the work project's view of every
// touched issue with the oracle's, persisting a CommitReplayResult for every
// (step, issue) pair (and a Mismatch, a CoverageGapRow or a rejection where there
// is one) and a summary of what the run covered. A mismatch does not abort the run
// -- AC2 requires a result row for every pair the run visits, matched or not;
// only an infrastructure failure (a broken ref, a bd that cannot run or is killed,
// a write error) does.
//
// The work project has versioned history switched on before the first step, so
// everything the replay creates records versions. A base that already holds
// issues is refused, before anything is written, unless the work clone was
// seeded from it.
//
// A run says it is running before it does anything a crash could cut short, and
// keeps a journal of its steps, so a run that stopped can be carried on with
// cfg.Resume: see resumeRun. Starting a run over the directory of one that
// stopped and was not resumed is refused. A run that completed is never run again.
func Run(ctx context.Context, cfg RunConfig) (ReplayRun, error) {
	walk, err := ReadWalk(ctx, cfg.OracleDataDir)
	if err != nil {
		return ReplayRun{}, fmt.Errorf("run: %w", err)
	}
	if err := checkSeedGuard(ctx, cfg.OracleDataDir, walk, cfg.Seed); err != nil {
		return ReplayRun{}, fmt.Errorf("run: %w", err)
	}
	if cfg.IntegrationSHA == "" {
		return ReplayRun{}, errors.New("run: no integration SHA: the caller builds the integration and gives the commit it built")
	}

	steps := walk.Steps(cfg.SampleSize)
	var b begun
	if cfg.Resume == "" {
		b, err = beginRun(cfg, walk)
	} else {
		b, err = resumeRun(ctx, cfg, walk, steps)
	}
	if err != nil {
		return b.run, fmt.Errorf("run: %w", err)
	}

	loopErr := replayHistory(ctx, cfg, b.r, steps[b.first:])
	return finishRun(cfg, b, walk, steps, loopErr)
}

// finishRun closes a run, whose loop ended with loopErr: its summary, run.json and
// last, its completed or failed row, which is what says the run is over. A close
// that cannot be written fails a run that had not failed.
func finishRun(cfg RunConfig, b begun, walk *Walk, steps []Step, loopErr error) (ReplayRun, error) {
	run := b.run
	run.Status = "completed"
	if loopErr != nil {
		run.Status = "failed"
	}
	run.FinishedAt = time.Now().UTC()

	settle := func(what string, err error) {
		switch {
		case err == nil:
		case loopErr == nil:
			loopErr = fmt.Errorf("%s: %w", what, err)
			run.Status = "failed"
		default:
			loopErr = fmt.Errorf("%w (and %s: %v)", loopErr, what, err)
		}
	}
	settle("writing the summary", b.store.WriteSummary(b.r.summarize(run, walk, steps, cfg.Seed)))
	rec := b.rec
	rec.Status = run.Status
	settle("writing "+fileRunRecord, writeRunRecord(cfg.OutDir, rec))
	settle("writing the "+run.Status+" row", b.store.WriteReplayRun(run))
	if loopErr != nil {
		return run, fmt.Errorf("run: %w", loopErr)
	}
	return run, nil
}

// replayHistory replays the steps into the work clone and ends the run with the
// check that nothing in it can reach another store. A seeded run first compares
// the clone with the oracle at the base. Then versioned history is switched on, the
// steps are replayed, and every issue the run made is checked to record versions.
//
// The outbound check runs whatever became of the steps, unless the run was
// interrupted, and a failure of the steps and a target left behind are both
// reported when both happened.
func replayHistory(ctx context.Context, cfg RunConfig, r *runner, steps []Step) error {
	err := replaySteps(ctx, cfg, r, steps)
	if ctx.Err() != nil {
		return err
	}
	switch outbound := checkNoOutbound(ctx, cfg.WorkDataDir); {
	case outbound == nil:
		return err
	case err == nil:
		return outbound
	default:
		return errors.Join(err, outbound)
	}
}

// replaySteps is the part of a run between the seed and the outbound check.
func replaySteps(ctx context.Context, cfg RunConfig, r *runner, steps []Step) error {
	env := realEnv{cfg: cfg}
	if cfg.Seed != nil {
		if err := checkSeedHead(ctx, cfg); err != nil {
			return err
		}
		// The baseline is the start of a run. A run carried on from its journal has
		// already had it, and its first step is not the walk's.
		if len(steps) > 0 && steps[0].Index == 0 {
			base := steps[0].From.Hash
			ids, err := issueIDsAt(ctx, cfg.OracleDataDir, base)
			if err != nil {
				return fmt.Errorf("baseline: %w", err)
			}
			if err := r.compareBaseline(ctx, env, base, ids); err != nil {
				return err
			}
		}
	}
	refusal, err := enableVersionedHistory(ctx, cfg)
	if err != nil {
		return err
	}
	r.enableRefused = refusal
	if err := r.runSteps(ctx, env, steps); err != nil {
		return err
	}
	if refusal != nil {
		// The product would not turn history on and the run says so: every row it
		// made is legacy, and the check has nothing to find that the summary does
		// not already say.
		return nil
	}
	return verifyNoLegacyRows(ctx, cfg)
}

// enableVersionedHistory switches versioned history on in the work clone through
// the bd under test. It must happen before the first issue is created: a row made
// while history is off is legacy for its whole life and never records.
//
// A seeded clone can hold a value a version could not record, and the product then
// refuses to turn history on. That is a finding and not a failure of the run: it is
// returned as the refusal, with the number of issues the product named, and the run
// goes on without versioning. Any other refusal of a seeded clone is a *SeedRefused
// of its class. Every other failure is an error, and a clone that was not seeded
// holds nothing the product could refuse for.
func enableVersionedHistory(ctx context.Context, cfg RunConfig) (*EnableRefusal, error) {
	argv := []string{"config", "set", "versioned-history.enabled", "true"}
	out, err := execBd(ctx, cfg.Tools.IntegrationBin, cfg.WorkDir, argv...)
	if err == nil {
		return nil, nil
	}
	var exit *exec.ExitError
	if cfg.Seed != nil && errors.As(err, &exit) && exit.ExitCode() > 0 {
		if n, ok := unversionableCount(string(out)); ok {
			return &EnableRefusal{Count: n}, nil
		}
		if refused := seedRefusalOf(&translate.ExecError{Argv: argv, ExitCode: exit.ExitCode(), Output: string(out), Err: err}); refused != nil {
			return nil, refused
		}
	}
	return nil, fmt.Errorf("switching versioned history on in the work clone: %w\n%s", err, out)
}

// verifyNoLegacyRows is the check that the switch took. History that silently
// stayed off would leave every created row legacy and a run that measured nothing
// it claims to, so a legacy row is a harness error.
//
// A seeded clone starts with legacy rows, which are the base's issues as they were
// and which versioned history never records. They are the issues of the seed's own
// head, so they are the ones the check leaves out; any other row without a
// participation generation was made by the run.
func verifyNoLegacyRows(ctx context.Context, cfg RunConfig) error {
	query := "SELECT COUNT(*) FROM issues WHERE participation_generation IS NULL"
	if cfg.Seed != nil {
		query += " AND id NOT IN (SELECT id FROM issues AS OF " + doltcli.SQLQuote(cfg.Seed.SeedHead) + ")"
	}
	n, err := queryCount(ctx, cfg.WorkDataDir, "checking the work clone for legacy rows", query)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d issues", ErrLegacyRowsPresent, n)
	}
	return nil
}

// realEnv is the step environment of a real run: the oracle's dolt database, the
// work clone's, and the bd under test.
type realEnv struct {
	cfg RunConfig
}

func (e realEnv) Plan(ctx context.Context, from, to string) (*translate.StepPlan, error) {
	return translate.Plan(ctx, e.cfg.OracleDataDir, from, to)
}

func (e realEnv) OracleView(ctx context.Context, ref, issue string) (*oracle.View, error) {
	return oracle.ReadView(ctx, e.cfg.OracleDataDir, ref, issue)
}

// Execute runs one action through the bd under test. On a seeded clone, a bd that
// refuses for the clone's identity or for the look of a remote-backed store is the
// seeding's refusal and not the step's: it is a *SeedRefused, which ends the run,
// where any other refusal is a rejected write.
func (e realEnv) Execute(ctx context.Context, action translate.Action) error {
	err := translate.ExecuteWith(ctx, e.cfg.Tools.IntegrationBin, e.cfg.WorkDir, action)
	var exit *translate.ExecError
	if e.cfg.Seed != nil && errors.As(err, &exit) {
		if refused := seedRefusalOf(exit); refused != nil {
			return refused
		}
	}
	return err
}

func (e realEnv) WorkHead(ctx context.Context) (string, error) {
	return headCommitOf(ctx, e.cfg.WorkDataDir)
}

func (e realEnv) CandidateView(ctx context.Context, ref, issue string) (*oracle.View, error) {
	return oracle.ReadView(ctx, e.cfg.WorkDataDir, ref, issue)
}

// selectSample returns all of steps, copied, when n<=0 or n>=len(steps)
// (UC2's exhaustive loop); otherwise it returns exactly n elements as a
// deterministic, strictly-ascending-index subsequence of steps (AF1's
// evenly-spaced sampled mode) -- never reordered, never randomized.
func selectSample[T any](steps []T, n int) []T {
	if n <= 0 || n >= len(steps) {
		out := make([]T, len(steps))
		copy(out, steps)
		return out
	}
	if n == 1 {
		return []T{steps[0]}
	}
	last := len(steps) - 1
	out := make([]T, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, steps[i*last/(n-1)])
	}
	return out
}

// headCommitOf returns dataDir's current head commit hash, as dolt itself names
// it, rather than as the newest row of dolt_log, which promises no order.
func headCommitOf(ctx context.Context, dataDir string) (string, error) {
	_, rows, err := doltcli.Query(ctx, dataDir, "SELECT hashof('HEAD')")
	if err != nil {
		return "", fmt.Errorf("head commit of %s: %w", dataDir, err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text == "" {
		return "", fmt.Errorf("head commit of %s: dolt returned no head", dataDir)
	}
	return rows[0][0].Text, nil
}
