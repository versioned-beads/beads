package scripts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins what the fork adds to the lane it keeps for the proxied-server
// cmd tests: the test-proxied-cmd job of fork-heavy-tiers.yml, which runs
// .github/scripts/proxied-test-shard.sh over the 15 shards of the fork's own
// split. The fork keeps the lane because bazel-proxied is skipped on vb in
// cache mode (ruling be-4y7b5s section 4.2). Ruling be-sudqo0 added two rules to
// it that no upstream test enforces; R9 is here and R8 is not:
//
//   - R9, the timeout budget: the test binary's own timeout is 25m, under a
//     26m step timeout, under the unchanged 30m job timeout. Shards 3 and 5
//     hit upstream's 15m panic in be-sudqo0, so 15m is a budget this lane can
//     overrun, not only a hang detector.
//   - R8, the coverage of the split, lives in fork_proxied_manifest_pin_test.go
//     with the pin that the shard script still honors the manifest override
//     (be-jvluf7 section 4): the split is a file the fork owns now, and that
//     file is in a different Bazel target from this one.
//
// R9 has a fifth value that no test below reads: the timeout column ("25m") of
// the proxied-test-shard.sh row of bazelShardScripts, in
// pr_risk_bazel_coverage_test.go. Upstream's
// TestBazelRetiredLanesCannotBeNarrowed holds that row equal to the script's
// two timeouts, so a sync that re-applies the script re-applies the row too.
//
// R9 lives in lines a sync takes from upstream, so the test below fails in the
// sync pull request, names what to put back, and leaves the fix to a person.
// The lane is temporary: when it retires with fork-heavy-tiers.yml, revert R9's
// edits and delete this file together, with its line in scripts/BUILD.bazel.
//
// Where R9 looks. R9 covers fork-heavy-tiers.yml, the lane's one home (ruling
// be-jvluf7 section 4.5). pr-risk.yml left forkProxiedLaneWorkflows by R9's own
// drop rule (a workflow leaves the pin only when `git grep -c test-proxied-cmd`
// on it is 0, as it is in upstream's pr-risk.yml). The budgets never loosen.

const (
	forkProxiedLaneJob   = "test-proxied-cmd"
	forkProxiedLaneStep  = "Test proxied-server cmd shard"
	forkProxiedShardFile = ".github/scripts/proxied-test-shard.sh"

	// R9's three numbers, innermost first. The binary's own timeout fires first
	// and panics with a goroutine dump; the step timeout only backstops a
	// wedged binary; the job budget absorbs the download, install and pull steps.
	forkProxiedBinaryMinutes = 25
	forkProxiedStepMinutes   = 26
	forkProxiedJobMinutes    = 30

	// Every R9 failure ends with this, so the sync author knows what a
	// conflict at the site resolves to.
	forkProxiedR9Remedy = "take upstream's text, then re-apply R9 (be-sudqo0)"
)

// The workflows that hold the proxied lane, and so the job R9's step and job
// budgets are read from: fork-heavy-tiers.yml alone. Its job is W1's lift of
// pr-risk.yml's, with job id and step name verbatim (be-fxkoxb); pr-risk.yml
// holds no such job in upstream's text, which is R9's own rule for leaving the list.
var forkProxiedLaneWorkflows = []string{"fork-heavy-tiers.yml"}

// The two places proxied-test-shard.sh sets the test binary's timeout: the
// compiled binary CI runs (-test.timeout=), and the go test fallback (-timeout).
var (
	forkProxiedBinaryTimeoutRe = regexp.MustCompile(`-test\.timeout=(\d+)m\b`)
	forkProxiedGoTestTimeoutRe = regexp.MustCompile(`\s-timeout (\d+)m\b`)
)

// R9. The three timeouts of the proxied lane are 25m, 26m and 30m, in that
// order, in the shard script and in each workflow that runs it
// (forkProxiedLaneWorkflows: fork-heavy-tiers.yml). scripts_test's runfiles hold
// the script and the workflow (//:repo_other_files), so this runs under Bazel and
// has no skip.
func TestForkLegacyProxiedLaneTimeoutBudget(t *testing.T) {
	script := readPolicyFile(t, sourceRepoRoot(t), forkProxiedShardFile)

	scriptMinutes := 0
	for _, site := range []struct {
		what string
		re   *regexp.Regexp
	}{
		{"the compiled test binary's -test.timeout", forkProxiedBinaryTimeoutRe},
		{"the go test fallback's -timeout", forkProxiedGoTestTimeoutRe},
	} {
		matches := site.re.FindAllStringSubmatch(script, -1)
		if len(matches) != 1 {
			t.Errorf("%s: found %d places matching %s (%s), want exactly 1; the script's shape changed, so %s by hand and update this test's pattern",
				forkProxiedShardFile, len(matches), site.re, site.what, forkProxiedR9Remedy)
			continue
		}
		minutes, err := strconv.Atoi(matches[0][1])
		if err != nil {
			t.Fatalf("%s: %s: %v", forkProxiedShardFile, site.what, err)
		}
		if minutes != forkProxiedBinaryMinutes {
			t.Errorf("%s: %s is %dm, want %dm; %s", forkProxiedShardFile, site.what, minutes, forkProxiedBinaryMinutes, forkProxiedR9Remedy)
		}
		scriptMinutes = max(scriptMinutes, minutes)
	}

	for _, name := range forkProxiedLaneWorkflows {
		if _, err := os.Stat(filepath.Join(sourceRepoRoot(t), ".github", "workflows", name)); err != nil {
			t.Errorf("%s: %v; it is listed in forkProxiedLaneWorkflows, so either W1 (be-fxkoxb) has not added it yet, or it was retired and leaves the list with R9's edits", name, err)
			continue
		}
		job := readCIWorkflow(t, name).job(t, forkProxiedLaneJob)
		step := job.step(t, forkProxiedLaneStep)
		if step.TimeoutMinutes != forkProxiedStepMinutes {
			t.Errorf("%s: job %s, step %q: timeout-minutes is %d, want %d; %s",
				name, forkProxiedLaneJob, forkProxiedLaneStep, step.TimeoutMinutes, forkProxiedStepMinutes, forkProxiedR9Remedy)
		}
		if job.TimeoutMinutes != forkProxiedJobMinutes {
			t.Errorf("%s: job %s: timeout-minutes is %d, want %d (R9 leaves the job budget as upstream has it); %s",
				name, forkProxiedLaneJob, job.TimeoutMinutes, forkProxiedJobMinutes, forkProxiedR9Remedy)
		}
		if !(scriptMinutes < step.TimeoutMinutes && step.TimeoutMinutes < job.TimeoutMinutes) {
			t.Errorf("%s: job %s: the timeouts must nest strictly, test binary %dm < step %dm < job %dm, so the binary's own panic and goroutine dump fire before the step timeout kills it and before the job budget runs out; %s",
				name, forkProxiedLaneJob, scriptMinutes, step.TimeoutMinutes, job.TimeoutMinutes, forkProxiedR9Remedy)
		}
	}
}
