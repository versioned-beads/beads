package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Options are the settings a driver-core command line carries.
type Options struct {
	IntegrationRef  string // git ref of the integration build under test
	IntegrationRepo string // checkout to build the integration binary from
	OracleDataDir   string // dolt data directory holding the historical corpus
	WorkDir         string // bd project directory to replay into; empty or absent for a new run
	OutDir          string // directory for the JSONL result files
	SampleSize      int    // evenly-spaced commits to sample; 0 replays everything
	Resume          string // id of a stopped run in OutDir to carry on; empty to start a run
}

// Run builds the integration binary under test, prepares the work project, and
// replays the oracle's history into it. The oracle read and the translator are
// libraries called in process, so the integration binary is the only thing
// built.
//
// A base that already holds issues is refused first, before anything is built or
// created, because nothing seeds the work project from it yet. So is a run that
// cannot start: a new run over the unfinished one OutDir holds, or into a WorkDir
// that has anything in it; a resume of a run that is not the one OutDir holds,
// that completed, or whose oracle has changed since. A refusal changes nothing.
// For a run that completed, the ReplayRun returned with the refusal is the one it
// ended with.
func (o Options) Run(ctx context.Context) (ReplayRun, error) {
	walk, err := ReadWalk(ctx, o.OracleDataDir)
	if err != nil {
		return ReplayRun{}, fmt.Errorf("reading the oracle's history: %w", err)
	}
	if err := checkSeedGuard(ctx, o.OracleDataDir, walk, nil); err != nil {
		return ReplayRun{}, err
	}
	if completed, err := o.checkStart(walk); err != nil {
		return completed, err
	}

	toolsDir, err := os.MkdirTemp("", "driver-core-tools-*")
	if err != nil {
		return ReplayRun{}, fmt.Errorf("creating tools dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(toolsDir) }()

	integrationBin := filepath.Join(toolsDir, "integration-bd")
	integrationSHA, err := BuildIntegration(ctx, o.IntegrationRepo, o.IntegrationRef, "./cmd/bd", integrationBin)
	if err != nil {
		return ReplayRun{}, fmt.Errorf("building integration binary: %w", err)
	}

	if o.Resume == "" {
		if err := ensureWorkProject(ctx, o.WorkDir, integrationBin); err != nil {
			return ReplayRun{}, fmt.Errorf("preparing work project: %w", err)
		}
	}
	workDataDir, err := findEmbeddedDoltDir(o.WorkDir)
	if err != nil {
		return ReplayRun{}, fmt.Errorf("locating work project's data dir: %w", err)
	}

	return Run(ctx, RunConfig{
		IntegrationRef:  o.IntegrationRef,
		IntegrationSHA:  integrationSHA,
		IntegrationRepo: o.IntegrationRepo,
		OracleDataDir:   o.OracleDataDir,
		WorkDir:         o.WorkDir,
		WorkDataDir:     workDataDir,
		OutDir:          o.OutDir,
		SampleSize:      o.SampleSize,
		Tools:           Tools{IntegrationBin: integrationBin},
		Resume:          o.Resume,
	})
}

// checkStart refuses what can be refused without a build, so that a refusal costs
// nothing and leaves nothing behind. A run that stopped and was not resumed is
// named before a work directory that has something in it, because it is the reason
// for it. A completed run comes back with the row it ended with.
func (o Options) checkStart(walk *Walk) (ReplayRun, error) {
	if o.Resume == "" {
		if err := checkNoUnfinishedRun(o.OutDir, o.WorkDir); err != nil {
			return ReplayRun{}, err
		}
		return ReplayRun{}, checkWorkDirEmpty(o.WorkDir)
	}
	_, completed, err := checkResume(o.OutDir, o.Resume, walk, o.SampleSize)
	return completed, err
}

// ensureWorkProject initializes workDir as a bd project via integrationBin if
// it isn't one already, so a caller can point driver-core at a fresh empty
// directory without a separate manual bootstrap step.
func ensureWorkProject(ctx context.Context, workDir, integrationBin string) error {
	if _, err := os.Stat(filepath.Join(workDir, ".beads")); err == nil {
		return nil
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("ensure work project: %w", err)
	}
	if out, err := execBd(ctx, integrationBin, workDir, "init", "--non-interactive", "--role=maintainer"); err != nil {
		return fmt.Errorf("ensure work project: init: %w\n%s", err, out)
	}
	return nil
}

// findEmbeddedDoltDir returns the embedded Dolt data directory bd init created
// under dir. Glob's "*" also matches a sibling ".lock" file, so matches must be
// filtered to directories.
func findEmbeddedDoltDir(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".beads", "embeddeddolt", "*"))
	if err != nil {
		return "", fmt.Errorf("find embedded dolt dir under %s: %w", dir, err)
	}
	for _, m := range matches {
		if info, statErr := os.Stat(m); statErr == nil && info.IsDir() {
			return m, nil
		}
	}
	return "", fmt.Errorf("find embedded dolt dir under %s: no directory among matches=%v", dir, matches)
}
