package scripts_test

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins what the fork adds to upstream's legacy proxied lane, the
// test-proxied-cmd job of pr-risk.yml (upstream #7316 removed main.yml's copy:
// push to main runs that tier under Bazel only). That job runs
// .github/scripts/proxied-test-shard.sh over the 15-shard block of
// .github/scripts/proxied-cmd-test-shards.txt, and the fork keeps it because
// BAZEL_COVERS_FORKS is off (fork_legacy_lanes_pin_test.go). Ruling be-sudqo0
// adds two rules to it that no upstream test enforces:
//
//   - R9, the timeout budget: the test binary's own timeout is 25m, under a
//     26m step timeout, under the unchanged 30m job timeout. Shards 3 and 5
//     hit upstream's 15m panic in be-sudqo0, so 15m is a budget this lane can
//     overrun, not only a hang detector.
//   - R8, the 15-block's coverage: it lists every discovered TestProxiedServer*
//     and TestServerMode* test exactly once. The script places a test the block
//     does not list by hash(name) % 15, which is how a sync's new tests landed
//     unbalanced on shard 5.
//
// R9 has a fifth value that no test below reads: the timeout column ("25m") of
// the proxied-test-shard.sh row of bazelShardScripts, in
// pr_risk_bazel_coverage_test.go. Upstream's
// TestBazelRetiredLanesCannotBeNarrowed holds that row equal to the script's
// two timeouts, so a sync that re-applies the script re-applies the row too.
//
// Both rules live in lines a sync takes from upstream, so each test below fails
// in the sync pull request, names what to put back, and leaves the fix to a
// person. If upstream retires the lane, or the operator turns BAZEL_COVERS_FORKS
// on, delete R9's edits and this file together, with its line in
// scripts/BUILD.bazel and its two entries in
// tools/bazel/equivalence_allowlist.txt (R8's lines are then harmless).

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

// The two places proxied-test-shard.sh sets the test binary's timeout: the
// compiled binary CI runs (-test.timeout=), and the go test fallback (-timeout).
var (
	forkProxiedBinaryTimeoutRe = regexp.MustCompile(`-test\.timeout=(\d+)m\b`)
	forkProxiedGoTestTimeoutRe = regexp.MustCompile(`\s-timeout (\d+)m\b`)
)

// R9. The three timeouts of the legacy proxied lane are 25m, 26m and 30m, in
// that order, in the shard script and in pr-risk.yml, the one workflow that
// still runs it.
func TestForkLegacyProxiedLaneTimeoutBudget(t *testing.T) {
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Skip("scripts_test's runfiles hold no .github/scripts")
	}
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

	for _, name := range []string{"pr-risk.yml"} {
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

// R8. The legacy 15-block lists every discovered TestProxiedServer* and
// TestServerMode* test exactly once. The generator's --check gates on name
// coverage alone (it never compares shard assignments), which is the whole of
// the rule: the block is topped up, never repacked.
//
// Upstream's TestProxiedShardManifestGeneratorNotStale checks only the Bazel
// lane's 30-block and leaves the frozen 15-block out, because a --check against
// it would always fail by design there. R8 tops the fork's block up, so the
// same check passes here and this test holds it.
func TestForkLegacyProxiedBlockCoversEveryTest(t *testing.T) {
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Skip("scripts_test's runfiles hold neither the generator's sources nor cmd/bd")
	}
	python := requireHostTool(t, "python3")
	cmd := exec.Command(python, "scripts/ci/gen_proxied_shard_manifest.py", "15", "--check")
	cmd.Dir = sourceRepoRoot(t)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	t.Errorf("the 15-shard block of .github/scripts/proxied-cmd-test-shards.txt does not list every discovered TestProxiedServer*/TestServerMode* test exactly once (R8, be-sudqo0): %v\n%s\n"+
		"Fix: run `python3 scripts/ci/gen_proxied_shard_manifest.py 15 --write`, the plain incremental top-up (it keeps every existing line and places only the new names), and commit the result. "+
		"Do not follow a `Run:` line above that adds --weights=duration, and never pass --repack: the 15-block is not repacked. "+
		"If the top-up puts a new heavy test on a shard the V2 table of be-sudqo0 shows to be busy, relocate that one new line only, never a line that already existed, and record it in the sync pull request.",
		err, out)
}
