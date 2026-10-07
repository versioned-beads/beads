// Command driver-core replays a historical corpus's mutations, commit by
// commit, through an integration build of bd and compares each result against
// the corpus's own row: the driver of the replay harness. It is a thin wrapper
// over internal/replay/driver, which holds all of the logic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/steveyegge/beads/internal/replay/driver"
)

func main() {
	integrationRef := flag.String("integration-ref", "HEAD", "git ref of the integration build under test")
	integrationRepo := flag.String("integration-repo", "", "path to the beads repo checkout to build the integration binary from (required)")
	oracleDataDir := flag.String("oracle-data-dir", "", "dolt data directory containing the historical corpus to replay (required)")
	workDir := flag.String("work-dir", "", "bd project directory to replay mutations into; a new run starts in an empty or absent directory and creates the project there (required)")
	outDir := flag.String("out-dir", "", "directory to write the replay_runs, commit_replay_results, mismatches and coverage_gaps JSONL files and summary.json to (required)")
	sampleSize := flag.Int("sample-size", 0, "number of evenly-spaced commits to sample; 0 replays the full history exhaustively")
	resume := flag.String("resume", "", "id of a run that stopped in --out-dir, to carry on from the step it got to; a run that completed is not run again")
	flag.Parse()

	if *integrationRepo == "" || *oracleDataDir == "" || *workDir == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "usage: driver-core --integration-repo DIR --oracle-data-dir DIR --work-dir DIR --out-dir DIR [--integration-ref REF] [--sample-size N] [--resume RUN_ID]")
		os.Exit(2)
	}

	// An interrupt stops the run where it is and lets it clean up after itself. The
	// first one is taken that way; once it has come, a second takes its usual effect
	// and ends the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()

	replayRun, err := driver.Options{
		IntegrationRef:  *integrationRef,
		IntegrationRepo: *integrationRepo,
		OracleDataDir:   *oracleDataDir,
		WorkDir:         *workDir,
		OutDir:          *outDir,
		SampleSize:      *sampleSize,
		Resume:          *resume,
	}.Run(ctx)
	stop()
	if errors.Is(err, driver.ErrRunCompleted) {
		// Nothing is applied twice, so the run is not run again. What it came to is said
		// before the refusal.
		printRun(replayRun)
	}
	if err != nil {
		log.Fatalf("driver-core: %v", err)
	}

	printRun(replayRun)
}

func printRun(replayRun driver.ReplayRun) {
	fmt.Printf("replay run %s: status=%s mode=%s integration_sha=%s\n", replayRun.ID, replayRun.Status, replayRun.Mode, replayRun.IntegrationSHA)
}
