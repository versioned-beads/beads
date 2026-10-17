package scripts_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict on
// it). It pins .github/workflows/fork-qualification.yml, the permanent workflow
// that keeps four duties: three the upstream sync takes out of pr.yml (work
// package W2, be-67atam; ruling be-jvluf7 section 3.5) and the served HTTP tests
// that no fork-hosted lane ran (ruling be-s847fi, build be-vvsbdl):
//
//   - build-artifacts: the fork's own build of the Linux bd, uploaded as
//     fork-build-artifacts. Upstream owns the name ci-build-artifacts, and
//     pr_lanes_bazel_coverage_test.go fails any pr.yml job that needs a
//     build-artifacts job, so the fork's copy lives here.
//   - graph-c0: the real-engines, installed-CLI and BDP qualification, unchanged
//     apart from the artifact it downloads.
//   - replay-census: the fork's replay guarantee (C1), rebuilt as a narrow go
//     test, because nothing at the sync target runs `make ci-pr-core` any more.
//   - served-http: the served HTTP tier. The end-to-end tests of ./backend/http
//     and ./internal/httpclient skip unless an embedded-Dolt gate is set, so the
//     job sets the gates, runs both packages under go test -v, and a step of its
//     own then fails the job unless the log shows both packages ok, five named
//     sentinel tests passed and every top-level skip is a KNOWN DIVERGENCE.
//
// plus one aggregator, "Fork Qualification / Required", that fails unless every
// other job's result is exactly success.
//
// What is pinned, by the acceptance criteria of the bead:
//
//   - 1, the workflow and the C1 pins: the four C1 tests keep the names they had
//     in ci_workflow_test.go (TestC1PRCoreSetsReplayRequire,
//     TestC1ReplayRequireIsStepScoped, TestC1NoNewWorkflowFile,
//     TestC1PRCoreCheckoutCarriesHistory) and are re-pointed at the replay-census
//     job; TestForkQualificationKeepsGraphCore and
//     TestGraphC0VerifierPassesAgainstForkQualification pin the graph job, its
//     timeout and the path scripts/ci/graph-c0-verifier-test.py reads it from;
//     TestForkQualificationBuildArtifactsJob and
//     TestForkQualificationCensusSetup pin the artifact names and the census
//     job's setup; the aggregator, the trigger shape and the barred text have
//     tests of their own.
//   - 2, REPLAY_REQUIRE never reaches a Bazel target
//     (TestForkQualificationReplayRequireNeverReachesBazel), and the census is a
//     census: TestForkQualificationCensusDiscoversTheReplayPackages runs the
//     census step's own script, with a go shim, against fixture modules and fails
//     unless the step computes the package list at run time, prints it, passes it
//     to one go test with the flags of pr-core.sh, and fails when the list or
//     ./internal/replay/... is empty or when go test fails.
//     TestForkQualificationCensusHarnessControls proves that harness can fail.
//   - 3, the served HTTP tier (ruling be-s847fi section 4.4):
//     TestForkQualificationServedJobShape pins the job's keys, the three
//     variables that make the tests run and make a missing run fail, the one go
//     test line and the step after it; TestForkQualificationServedCheckStep runs
//     that step's own script, with a go shim, against fixture logs and fails
//     unless it rejects every way a run can be missing or hollow; and
//     TestForkQualificationServedControls proves that harness can fail.
//
// Criteria this file cannot pin are runs, and nothing here stands in for them:
// the census run with dolt absent from PATH and REPLAY_REQUIRE=1 (ruling 3.5:
// "a census that cannot fail is not a census"), the upstream workflow sweeps
// run in a scratch checkout at the sync target with this workflow present, the
// fork pull request that runs the workflow with its aggregator green, and the
// hosted runs of the served-http job (H1 to H4 of ruling be-s847fi section 5.2).
//
// Every pin here asserts a shape of the fork, so the file is permanent: it goes
// only if the ruling is lifted. If a sync makes one fail in a way the failure
// text does not cover, take it to the architect before changing the pin: the
// sections of the ruling are cited so the question can be asked precisely.

const (
	forkQualFile       = "fork-qualification.yml"
	forkQualAggregator = "Fork Qualification / Required"
	forkQualBuildJob   = "build-artifacts"
	forkQualGraphJob   = "graph-c0"
	forkQualCensusJob  = "replay-census"
	forkQualServedJob  = "served-http"
	forkQualArtifact   = "fork-build-artifacts"
	forkQualBinary     = "bd-linux-gms-pure"
	forkQualBuildPre   = "Fork "
	forkQualGraphName  = "Graph core / real engines, installed CLI and BDP"
	forkQualGraphMins  = 90
	forkQualReplayKey  = "REPLAY_REQUIRE"
	forkQualVerifier   = "scripts/ci/graph-c0-verifier-test.py"
	forkQualQualify    = "graph-c0-qualify.py"
	forkQualPRCore     = "scripts/ci/pr-core.sh"
	forkQualGate       = ".github/scripts/ci-gate.sh"

	// The one test of the verifier that reads the graph job's timeout, and so
	// the one that notices the job moving out of pr.yml.
	forkQualVerifierTest = "SmokeDeadlines.test_slow_runner_deadlines_nest_inside_the_job_budget"
)

var (
	forkQualHostedRunnerRe = regexp.MustCompile(`^ubuntu-(latest|[0-9]{2}\.[0-9]{2})$`)
	forkQualMakeRe         = regexp.MustCompile(`\bmake\b`)
	forkQualBazelRe        = regexp.MustCompile(`(?i)\bbazel(isk)?\b`)
	forkQualNeedsResultRe  = regexp.MustCompile(`\$\{\{\s*needs\.([A-Za-z0-9_-]+)\.result\s*\}\}`)
	forkQualExprRe         = regexp.MustCompile(`\$\{\{.*?\}\}`)

	// The jobs the aggregator waits for, sorted.
	forkQualJobIDs = []string{forkQualBuildJob, forkQualGraphJob, forkQualCensusJob, forkQualServedJob}

	// Text the ruling bars from the file: the same words the other fork-owned
	// workflows may not carry (fork_heavy_tiers_pin_test.go), plus the upstream
	// sweeps' reading of every workflow file as text. Comments count.
	forkQualBarredText = []string{
		"golangci", "blacksmith", "secrets.", "run-id", "workflow_run", "rbe_", "pull_request_target",
	}
)

// forkQualRead returns the workflow's text. The first thing every pin does, so
// that until W2 lands each one fails for the same stated reason.
func forkQualRead(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), ".github", "workflows", forkQualFile))
	if err != nil {
		t.Fatalf("%s is missing: W2 (be-67atam, ruling be-jvluf7 section 3.5) adds it: %v", forkQualFile, err)
	}
	return data
}

func forkQualLoad(t *testing.T) ciWorkflow {
	t.Helper()
	forkQualRead(t)
	return readCIWorkflow(t, forkQualFile)
}

// forkQualAggregatorJob finds the all-success job by its name: the ruling fixes
// the name and the semantics, not the job id.
func forkQualAggregatorJob(t *testing.T, wf ciWorkflow) (string, ciWorkflowJob) {
	t.Helper()
	var ids []string
	for id, job := range wf.Jobs {
		if job.Name == forkQualAggregator {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("%d jobs in %s are named %q (%v), want exactly 1: the aggregator", len(ids), forkQualFile, forkQualAggregator, ids)
	}
	return ids[0], wf.Jobs[ids[0]]
}

// forkQualIf normalizes a job or step condition: the ${{ }} wrapper is optional
// in GitHub's syntax.
func forkQualIf(condition string) string {
	condition = strings.TrimSpace(condition)
	if strings.HasPrefix(condition, "${{") && strings.HasSuffix(condition, "}}") {
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	}
	return condition
}

// forkQualContinues reports whether a continue-on-error value lets the step or
// job fail without failing the run: anything but absent or a literal false.
func forkQualContinues(value any) bool {
	return value != nil && value != false
}

// forkQualRunLines returns the lines of a run script that the shell would act
// on: shell comment lines are left out.
func forkQualRunLines(run string) []string {
	var lines []string
	for _, line := range strings.Split(run, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// forkQualSteps returns the indexes of the steps that match.
func forkQualSteps(job ciWorkflowJob, match func(ciWorkflowStep) bool) []int {
	var at []int
	for i, step := range job.Steps {
		if match(step) {
			at = append(at, i)
		}
	}
	return at
}

func forkQualUses(family string) func(ciWorkflowStep) bool {
	return func(step ciWorkflowStep) bool { return actionFamily(step.Uses) == family }
}

// forkQualRuns matches a step whose run script contains every needle.
func forkQualRuns(needles ...string) func(ciWorkflowStep) bool {
	return func(step ciWorkflowStep) bool {
		if step.Run == "" {
			return false
		}
		for _, needle := range needles {
			if !strings.Contains(step.Run, needle) {
				return false
			}
		}
		return true
	}
}

// forkQualOne returns the index of the one step that matches and fails the test
// if there is none or more than one.
func forkQualOne(t *testing.T, id string, job ciWorkflowJob, what string, match func(ciWorkflowStep) bool) int {
	t.Helper()
	at := forkQualSteps(job, match)
	if len(at) != 1 {
		t.Fatalf("job %s has %d steps that %s, want exactly 1 (ruling be-jvluf7 section 3.5)", id, len(at), what)
	}
	return at[0]
}

// forkQualCensusStep returns the census job's go test step: the one step whose
// env carries REPLAY_REQUIRE. The variable is step-scoped (C1), so a job that
// sets it anywhere else has no such step.
func forkQualCensusStep(t *testing.T, job ciWorkflowJob) (int, ciWorkflowStep) {
	t.Helper()
	at := forkQualSteps(job, func(step ciWorkflowStep) bool {
		_, ok := step.Env[forkQualReplayKey]
		return ok
	})
	switch len(at) {
	case 1:
		return at[0], job.Steps[at[0]]
	case 0:
		t.Fatalf("no step of job %s has %s in its env: the variable must be set in the env of the one step that runs go test, not on the job or the workflow (replaytest.Require reports bd as unavailable inside a Bazel sandbox, so a wider scope would fail every test that needs bd there)", forkQualCensusJob, forkQualReplayKey)
	default:
		t.Fatalf("steps %v of job %s all set %s, want exactly one: the go test step", at, forkQualCensusJob, forkQualReplayKey)
	}
	return 0, ciWorkflowStep{}
}

// forkWorkflowFileNames lists the workflow files under .github/workflows.
func forkWorkflowFileNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(sourceRepoRoot(t), ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// C1.PRCoreSetsReplayRequire: the replay census runs the replay harness's
// packages with REPLAY_REQUIRE=1. The harness's tests need the dolt CLI and the
// Go toolchain, and replaytest.Require fails a test whose tool is missing
// instead of skipping it only when the variable is exactly "1". Without it a
// runner that lost either tool would skip those tests and the census would
// still pass. The job is unconditional (a skipped job fails the aggregator) and
// runs go test itself: make ci-pr-core is not the vehicle (nothing at the sync
// target runs it) and the census is never a Bazel target.
func TestC1PRCoreSetsReplayRequire(t *testing.T) {
	job := forkQualLoad(t).job(t, forkQualCensusJob)
	if !forkQualHostedRunnerRe.MatchString(job.RunsOn) || job.If != "" || job.ContinueOnError {
		t.Errorf("job %s must run unconditionally in a GitHub-hosted Linux job (runs-on %q, if %q, continue-on-error %v)", forkQualCensusJob, job.RunsOn, job.If, job.ContinueOnError)
	}
	if !slices.Equal([]string(job.Needs), []string{forkQualBuildJob}) {
		t.Errorf("job %s needs %v, want exactly [%s]: it verifies and runs the bd that job uploaded", forkQualCensusJob, []string(job.Needs), forkQualBuildJob)
	}
	_, step := forkQualCensusStep(t, job)
	if step.If != "" || forkQualContinues(step.ContinueOnError) {
		t.Errorf("the go test step of job %s must be nonoptional (if %q, continue-on-error %v)", forkQualCensusJob, step.If, step.ContinueOnError)
	}
	if got := step.Env[forkQualReplayKey]; got != "1" {
		t.Errorf("the go test step sets %s to %q, want \"1\": replaytest.Require turns a missing dependency into a failure only for exactly that value", forkQualReplayKey, got)
	}
	run := strings.Join(forkQualRunLines(step.Run), "\n")
	if !strings.Contains(run, "go test") {
		t.Errorf("the step that sets %s does not run go test; it must be the step that runs the replay packages", forkQualReplayKey)
	}
	if forkQualMakeRe.MatchString(run) || forkQualBazelRe.MatchString(run) {
		t.Errorf("the go test step runs make or Bazel; the census is a plain go test (ruling be-jvluf7 section 3.5): %q", run)
	}
}

// C1.ReplayRequireIsStepScoped: REPLAY_REQUIRE appears in exactly one place
// across the workflows, the env of the census job's go test step: never in a
// workflow-level or job-level env, never on another step, never as an input to
// a called workflow, and never named in a run script (a shell comment or an
// echo counts: it is a mention of the variable inside a value). replaytest.Require
// reports bd as unavailable inside a Bazel sandbox, so a wider scope would fail
// every test that needs bd there.
func TestC1ReplayRequireIsStepScoped(t *testing.T) {
	job := forkQualLoad(t).job(t, forkQualCensusJob)
	at, _ := forkQualCensusStep(t, job)
	want := fmt.Sprintf("%s:.jobs.%s.steps[%d].env.%s", forkQualFile, forkQualCensusJob, at, forkQualReplayKey)

	var got []string
	for _, name := range forkWorkflowFileNames(t) {
		walkYAML(readYAMLNode(t, filepath.Join(".github", "workflows", name)), "", func(where string, _ bool, value string) {
			if strings.Contains(value, forkQualReplayKey) {
				got = append(got, name+":"+where)
			}
		})
	}
	if !slices.Equal(got, []string{want}) {
		t.Errorf("%s appears at %q, want only at %q", forkQualReplayKey, got, want)
	}
}

// C1.NoNewWorkflowFile: the replay harness's fail-loud lane lives in
// fork-qualification.yml, so no other workflow file mentions REPLAY_REQUIRE. A
// test sees a tree, not a diff, so this pins what a lane in a workflow file of
// its own would have to do.
func TestC1NoNewWorkflowFile(t *testing.T) {
	root := sourceRepoRoot(t)
	sawQualification := false
	for _, name := range forkWorkflowFileNames(t) {
		if name == forkQualFile {
			sawQualification = true
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), forkQualReplayKey) {
			t.Errorf("%s mentions %s; the replay harness's fail-loud lane lives in %s, so extend that file instead of adding the lane to another workflow", name, forkQualReplayKey, forkQualFile)
		}
	}
	if !sawQualification {
		t.Fatalf("no %s among the workflow files: W2 (be-67atam, ruling be-jvluf7 section 3.5) adds it, and the scan has nothing to compare against", forkQualFile)
	}
}

// C1.PRCoreCheckoutCarriesHistory: the census job checks out the full history.
// A replay scenario can build an older bd from a pinned commit, and with
// REPLAY_REQUIRE=1 (TestC1PRCoreSetsReplayRequire) a commit missing from the
// checkout fails the scenario instead of skipping it, so GitHub's default
// depth-1 checkout would fail such a scenario on every run. The job has exactly
// one actions/checkout step, so no second checkout can take the history away
// again.
func TestC1PRCoreCheckoutCarriesHistory(t *testing.T) {
	job := forkQualLoad(t).job(t, forkQualCensusJob)
	var checkouts []ciWorkflowStep
	for _, step := range job.Steps {
		if actionFamily(step.Uses) == "actions/checkout" {
			checkouts = append(checkouts, step)
		}
	}
	if len(checkouts) != 1 {
		t.Fatalf("%s job %s has %d actions/checkout steps, want exactly 1; a replay scenario that builds an older bd from a pinned commit needs that one checkout to carry the full history, and a second checkout could take it away again", forkQualFile, forkQualCensusJob, len(checkouts))
	}
	if got := checkouts[0].With["fetch-depth"]; got != "0" {
		t.Errorf("%s job %s checks out with fetch-depth %q, want \"0\" (an unset depth is GitHub's default of 1); a replay scenario that builds an older bd from a pinned commit needs that commit in the checkout, and REPLAY_REQUIRE=1 makes it a failure when it is missing", forkQualFile, forkQualCensusJob, got)
	}
}

// The graph job moves here from pr.yml unchanged: it keeps its display name
// (the verifier test finds the job by that exact line), its timeout, its two
// qualification steps in their order, and downloads the fork's artifact.
func TestForkQualificationKeepsGraphCore(t *testing.T) {
	data := forkQualRead(t)
	job := forkQualLoad(t).job(t, forkQualGraphJob)
	const id = forkQualGraphJob

	if job.Name != forkQualGraphName {
		t.Errorf("job %s is named %q, want %q verbatim, with no %q prefix: %s finds the job by that name", id, job.Name, forkQualGraphName, forkQualBuildPre, forkQualVerifier)
	}
	nameLine := "    name: " + forkQualGraphName
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == nameLine {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%s has %d lines %q, want exactly 1: %s reads the job's timeout by finding that line, so it must occur once, indented as a job's name", forkQualFile, count, nameLine, forkQualVerifier)
	}
	if !slices.Equal([]string(job.Needs), []string{forkQualBuildJob}) {
		t.Errorf("job %s needs %v, want exactly [%s]: the fork's own build job, not pr.yml's", id, []string(job.Needs), forkQualBuildJob)
	}
	if job.TimeoutMinutes != forkQualGraphMins {
		t.Errorf("job %s has timeout-minutes %d, want %d: the verifier test pins the cap to at least 1.75 times the slowest measured job", id, job.TimeoutMinutes, forkQualGraphMins)
	}
	if !forkQualHostedRunnerRe.MatchString(job.RunsOn) {
		t.Errorf("job %s runs on %q, want a GitHub-hosted ubuntu runner (R6 v4 rule 4)", id, job.RunsOn)
	}
	if job.If != "" || job.ContinueOnError {
		t.Errorf("job %s must run unconditionally (if %q, continue-on-error %v): a skipped or optional graph job is the gap this workflow closes", id, job.If, job.ContinueOnError)
	}

	downloads := forkQualSteps(job, forkQualUses("actions/download-artifact"))
	if len(downloads) != 1 || job.Steps[downloads[0]].With["name"] != forkQualArtifact {
		t.Errorf("job %s must download exactly one artifact, named %q (upstream owns ci-build-artifacts); its download steps: %v", id, forkQualArtifact, downloads)
	}
	verifier := forkQualOne(t, id, job, "run "+path.Base(forkQualVerifier), forkQualRuns(path.Base(forkQualVerifier)))
	qualify := forkQualOne(t, id, job, "run "+forkQualQualify, forkQualRuns(forkQualQualify))
	if verifier >= qualify {
		t.Errorf("job %s runs %s (step %d) after %s (step %d); the verifier's negative controls run first, so a broken verifier cannot pass the qualification", id, path.Base(forkQualVerifier), verifier, forkQualQualify, qualify)
	}
	for _, i := range []int{verifier, qualify} {
		if step := job.Steps[i]; step.If != "" || forkQualContinues(step.ContinueOnError) {
			t.Errorf("job %s step %q must be nonoptional (if %q, continue-on-error %v)", id, step.Name, step.If, step.ContinueOnError)
		}
	}

	// The job lives here and nowhere else.
	for _, name := range forkWorkflowFileNames(t) {
		if name == forkQualFile {
			continue
		}
		if other, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), ".github", "workflows", name)); err != nil {
			t.Fatal(err)
		} else if strings.Contains(string(other), forkQualGraphName) {
			t.Errorf("%s names the graph job %q; it lives in %s only, and must stay out of CI Gate's needs", name, forkQualGraphName, forkQualFile)
		}
	}
}

// scripts/ci/graph-c0-verifier-test.py reads the graph job's timeout out of the
// workflow file as plain text. When the job moved out of pr.yml the path in that
// reader had to follow; this runs the verifier test that reads it (the whole
// file is 44 tests and the graph job runs it first), and pins that the reader
// names the new file and not the old one.
func TestGraphC0VerifierPassesAgainstForkQualification(t *testing.T) {
	python := requireHostTool(t, "python3")
	root := sourceRepoRoot(t)

	source := readPolicyFile(t, root, forkQualVerifier)
	if !strings.Contains(source, ".github/workflows/"+forkQualFile) {
		t.Errorf("%s does not read .github/workflows/%s: job_timeout_minutes must follow the graph job out of pr.yml (ruling be-jvluf7 section 3.5)", forkQualVerifier, forkQualFile)
	}
	if strings.Contains(source, "workflows/pr.yml") {
		t.Errorf("%s still reads workflows/pr.yml; the graph job is not there any more", forkQualVerifier)
	}

	// The verifier opens the workflow file; until W2 adds it, say so here and not
	// through the verifier's traceback.
	forkQualRead(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, forkQualVerifier, forkQualVerifierTest)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("python3 %s %s: %v\n%s", forkQualVerifier, forkQualVerifierTest, err, out)
	} else if !strings.Contains(string(out), "Ran 1 test") {
		t.Errorf("python3 %s %s ran something other than the one test:\n%s", forkQualVerifier, forkQualVerifierTest, out)
	}
}

// The fork's build job is lifted from pr.yml with three changes the ruling
// names: its display name gains the prefix "Fork ", it has no needs and no if
// (pr.yml's needed the bazel-coverage job), and it uploads fork-build-artifacts
// instead of ci-build-artifacts, a name upstream owns.
func TestForkQualificationBuildArtifactsJob(t *testing.T) {
	wf := forkQualLoad(t)
	job := wf.job(t, forkQualBuildJob)
	const id = forkQualBuildJob

	if !strings.HasPrefix(job.Name, forkQualBuildPre) || job.Name == forkQualBuildPre {
		t.Errorf("job %s is named %q, want a name with the prefix %q", id, job.Name, forkQualBuildPre)
	}
	if len(job.Needs) != 0 || job.If != "" || job.ContinueOnError {
		t.Errorf("job %s has needs %v, if %q, continue-on-error %v; it is the root of the workflow: no needs, no if", id, []string(job.Needs), job.If, job.ContinueOnError)
	}
	if !forkQualHostedRunnerRe.MatchString(job.RunsOn) {
		t.Errorf("job %s runs on %q, want a GitHub-hosted ubuntu runner (R6 v4 rule 4)", id, job.RunsOn)
	}

	build := forkQualOne(t, id, job, "run the lifted go build", forkQualRuns(
		"go build -tags gms_pure_go -o artifacts/"+forkQualBinary+" ./cmd/bd", "SHA256SUMS", "build-manifest.txt"))
	if step := job.Steps[build]; step.If != "" || forkQualContinues(step.ContinueOnError) {
		t.Errorf("job %s step %q must be nonoptional", id, step.Name)
	}
	upload := forkQualOne(t, id, job, "use actions/upload-artifact", forkQualUses("actions/upload-artifact"))
	if got := job.Steps[upload].With; got["name"] != forkQualArtifact || got["if-no-files-found"] != "error" {
		t.Errorf("job %s uploads %v, want name %q and if-no-files-found \"error\" (an empty artifact must fail the job, not the jobs downstream)", id, got, forkQualArtifact)
	}
	if build >= upload {
		t.Errorf("job %s uploads (step %d) before it builds (step %d)", id, upload, build)
	}

	// Upstream owns ci-build-artifacts (.bazelrc, bazel.yml, pr.yml,
	// scripts/ci/package-bazel-bd.sh and their tests); the fork's names are its own.
	walkYAML(readYAMLNode(t, ".github/workflows/"+forkQualFile), "", func(where string, _ bool, value string) {
		if strings.Contains(value, "ci-build-artifacts") {
			t.Errorf("%s %s mentions ci-build-artifacts, a name upstream owns; the fork's artifact is %s", forkQualFile, where, forkQualArtifact)
		}
	})
	for id, other := range wf.Jobs {
		for _, i := range forkQualSteps(other, forkQualUses("actions/download-artifact")) {
			if name := other.Steps[i].With["name"]; name != forkQualArtifact {
				t.Errorf("job %s step %d downloads %q, want %q", id, i, name, forkQualArtifact)
			}
		}
	}
}

// The census job's setup mirrors the pr-core-wrapper job it replaces, minus what
// only make ci-pr-core needed: the full history, Go from go.mod with the setup
// action's own cache off, the module cache restored (never the monolithic
// actions/cache, which the cache sweeps bar), dolt installed and git and dolt
// identities set (the replay tests shell out to both), and the fork's bd
// downloaded and checksum-verified before the one step that uses it.
func TestForkQualificationCensusSetup(t *testing.T) {
	job := forkQualLoad(t).job(t, forkQualCensusJob)
	const id = forkQualCensusJob
	census, step := forkQualCensusStep(t, job)

	checkout := forkQualOne(t, id, job, "use actions/checkout", forkQualUses("actions/checkout"))
	if checkout != 0 {
		t.Errorf("job %s checks out at step %d, want step 0: a checkout later in the job would delete what the steps before it set up", id, checkout)
	}
	setupGo := forkQualOne(t, id, job, "use actions/setup-go", forkQualUses("actions/setup-go"))
	if with := job.Steps[setupGo].With; with["go-version-file"] != "go.mod" || with["cache"] != "false" {
		t.Errorf("job %s sets up Go with %v, want go-version-file go.mod and cache false (the module cache is restored by its own step)", id, with)
	}
	moduleCache := forkQualOne(t, id, job, "restore the Go module cache with actions/cache/restore", func(s ciWorkflowStep) bool {
		return actionFamily(s.Uses) == "actions/cache/restore" && strings.Contains(s.With["path"], "pkg/mod")
	})
	installDolt := forkQualOne(t, id, job, "run scripts/ci/install-dolt.sh", forkQualRuns("scripts/ci/install-dolt.sh"))
	identity := forkQualOne(t, id, job, "set the git and dolt identities", forkQualRuns("git config --global", "dolt config --global", "user.name", "user.email"))
	download := forkQualOne(t, id, job, "use actions/download-artifact", forkQualUses("actions/download-artifact"))
	verify := forkQualOne(t, id, job, "verify the artifact's checksums", forkQualRuns("sha256sum", "SHA256SUMS"))

	for name, at := range map[string]int{"Go setup": setupGo, "module cache restore": moduleCache, "dolt install": installDolt, "identity step": identity, "verify": verify} {
		if at >= census {
			t.Errorf("job %s: the %s (step %d) must come before the go test step (step %d)", id, name, at, census)
		}
	}
	if !(download < verify && verify < census) {
		t.Errorf("job %s must download (step %d), then verify (step %d), then run the census (step %d)", id, download, verify, census)
	}

	dl := job.Steps[download].With
	if dl["name"] != forkQualArtifact {
		t.Errorf("job %s downloads %q, want %q", id, dl["name"], forkQualArtifact)
	}
	dir := dl["path"]
	if dir == "" {
		t.Fatalf("job %s downloads into the workspace root; give the download an explicit path so BEADS_TEST_BD_BINARY can name the verified file", id)
	}
	prefix := dir + "/"
	if !strings.HasPrefix(dir, "${{") {
		prefix = "${{ github.workspace }}/" + strings.TrimPrefix(dir, "./") + "/"
	}
	if got, want := step.Env["BEADS_TEST_BD_BINARY"], prefix+forkQualBinary; got != want {
		t.Errorf("the go test step's BEADS_TEST_BD_BINARY = %q, want %q: the replay tests must run the artifact the job verified", got, want)
	}
	for key, want := range map[string]string{forkQualReplayKey: "1", "BEADS_TEST_REQUIRE_EXCLUDE_PERMISSION": "1"} {
		if got := step.Env[key]; got != want {
			t.Errorf("the go test step's %s = %q, want %q", key, got, want)
		}
	}
	if step.Env["GOCACHE"] == "" {
		t.Errorf("the go test step sets no GOCACHE; the census builds with -race and must not share the non-race build cache")
	}
}

// The aggregator's job set: the four jobs and the aggregator, nothing else, and
// the aggregator needs all four, runs whatever they did, and cannot be made
// optional.
func TestForkQualificationJobSetAndAggregator(t *testing.T) {
	wf := forkQualLoad(t)
	aggregatorID, aggregator := forkQualAggregatorJob(t, wf)

	want := append(slices.Clone(forkQualJobIDs), aggregatorID)
	slices.Sort(want)
	if got := sortedKeys(wf.Jobs); !slices.Equal(got, want) {
		t.Errorf("%s jobs = %v, want %v: the four jobs of rulings be-jvluf7 section 3.5 and be-s847fi section 4.1, and the aggregator", forkQualFile, got, want)
	}
	needs := slices.Clone([]string(aggregator.Needs))
	slices.Sort(needs)
	if !slices.Equal(needs, forkQualJobIDs) {
		t.Errorf("aggregator %s needs %v, want %v", aggregatorID, needs, forkQualJobIDs)
	}
	if got := forkQualIf(aggregator.If); got != "always()" {
		t.Errorf("aggregator %s has if %q, want always(): it must report on a failed or cancelled need, not be skipped with it", aggregatorID, aggregator.If)
	}
	for id, job := range wf.Jobs {
		if !forkQualHostedRunnerRe.MatchString(job.RunsOn) {
			t.Errorf("job %s runs on %q, want a GitHub-hosted ubuntu runner (R6 v4 rule 4)", id, job.RunsOn)
		}
		if job.ContinueOnError {
			t.Errorf("job %s has continue-on-error: a job that cannot fail the run is not a qualification", id)
		}
		if id != aggregatorID && job.If != "" {
			t.Errorf("job %s has if %q; every job runs on every pull request, and a skipped job fails the aggregator", id, job.If)
		}
		for _, step := range job.Steps {
			if id == aggregatorID && forkQualContinues(step.ContinueOnError) {
				t.Errorf("aggregator step %q has continue-on-error %v", step.Name, step.ContinueOnError)
			}
		}
	}
}

// forkQualEval resolves ${{ needs.<job>.result }}, the one expression the
// aggregator simulation understands. Any other expression fails the test, so
// the simulation cannot silently drift from the workflow.
func forkQualEval(t *testing.T, where, text string, results map[string]string) string {
	t.Helper()
	text = forkQualNeedsResultRe.ReplaceAllStringFunc(text, func(expr string) string {
		id := forkQualNeedsResultRe.FindStringSubmatch(expr)[1]
		result, ok := results[id]
		if !ok {
			t.Fatalf("%s reads needs.%s.result, and %s is not a job in this file", where, id, id)
		}
		return result
	})
	if expr := forkQualExprRe.FindString(text); expr != "" {
		t.Fatalf("%s uses %s: the aggregator may read only ${{ needs.<job>.result }}, the one expression the simulation evaluates (fork-heavy-tiers.yml's aggregator is the shape to follow)", where, expr)
	}
	return text
}

// forkQualRunAggregator runs the aggregator's own run steps, under GitHub's bash
// flags, with every need at the result the scenario gives it, and reports
// whether the job would pass. Steps that use an action (the checkout) are
// skipped: the files are already here.
func forkQualRunAggregator(t *testing.T, bash, id string, job ciWorkflowJob, results map[string]string) (bool, string) {
	t.Helper()
	root := sourceRepoRoot(t)
	var log strings.Builder
	ran := 0
	for i, step := range job.Steps {
		where := fmt.Sprintf("aggregator %s step %d (%q)", id, i, step.Name)
		if step.Uses != "" {
			continue
		}
		switch forkQualIf(step.If) {
		case "", "always()":
		default:
			t.Fatalf("%s has if: %s; the aggregator's steps may be unconditional or always() only", where, step.If)
		}
		flags := []string{"--noprofile", "--norc", "-e"}
		switch step.Shell {
		case "":
		case "bash":
			flags = []string{"--noprofile", "--norc", "-eo", "pipefail"}
		default:
			t.Fatalf("%s has shell: %s; the aggregator's steps run under bash", where, step.Shell)
		}
		env := []string{"PATH=" + os.Getenv("PATH"), "GITHUB_WORKSPACE=" + root}
		for _, vars := range []map[string]string{job.Env, step.Env} {
			for key, value := range vars {
				env = append(env, key+"="+forkQualEval(t, where+" env "+key, value, results))
			}
		}
		cmd := exec.Command(bash, append(flags, "-c", forkQualEval(t, where+" run", step.Run, results))...)
		cmd.Dir = root
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		log.Write(out)
		ran++
		if err != nil {
			return false, log.String()
		}
	}
	if ran == 0 {
		t.Fatalf("aggregator %s has no run step, so nothing evaluates its needs", id)
	}
	return true, log.String()
}

// forkQualAggregatorViolations runs the aggregator against every scenario of the
// ruling: all needs succeeded (it must pass), and each need failed, was cancelled
// or was skipped (it must fail).
func forkQualAggregatorViolations(t *testing.T, bash, id string, job ciWorkflowJob) []string {
	t.Helper()
	scenario := func(need, result string) map[string]string {
		results := map[string]string{}
		for _, id := range forkQualJobIDs {
			results[id] = "success"
		}
		if need != "" {
			results[need] = result
		}
		return results
	}
	var violations []string
	if ok, out := forkQualRunAggregator(t, bash, id, job, scenario("", "")); !ok {
		violations = append(violations, fmt.Sprintf("the aggregator fails although all %d jobs succeeded:\n%s", len(forkQualJobIDs), out))
	}
	for _, need := range forkQualJobIDs {
		for _, result := range []string{"failure", "cancelled", "skipped"} {
			if ok, out := forkQualRunAggregator(t, bash, id, job, scenario(need, result)); ok {
				violations = append(violations, fmt.Sprintf("the aggregator passes when needs.%s.result is %s; it must fail unless every job's result is exactly success (skipped is not acceptable):\n%s", need, result, out))
			}
		}
	}
	return violations
}

// The aggregator fails on any non-success need, skipped included: the only way
// to a green Fork Qualification / Required is that all four jobs ran and
// passed. A skipped qualification job is exactly the silent gap this workflow
// exists to close.
func TestForkQualificationAggregatorFailsUnlessEveryNeedSucceeded(t *testing.T) {
	wf := forkQualLoad(t)
	bash := requireHostTool(t, "bash")
	id, aggregator := forkQualAggregatorJob(t, wf)
	for _, v := range forkQualAggregatorViolations(t, bash, id, aggregator) {
		t.Error(v)
	}
}

// forkQualGateJob is a correct aggregator, built in memory: the shape
// fork-heavy-tiers.yml uses, over this workflow's four jobs. skippedOK names
// the variables it lets be skipped.
func forkQualGateJob(skippedOK string, run string) ciWorkflowJob {
	env := map[string]string{"CI_GATE_NAME": "Fork Qualification", "CI_GATE_SKIPPED_OK": skippedOK}
	var required []string
	for _, id := range forkQualJobIDs {
		name := strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
		required = append(required, name)
		env[name] = "${{ needs." + id + ".result }}"
	}
	env["CI_GATE_REQUIRED"] = strings.Join(required, " ")
	return ciWorkflowJob{
		Name:   forkQualAggregator,
		Needs:  forkQualJobIDs,
		If:     "${{ always() }}",
		RunsOn: "ubuntu-latest",
		Steps: []ciWorkflowStep{
			{Uses: "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"},
			{Name: "Evaluate CI gate", Env: env, Run: run},
		},
	}
}

// The aggregator check can fail: a correct aggregator has no violation, one
// whose step does not evaluate its needs has one for every non-success result of
// every need, and one that tolerates a skipped graph job has exactly that one.
func TestForkQualificationAggregatorControls(t *testing.T) {
	bash := requireHostTool(t, "bash")
	const gate = "bash " + forkQualGate

	if got := forkQualAggregatorViolations(t, bash, "gate", forkQualGateJob("", gate)); len(got) != 0 {
		t.Errorf("a correct aggregator has violations: %v", got)
	}
	if got, want := len(forkQualAggregatorViolations(t, bash, "gate", forkQualGateJob("", "true"))), 3*len(forkQualJobIDs); got != want {
		t.Errorf("an aggregator that evaluates nothing has %d violations, want %d (every non-success result of every need)", got, want)
	}
	got := forkQualAggregatorViolations(t, bash, "gate", forkQualGateJob("GRAPH_C0", gate))
	if len(got) != 1 || !strings.Contains(got[0], "needs.graph-c0.result is skipped") {
		t.Errorf("an aggregator that tolerates a skipped graph job has violations %v, want exactly the skipped graph-c0 one", got)
	}
}

// The workflow runs pull-request code, so like the other fork-owned workflows it
// may only run unprivileged: triggers that carry no secrets, on pull requests
// into integration (a path filter would let a qualification go unrun, and the
// generic fork_integration_trigger_pin_test.go pins the branch list for every
// fork-* file), a read-only token, and no job that widens its own permissions.
func TestForkQualificationRunsUnprivilegedOnIntegrationPullRequests(t *testing.T) {
	var doc struct {
		On          map[string]any `yaml:"on"`
		Permissions map[string]any `yaml:"permissions"`
		Jobs        map[string]struct {
			Permissions any `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(forkQualRead(t), &doc); err != nil {
		t.Fatalf("parse %s: %v", forkQualFile, err)
	}

	for trigger := range doc.On {
		if trigger != "pull_request" && trigger != "workflow_dispatch" {
			t.Errorf("%s has the trigger %q; the workflow runs pull-request code, so only pull_request (and a manual workflow_dispatch) are allowed: never pull_request_target, workflow_run or merge_group", forkQualFile, trigger)
		}
	}
	if pr, ok := doc.On["pull_request"]; !ok {
		t.Errorf("%s has no pull_request trigger; it is a pull-request qualification", forkQualFile)
	} else if want := map[string]any{"branches": []any{"integration"}}; !reflect.DeepEqual(pr, want) {
		t.Errorf("%s pull_request trigger = %v, want %v (no path filter, no event types)", forkQualFile, pr, want)
	}
	if !reflect.DeepEqual(doc.Permissions, map[string]any{"contents": "read"}) {
		t.Errorf("%s permissions = %v, want exactly contents: read", forkQualFile, doc.Permissions)
	}
	for id, job := range doc.Jobs {
		if job.Permissions != nil {
			t.Errorf("%s job %s sets its own permissions %v", forkQualFile, id, job.Permissions)
		}
	}
}

// Text the ruling bars from the file. Upstream's sweeps read every workflow file
// as text and fork-heavy-tiers.yml already passes them, so this file has to as
// well; the same words are barred from all fork-owned workflows by R6 (be-4y7b5s
// section 5.1 rule 4), which says "NO Blacksmith text", not "no Blacksmith
// runner". Comments count. And no make call: upstream's
// TestWorkflowsNameTheirTestEngine flags the ones that run the primary engine,
// and the census is not a make target.
func TestForkQualificationNamesNothingTheRulingBars(t *testing.T) {
	text := strings.ToLower(string(forkQualRead(t)))
	for _, barred := range forkQualBarredText {
		if i := strings.Index(text, barred); i >= 0 {
			t.Errorf("%s:%d names %q; the ruling and R6 bar it from the file, comments included", forkQualFile, strings.Count(text[:i], "\n")+1, barred)
		}
	}
	walkYAML(readYAMLNode(t, ".github/workflows/"+forkQualFile), "", func(where string, key bool, value string) {
		if key {
			return
		}
		for _, line := range forkQualRunLines(value) {
			if forkQualMakeRe.MatchString(line) {
				t.Errorf("%s %s calls make (%q); the fork's qualification jobs call no make target (ruling be-jvluf7 section 3.5)", forkQualFile, where, strings.TrimSpace(line))
			}
		}
	})
}

// forkQualBazelFile reports whether a repository file configures Bazel: a BUILD
// or MODULE file, a .bzl or .bazelrc file, or anything under tools/bazel.
func forkQualBazelFile(rel string) bool {
	base := path.Base(rel)
	return base == "BUILD" || base == "BUILD.bazel" || base == "MODULE.bazel" ||
		strings.HasPrefix(base, "WORKSPACE") || strings.HasPrefix(base, ".bazelrc") ||
		strings.HasSuffix(base, ".bzl") || strings.HasPrefix(rel, "tools/bazel/")
}

// REPLAY_REQUIRE=1 turns a missing dependency from a skip into a failure, and
// replaytest needs the dolt CLI and a real bd (NeedBd is never available inside
// a Bazel sandbox), so the variable must never reach a Bazel target: not in a
// BUILD file's env, not in a .bazelrc's test_env, not in anything that writes
// one, and not through a Bazel step of the census job (workflow mentions of the
// variable are pinned by TestC1ReplayRequireIsStepScoped).
func TestForkQualificationReplayRequireNeverReachesBazel(t *testing.T) {
	root := sourceRepoRoot(t)
	scanned := 0
	for _, rel := range repoFiles(t, root, 500) {
		if !forkQualBazelFile(rel) {
			continue
		}
		scanned++
		for i, line := range strings.Split(readPolicyFile(t, root, rel), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, forkQualReplayKey) {
				t.Errorf("%s:%d names %s (%q); REPLAY_REQUIRE=1 must never reach a Bazel target: replaytest cannot get bd inside a Bazel sandbox, so every test that needs it would fail", rel, i+1, forkQualReplayKey, strings.TrimSpace(line))
			}
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d Bazel files; the scan found nothing to check", scanned)
	}

	job := forkQualLoad(t).job(t, forkQualCensusJob)
	for i, step := range job.Steps {
		if forkQualBazelRe.MatchString(step.Uses) || forkQualBazelRe.MatchString(strings.Join(forkQualRunLines(step.Run), "\n")) {
			t.Errorf("job %s step %d (%q) uses Bazel; the census is a plain go test job", forkQualCensusJob, i, step.Name)
		}
	}
}

// The census's flags are pr-core.sh's: the ruling has the census mirror the
// go_test line there (-race -short -timeout=30m -skip '^TestEmbedded', plus
// parallelism). forkQualCensusFlags holds those flags and the harness holds the
// census to them; this test fails when a sync changes the line they come from,
// so the table is re-derived from pr-core.sh instead of drifting from it.
func TestForkQualificationCensusFlagsMirrorPRCore(t *testing.T) {
	var calls []string
	for _, line := range strings.Split(readPolicyFile(t, sourceRepoRoot(t), forkQualPRCore), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "go_test -") {
			calls = append(calls, line)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("%s has %d `go_test -...` lines, want exactly 1: the sync changed pr-core.sh, and the census flags of ruling be-jvluf7 section 3.5 must be re-derived from it; take it to the architect", forkQualPRCore, len(calls))
	}
	for _, problem := range forkQualFlagProblems(forkQualParseGoTest(forkQualShellWords(calls[0]))) {
		t.Errorf("%s: %s in %q: the census mirrors that line, so re-derive forkQualCensusFlags from it; any deviation needs a one-line justification in the pull request body", forkQualPRCore, problem, calls[0])
	}
}

// The census step's own script is the thing to pin, and matching its text proves
// little about a shell script: a hand-kept list, an empty-list guard that is
// missing or tests the wrong list, a package match that is too loose or too
// narrow, and a pipe that masks go test's status all read as plausible YAML. So
// the tests run the script, as GitHub would run it (bash -e, or -eo pipefail
// under shell: bash), in small fixture modules shaped like the cases the ruling
// names, with a shim as `go` that records the arguments of `go test`, answers
// with a chosen exit status and hands every other subcommand (go list) to the
// real go. What the script must do there is what ruling be-jvluf7 section 3.5
// says the census does:
//
//   - run go test over ./internal/replay/... and over every package whose test or
//     external test imports a path ending /internal/replay/replaytest, found at
//     run time and printed, and over nothing else (the census is narrow);
//   - carry the go test flags of pr-core.sh;
//   - exit non-zero when go test does, whatever sits between the two;
//   - exit non-zero, running nothing, when ./internal/replay/... lists no package
//     or the computed list is empty: a renamed harness must not pass with
//     nothing run.
//
// The step is run as written. Only ${{ github.workspace }} and ${{ runner.temp }}
// are evaluated, and nothing the step calls (a helper script, the output of
// another step) is there, so the discovery, its guards and the go test call live
// in the one step that sets REPLAY_REQUIRE; a go test -json summary for the job
// page is a separate always() step. A builder who needs another shape asks the
// architect, and the harness grows to run it.
//
// What this does not stand in for: the run that shows the replay tests failing,
// not skipping, under REPLAY_REQUIRE=1 with dolt absent from PATH (the builder
// pastes it), and a run of this harness under Bazel (it shells out to a fixture
// module and has only been run under go test).

const forkQualFixtureModule = "example.com/fixture"

// The shim records each `go test` call (a marker, then every argument, each NUL
// terminated) and exits with FORK_QUAL_GO_TEST_RC; anything else is the real go.
const forkQualGoShim = `#!/bin/bash
if [ "${1:-}" = test ]; then
  {
    printf '\001CALL\001\0'
    for arg in "$@"; do
      printf '%s\0' "$arg"
    done
  } >> "$FORK_QUAL_GO_LOG"
  exit "${FORK_QUAL_GO_TEST_RC:-0}"
fi
exec "$FORK_QUAL_REAL_GO" "$@"
`

// A mirror of the repository's .buildflags, so a step that sources it finds one.
const forkQualBuildflags = `: "${CGO_ENABLED:=1}"; export CGO_ENABLED
export BEADS_BUILD_TAGS="gms_pure_go"
if [[ "${GOFLAGS:-}" != *gms_pure_go* ]]; then export GOFLAGS="${GOFLAGS:+$GOFLAGS }-tags=${BEADS_BUILD_TAGS}"; fi
`

var (
	forkQualWorkspaceExprRe  = regexp.MustCompile(`\$\{\{\s*github\.workspace\s*\}\}`)
	forkQualRunnerTempExprRe = regexp.MustCompile(`\$\{\{\s*runner\.temp\s*\}\}`)
)

// The go test flags that take their value from the next argument. Every other
// flag is a boolean, so `-race ./x` does not read ./x as the race flag's value.
var forkQualValueFlags = map[string]bool{
	"p": true, "parallel": true, "timeout": true, "skip": true, "run": true, "count": true, "tags": true,
	"cpu": true, "shuffle": true, "vet": true, "coverprofile": true, "covermode": true, "coverpkg": true,
	"bench": true, "benchtime": true, "o": true, "exec": true, "ldflags": true, "gcflags": true, "mod": true,
}

// forkQualCensusFlags are the go test flags of pr-core.sh's go_test line that the
// census mirrors (parallelism is free): "" is a boolean flag.
var forkQualCensusFlags = map[string]string{"race": "", "short": "", "timeout": "30m", "skip": "^TestEmbedded"}

type forkQualGoTest struct {
	flags    map[string]string
	packages []string
}

// forkQualParseGoTest reads a go test command line; words[0] is the command.
func forkQualParseGoTest(words []string) forkQualGoTest {
	call := forkQualGoTest{flags: map[string]string{}}
	for i := 1; i < len(words); i++ {
		word := words[i]
		if !strings.HasPrefix(word, "-") || word == "-" {
			call.packages = append(call.packages, word)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(word, "-"), "=")
		if !hasValue && forkQualValueFlags[name] && i+1 < len(words) {
			i++
			value = words[i]
		}
		call.flags[name] = value
	}
	return call
}

// forkQualFlagProblems lists what a go test call lacks of forkQualCensusFlags.
func forkQualFlagProblems(call forkQualGoTest) []string {
	var problems []string
	for _, name := range sortedKeys(forkQualCensusFlags) {
		want := forkQualCensusFlags[name]
		got, ok := call.flags[name]
		switch {
		case !ok:
			problems = append(problems, "-"+name+" is missing")
		case want == "" && got != "" && got != "true":
			problems = append(problems, fmt.Sprintf("-%s=%s turns the flag off", name, got))
		case want != "" && got != want:
			problems = append(problems, fmt.Sprintf("-%s is %q, want %q", name, got, want))
		}
	}
	return problems
}

// forkQualShellWords splits a shell line into words, honoring single and double
// quotes: enough to read a flag list, not a shell.
func forkQualShellWords(line string) []string {
	var words []string
	var word strings.Builder
	var quote rune
	inWord := false
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// forkQualRel turns an import path or ./ path into a path relative to the module.
func forkQualRel(pkg string) string {
	pkg = strings.TrimPrefix(pkg, forkQualFixtureModule+"/")
	pkg = strings.TrimPrefix(pkg, "./")
	return strings.TrimSuffix(pkg, "/")
}

// forkQualPrinted reports whether output names the package, as an import path, a
// ./ path or a bare relative path.
func forkQualPrinted(output, rel string) bool {
	tokens := strings.FieldsFunc(output, func(r rune) bool {
		return unicode.IsSpace(r) || r == '"' || r == '\'' || r == ','
	})
	for _, token := range tokens {
		if token == rel || token == "./"+rel || token == forkQualFixtureModule+"/"+rel {
			return true
		}
	}
	return false
}

func forkQualTail(out []byte) string {
	const limit = 1500
	text := strings.TrimSpace(string(out))
	if len(text) > limit {
		text = "..." + text[len(text)-limit:]
	}
	return text
}

// forkQualReadCalls reads the shim's log: one []string per `go test` call.
func forkQualReadCalls(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) || (err == nil && len(data) == 0) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, field := range strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00") {
		if field == "\x01CALL\x01" {
			calls = append(calls, nil)
			continue
		}
		if len(calls) == 0 {
			t.Fatalf("go shim log starts with %q, not a call marker", field)
		}
		calls[len(calls)-1] = append(calls[len(calls)-1], field)
	}
	return calls
}

// The host tools the harness runs the step with.
type forkQualTools struct {
	bash, goBin, goroot string
}

func forkQualToolchain(t *testing.T) forkQualTools {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the census step runs under bash with a go shim script")
	}
	bash := requireHostTool(t, "bash")
	goBin := testGo(t)
	out, err := exec.Command(goBin, "env", "GOROOT").Output()
	if err != nil {
		t.Fatalf("%s env GOROOT: %v", goBin, err)
	}
	return forkQualTools{bash: bash, goBin: goBin, goroot: strings.TrimSpace(string(out))}
}

type forkQualKind int

const (
	forkQualPasses     forkQualKind = iota // go test passes: the step exits 0 and runs exactly want
	forkQualRefuses                        // nothing to census: the step exits non-zero without running go test
	forkQualPropagates                     // go test fails: the step exits non-zero
)

// A fixture module the census step runs in, and what the step must do with it.
type forkQualScenario struct {
	name   string
	why    string
	kind   forkQualKind
	files  map[string]string
	testRC int
	want   []string
}

type forkQualViolation struct{ scenario, text string }

func (v forkQualViolation) String() string { return "scenario " + v.scenario + ": " + v.text }

// forkQualGoFile is a Go source file of package pkg that imports each module-
// relative path for its side effects.
func forkQualGoFile(pkg string, imports ...string) string {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n")
	for _, imp := range imports {
		fmt.Fprintf(&b, "\nimport _ %q\n", forkQualFixtureModule+"/"+imp)
	}
	return b.String()
}

func forkQualModule(files map[string]string) map[string]string {
	files["go.mod"] = "module " + forkQualFixtureModule + "\n\ngo 1.21\n"
	return files
}

const forkQualReplaytest = "internal/replay/replaytest"

func forkQualScenarios() []forkQualScenario {
	a := forkQualModule(map[string]string{
		"internal/replay/replaytest/replaytest.go":    forkQualGoFile("replaytest"),
		"internal/replay/driver/driver.go":            forkQualGoFile("driver"),
		"internal/replay/driver/driver_test.go":       forkQualGoFile("driver", forkQualReplaytest),
		"internal/replay/scenarios/compat/compat.go":  forkQualGoFile("compat"),
		"internal/consumer/consumer.go":               forkQualGoFile("consumer"),
		"internal/consumer/consumer_test.go":          forkQualGoFile("consumer", forkQualReplaytest),
		"internal/xconsumer/xconsumer.go":             forkQualGoFile("xconsumer"),
		"internal/xconsumer/xconsumer_test.go":        forkQualGoFile("xconsumer_test", forkQualReplaytest),
		"internal/bystander/bystander.go":             forkQualGoFile("bystander"),
		"internal/bystander/bystander_test.go":        forkQualGoFile("bystander"),
		"internal/replayish/replaytest/replaytest.go": forkQualGoFile("replaytest"),
		"internal/lookalike/lookalike.go":             forkQualGoFile("lookalike"),
		"internal/lookalike/lookalike_test.go":        forkQualGoFile("lookalike", "internal/replayish/replaytest"),
	})
	b := forkQualModule(map[string]string{
		"internal/replay/replaytest/replaytest.go": forkQualGoFile("replaytest"),
		"internal/replay/deep/er/er.go":            forkQualGoFile("er"),
		"cmd/tool/main.go":                         forkQualGoFile("main") + "\nfunc main() {}\n",
		"cmd/tool/main_test.go":                    forkQualGoFile("main", forkQualReplaytest),
		"pkg/ext/ext.go":                           forkQualGoFile("ext"),
		"pkg/ext/ext_test.go":                      forkQualGoFile("ext_test", forkQualReplaytest),
		"pkg/replaytest/replaytest.go":             forkQualGoFile("replaytest"),
		"pkg/user/user.go":                         forkQualGoFile("user"),
		"pkg/user/user_test.go":                    forkQualGoFile("user", "pkg/replaytest"),
	})
	empty := forkQualModule(map[string]string{
		"internal/replay/NOTES.txt": "the replay harness moved\n",
		"internal/x/x.go":           forkQualGoFile("x"),
	})
	moved := forkQualModule(map[string]string{
		"internal/replay/NOTES.txt":                          "the replay harness moved\n",
		"vendorish/internal/replay/replaytest/replaytest.go": forkQualGoFile("replaytest"),
		"vendorish/consumer/consumer.go":                     forkQualGoFile("consumer"),
		"vendorish/consumer/consumer_test.go":                forkQualGoFile("consumer", "vendorish/internal/replay/replaytest"),
	})
	return []forkQualScenario{
		{
			name: "P-A", kind: forkQualPasses, files: a,
			why:  "internal/replay holds three packages; one test and one external test elsewhere import replaytest; look-alike paths (internal/replayish/replaytest) and a bystander do not count",
			want: []string{"internal/replay/replaytest", "internal/replay/driver", "internal/replay/scenarios/compat", "internal/consumer", "internal/xconsumer"},
		},
		{
			name: "P-B", kind: forkQualPasses, files: b,
			why:  "a deeper replay package; importers in package main and in an external test package of a library; pkg/replaytest is a look-alike",
			want: []string{"internal/replay/replaytest", "internal/replay/deep/er", "cmd/tool", "pkg/ext"},
		},
		{
			name: "E", kind: forkQualRefuses, files: empty,
			why: "internal/replay holds no package and nothing imports replaytest (the harness was renamed away)",
		},
		{
			name: "E2", kind: forkQualRefuses, files: moved,
			why: "internal/replay holds no package although a package elsewhere imports a path ending /internal/replay/replaytest (the rename canary: the computed list alone is not enough)",
		},
		{
			name: "G", kind: forkQualPropagates, files: a, testRC: 1,
			why: "go test exits 1",
		},
	}
}

// forkQualExpand turns a go test package argument into module-relative paths,
// expanding ... patterns with the real go.
func forkQualExpand(tools forkQualTools, fixture string, env []string, pkg string) []string {
	if !strings.Contains(pkg, "...") {
		return []string{forkQualRel(pkg)}
	}
	cmd := exec.Command(tools.goBin, "list", "-e", "-f", "{{.ImportPath}}", pkg)
	cmd.Dir = fixture
	cmd.Env = env
	out, _ := cmd.Output()
	var rels []string
	for _, line := range strings.Fields(string(out)) {
		rels = append(rels, forkQualRel(line))
	}
	return rels
}

// forkQualRunScenario runs the census step in one fixture and returns what the
// step got wrong there.
func forkQualRunScenario(t *testing.T, tools forkQualTools, scratch string, sc forkQualScenario, wfEnv, jobEnv map[string]string, step ciWorkflowStep) []string {
	t.Helper()
	dir := filepath.Join(scratch, sc.name)
	fixture := filepath.Join(dir, "fixture")
	runnerTemp := filepath.Join(dir, "runner-temp")
	shimDir := filepath.Join(dir, "shim")
	home := filepath.Join(dir, "home")
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range sc.files {
		write(filepath.Join(fixture, filepath.FromSlash(rel)), content, 0o644)
	}
	write(filepath.Join(fixture, ".buildflags"), forkQualBuildflags, 0o644)
	write(filepath.Join(shimDir, "go"), forkQualGoShim, 0o755)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"step-summary", "output", "env"} {
		write(filepath.Join(runnerTemp, name), "", 0o644)
	}
	logPath := filepath.Join(runnerTemp, "go-test-calls")

	evaluate := func(text string) (string, string) {
		text = forkQualWorkspaceExprRe.ReplaceAllLiteralString(text, fixture)
		text = forkQualRunnerTempExprRe.ReplaceAllLiteralString(text, runnerTemp)
		return text, forkQualExprRe.FindString(text)
	}
	strict := func(where, text string) string {
		t.Helper()
		text, expr := evaluate(text)
		if expr != "" {
			t.Fatalf("the census step's %s uses %s: the pin evaluates only ${{ github.workspace }} and ${{ runner.temp }}; set anything else as a plain value, or ask the architect for a shape the pin can run", where, expr)
		}
		return text
	}

	vars := map[string]string{
		"HOME": home, "GOPATH": filepath.Join(scratch, "gopath"), "GOCACHE": filepath.Join(scratch, "gocache"),
		"GOTOOLCHAIN": "local", "GOPROXY": "off", "GOFLAGS": "", "GOWORK": "off", "GOROOT": tools.goroot,
		"RUNNER_OS": "Linux", "GITHUB_STEP_SUMMARY": filepath.Join(runnerTemp, "step-summary"),
		"GITHUB_OUTPUT": filepath.Join(runnerTemp, "output"), "GITHUB_ENV": filepath.Join(runnerTemp, "env"),
	}
	for _, layer := range []map[string]string{wfEnv, jobEnv} {
		for key, value := range layer {
			if text, expr := evaluate(value); expr == "" {
				vars[key] = text
			}
		}
	}
	for key, value := range step.Env {
		vars[key] = strict("env "+key, value)
	}
	vars["PATH"] = shimDir + string(os.PathListSeparator) + os.Getenv("PATH")
	vars["GITHUB_WORKSPACE"] = fixture
	vars["RUNNER_TEMP"] = runnerTemp
	vars["FORK_QUAL_GO_LOG"] = logPath
	vars["FORK_QUAL_GO_TEST_RC"] = fmt.Sprint(sc.testRC)
	vars["FORK_QUAL_REAL_GO"] = tools.goBin
	var env []string
	for _, key := range sortedKeys(vars) {
		env = append(env, key+"="+vars[key])
	}
	script := strict("run script", step.Run)

	// A step that checks the binary it was told to run finds a stub where the
	// artifact download would have put it.
	if bin := vars["BEADS_TEST_BD_BINARY"]; strings.HasPrefix(bin, fixture+string(filepath.Separator)) || strings.HasPrefix(bin, runnerTemp+string(filepath.Separator)) {
		write(bin, "#!/bin/sh\nexit 0\n", 0o755)
	}

	flags := []string{"--noprofile", "--norc", "-e"}
	switch step.Shell {
	case "":
	case "bash":
		flags = []string{"--noprofile", "--norc", "-eo", "pipefail"}
	default:
		t.Fatalf("the census step has shell: %s; it runs under bash, the one shell the pin evaluates", step.Shell)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tools.bash, append(flags, "-c", script)...)
	cmd.Dir = fixture
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	code := exitCode(err)

	var parsed []forkQualGoTest
	for _, call := range forkQualReadCalls(t, logPath) {
		parsed = append(parsed, forkQualParseGoTest(call))
	}
	var found []string
	bad := func(format string, args ...any) { found = append(found, fmt.Sprintf(format, args...)) }
	if ctx.Err() != nil {
		bad("the step did not finish within 90s:\n%s", forkQualTail(out))
		return found
	}

	switch sc.kind {
	case forkQualRefuses:
		if code == 0 {
			bad("the step exits 0 although %s: it must fail, running nothing, when ./internal/replay/... lists no package or the computed list is empty; otherwise a renamed or moved harness passes with nothing run\n%s", sc.why, forkQualTail(out))
		}
		if len(parsed) > 0 {
			bad("the step runs go test (%d call(s)) although %s: with nothing to census it must fail first", len(parsed), sc.why)
		}
	case forkQualPropagates:
		if code == 0 {
			bad("the step exits 0 although %s: nothing between go test and the end of the step may mask its status (a pipe needs pipefail, and a summary step is a step of its own)\n%s", sc.why, forkQualTail(out))
		}
	case forkQualPasses:
		if code != 0 {
			bad("the step exits %d although go test succeeded and %s. A helper script, or the output of another step, is not available to the pin: the census lives in the one step, or the architect rules on another shape\n%s", code, sc.why, forkQualTail(out))
			return found
		}
		if len(parsed) == 0 {
			bad("the step ran no go test (%s)", sc.why)
			return found
		}
		got := map[string]bool{}
		for i, call := range parsed {
			for _, problem := range forkQualFlagProblems(call) {
				bad("go test call %d: %s; the census mirrors the go_test line of %s", i+1, problem, forkQualPRCore)
			}
			for _, pkg := range call.packages {
				for _, rel := range forkQualExpand(tools, fixture, env, pkg) {
					got[rel] = true
				}
			}
		}
		want := map[string]bool{}
		var missing, extra, unprinted []string
		for _, rel := range sc.want {
			want[rel] = true
			if !got[rel] {
				missing = append(missing, rel)
			}
			if !forkQualPrinted(string(out), rel) {
				unprinted = append(unprinted, rel)
			}
		}
		for rel := range got {
			if !want[rel] {
				extra = append(extra, rel)
			}
		}
		sort.Strings(extra)
		if len(missing) > 0 {
			bad("go test never runs %v (%s): the census is every package of ./internal/replay/... plus every package whose test or external test imports a path ending /internal/replay/replaytest, computed at run time", missing, sc.why)
		}
		if len(extra) > 0 {
			bad("go test also runs %v (%s): the census is narrow, it runs those packages and no others", extra, sc.why)
		}
		if len(unprinted) > 0 {
			bad("the step does not print %v: the computed list must appear in the job log, so a reader can see what the census ran", unprinted)
		}
	}
	return found
}

// forkQualCensusViolations runs the step in every scenario (or only the named
// ones) and returns what it got wrong.
func forkQualCensusViolations(t *testing.T, wfEnv, jobEnv map[string]string, step ciWorkflowStep, only ...string) []forkQualViolation {
	t.Helper()
	tools := forkQualToolchain(t)
	scratch := t.TempDir()
	var violations []forkQualViolation
	for _, sc := range forkQualScenarios() {
		if len(only) > 0 && !slices.Contains(only, sc.name) {
			continue
		}
		for _, text := range forkQualRunScenario(t, tools, scratch, sc, wfEnv, jobEnv, step) {
			violations = append(violations, forkQualViolation{scenario: sc.name, text: text})
		}
	}
	return violations
}

// The census step discovers, guards, prints and runs as the ruling says. This
// runs the step's own script against the fixtures of forkQualScenarios.
func TestForkQualificationCensusDiscoversTheReplayPackages(t *testing.T) {
	wf := forkQualLoad(t)
	job := wf.job(t, forkQualCensusJob)
	_, step := forkQualCensusStep(t, job)
	for _, v := range forkQualCensusViolations(t, wf.Env, job.Env, step) {
		t.Error(v)
	}
}

// The census script of the controls, in pieces so a control can leave one out.
const (
	forkQualSampleList = `replay_pkgs="$(go list ./internal/replay/...)"
`
	forkQualSampleCanary = `if [ -z "$replay_pkgs" ]; then
  echo "::error::go list ./internal/replay/... matched no packages: the replay harness moved or was renamed"
  exit 1
fi
`
	forkQualSampleImporters = `importers="$(go list -e -f '{{.ImportPath}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | awk '{ for (i = 2; i <= NF; i++) if ($i ~ /\/internal\/replay\/replaytest$/) { print $1; break } }')"
`
	forkQualSampleMerge = `packages="$(printf '%s\n%s\n' "$replay_pkgs" "$importers" | sed '/^$/d' | sort -u)"
`
	forkQualSampleGuard = `if [ -z "$packages" ]; then
  echo "::error::the replay census found no package to run"
  exit 1
fi
`
	forkQualSamplePrint = `echo "Replay census packages:"
echo "$packages"
`
	forkQualSampleRun = `go test -p 4 -parallel 4 -race -short -timeout=30m -skip '^TestEmbedded' $packages
`
	forkQualSampleFlags = `-p 4 -parallel 4 -race -short -timeout=30m -skip '^TestEmbedded'`

	forkQualSample = forkQualSampleList + forkQualSampleCanary + forkQualSampleImporters + forkQualSampleMerge +
		forkQualSampleGuard + forkQualSamplePrint + forkQualSampleRun
)

// forkQualVariant swaps the one occurrence of old in script for replacement.
func forkQualVariant(t *testing.T, script, old, replacement string) string {
	t.Helper()
	if n := strings.Count(script, old); n != 1 {
		t.Fatalf("control construction: %q occurs %d times in the sample census script, want 1", old, n)
	}
	return strings.Replace(script, old, replacement, 1)
}

// The census check can fail: the sample census script passes in every scenario,
// and each way the ruling warns about is caught by a scenario of its own. A
// control that is not flagged here means the real step could do the same and
// pass.
func TestForkQualificationCensusHarnessControls(t *testing.T) {
	step := func(run string) ciWorkflowStep { return ciWorkflowStep{Run: run} }

	if got := forkQualCensusViolations(t, nil, nil, step(forkQualSample)); len(got) != 0 {
		t.Errorf("the sample census script has violations: %v", got)
	}
	// A pipe through tee keeps go test's status only under pipefail, which
	// shell: bash provides.
	masked := forkQualVariant(t, forkQualSample, forkQualSampleRun,
		strings.TrimSuffix(forkQualSampleRun, "\n")+" | tee \"$RUNNER_TEMP/census.log\"\n")
	if got := forkQualCensusViolations(t, nil, nil, ciWorkflowStep{Run: masked, Shell: "bash"}, "P-A", "G"); len(got) != 0 {
		t.Errorf("a tee under shell: bash has violations: %v", got)
	}

	handKept := forkQualSampleList + forkQualSampleCanary +
		"packages=\"./internal/replay/... ./internal/consumer ./internal/xconsumer\"\n" +
		forkQualSampleGuard + forkQualSamplePrint + forkQualSampleRun
	noGuards := forkQualSampleList + forkQualSampleImporters + forkQualSampleMerge + forkQualSamplePrint + forkQualSampleRun
	noCanary := forkQualSampleList + forkQualSampleImporters + forkQualSampleMerge + forkQualSampleGuard + forkQualSamplePrint + forkQualSampleRun

	for _, tc := range []struct {
		name, flagged, script string
		shell                 string
	}{
		{"no guard on an empty list", "E", noGuards, ""},
		{"no check that ./internal/replay/... lists anything", "E2", noCanary, ""},
		{"a hand-kept list", "P-B", handKept, ""},
		{"test imports without external test imports", "P-A",
			forkQualVariant(t, forkQualSample, ` {{join .XTestImports " "}}`, ""), ""},
		{"a match on any path ending replaytest", "P-A",
			forkQualVariant(t, forkQualSample, `/\/internal\/replay\/replaytest$/`, `/replaytest$/`), ""},
		{"a hard-coded module path", "P-A",
			forkQualVariant(t, forkQualSample, `$i ~ /\/internal\/replay\/replaytest$/`, `$i == "github.com/steveyegge/beads/internal/replay/replaytest"`), ""},
		{"a pipe that masks go test's status", "G", masked, ""},
		{"go test without the pr-core flags", "P-A",
			forkQualVariant(t, forkQualSample, forkQualSampleFlags, `-p 4`), ""},
		{"a list that is not printed", "P-A",
			forkQualVariant(t, forkQualSample, forkQualSamplePrint, ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forkQualCensusViolations(t, nil, nil, ciWorkflowStep{Run: tc.script, Shell: tc.shell}, tc.flagged)
			if len(got) == 0 {
				t.Errorf("%s is not flagged by scenario %s: the census check cannot tell it from the sample", tc.name, tc.flagged)
				return
			}
			t.Logf("flagged: %s", got[0])
		})
	}
}

// The served HTTP tier (ruling be-s847fi, build be-vvsbdl). The binding values
// are B1 to B8 of section 4 of the ruling; the check step is its contract K1 to K6
// and Appendix B, which the controls below mutate.
const (
	forkQualServedName   = "Fork Served HTTP Tier"
	forkQualServedMins   = 60
	forkQualServedCheck  = "Check what ran"
	forkQualServedLog    = "SERVED_LOG"
	forkQualServedLogVal = "${{ runner.temp }}/served-http.log"
	forkQualServedCache  = "${{ runner.temp }}/go-cache/non-race"
	forkQualServedStrict = "set -euo pipefail"
	forkQualServedHTTP   = forkQualFixtureModule + "/backend/http"
	forkQualServedClient = forkQualFixtureModule + "/internal/httpclient"
	forkQualServedExtra  = forkQualFixtureModule + "/extra"

	// What a test that is a known divergence says, and the one rule the check
	// step has for a skip: nothing else explains one.
	forkQualServedDivergence = "KNOWN DIVERGENCE L-fixture (ga-0000): the wire has no such member"
)

var (
	// The variables that make the served tests run (the two embedded-Dolt
	// gates) and make a missing run fail (the required gate), set in the env of
	// the one go test step and nowhere else in the file.
	forkQualServedGates = []string{"BEADS_TEST_EMBEDDED_DOLT", "BEADS_HTTP_TEST_REQUIRED", "CGO_ENABLED"}

	forkQualServedRoots = []string{"./backend/http", "./internal/httpclient"}

	// Five tests that must be reported PASS: one for each way the tier can be
	// linked, fenced or run hollow.
	forkQualServedSentinels = []string{
		"TestE2E_BDOverHTTP",
		"TestE2E_PingOverHTTP",
		"TestServedTierIsLinkedWhenRequired",
		"TestServedParticipationGenerationFence",
		"TestServedReadsAndWritesRefuseAfterAPostHandshakeServerSwap",
	}

	// The logical go test line, word for word.
	forkQualServedCommand = slices.Concat(
		[]string{"go", "test", "-tags", "gms_pure_go", "-count=1", "-v", "-timeout=50m"},
		forkQualServedRoots,
		[]string{"2>&1", "|", "tee", "$" + forkQualServedLog})

	// The one call the check step makes to go.
	forkQualServedList = slices.Concat([]string{"list", "-tags", "gms_pure_go"}, forkQualServedRoots)
)

// forkQualLogicalLines returns the commands of a run script, one per element:
// comment lines and blank lines are left out and a line ending in a backslash
// joins the line after it.
func forkQualLogicalLines(run string) []string {
	var lines []string
	var open string
	for _, line := range forkQualRunLines(run) {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, `\`) {
			open += strings.TrimSpace(strings.TrimSuffix(line, `\`)) + " "
			continue
		}
		if line = strings.TrimSpace(open + line); line != "" {
			lines = append(lines, line)
		}
		open = ""
	}
	if open = strings.TrimSpace(open); open != "" {
		lines = append(lines, open)
	}
	return lines
}

// forkQualServedSteps returns the indexes of the served job's go test step (the
// one step that runs go test) and of its check step (the one step by that
// name).
func forkQualServedSteps(t *testing.T, job ciWorkflowJob) (test, check int) {
	t.Helper()
	tests := forkQualSteps(job, forkQualRuns("go test"))
	checks := forkQualSteps(job, func(step ciWorkflowStep) bool { return step.Name == forkQualServedCheck })
	if len(tests) != 1 {
		t.Fatalf("job %s has %d steps that run go test, want exactly 1: the step that runs both served packages (ruling be-s847fi section 4.2)", forkQualServedJob, len(tests))
	}
	if len(checks) != 1 {
		t.Fatalf("job %s has %d steps named %q, want exactly 1: the step that fails the job unless the log shows what ran (ruling be-s847fi section 4.3)", forkQualServedJob, len(checks), forkQualServedCheck)
	}
	return tests[0], checks[0]
}

// TestForkQualificationServedJobShape pins the job: its keys, the three
// variables that make the served tests run and make a missing run fail, the one
// go test line and the step after it. It works on the parsed YAML, so a comment
// changes nothing it reads.
func TestForkQualificationServedJobShape(t *testing.T) {
	const id = forkQualServedJob
	wf := forkQualLoad(t)
	job := wf.job(t, id)
	node := readYAMLNode(t, filepath.Join(".github", "workflows", forkQualFile))

	// B4: the job carries four keys. A needs, if, continue-on-error, strategy,
	// container, services, permissions or env at the job level would make the
	// job optional, wider or differently privileged than the ruling says.
	prefix := ".jobs." + id + "."
	var keys []string
	walkYAML(node, "", func(where string, key bool, _ string) {
		if rest, ok := strings.CutPrefix(where, prefix); ok && key && !strings.ContainsAny(rest, ".[") {
			keys = append(keys, rest)
		}
	})
	slices.Sort(keys)
	if want := []string{"name", "runs-on", "steps", "timeout-minutes"}; !slices.Equal(keys, want) {
		t.Errorf("job %s has the keys %v, want exactly %v: a job-level needs, if, continue-on-error, strategy, container, services, permissions or env would change what the job is (ruling be-s847fi B4)", id, keys, want)
	}
	if job.Name != forkQualServedName {
		t.Errorf("job %s is named %q, want %q (B1)", id, job.Name, forkQualServedName)
	}
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("job %s runs on %q, want ubuntu-latest (B2): the tests need a hosted Linux runner with a C toolchain", id, job.RunsOn)
	}
	if job.TimeoutMinutes != forkQualServedMins {
		t.Errorf("job %s has timeout-minutes %d, want %d (B3): a 50 minute go test and a margin, not the platform's 360", id, job.TimeoutMinutes, forkQualServedMins)
	}

	// B5: six steps, the actions the other jobs of the file already pin, no
	// Dolt install.
	wantUses := []string{"actions/checkout", "actions/setup-go", "actions/cache/restore", "actions/cache/restore", "", ""}
	var gotUses []string
	for _, step := range job.Steps {
		gotUses = append(gotUses, actionFamily(step.Uses))
	}
	if !slices.Equal(gotUses, wantUses) {
		t.Fatalf("job %s steps use %q, want %q: checkout, Go setup, the module cache and the build cache restores, the go test step and the check step, and no other (B5: no Dolt install, no new action)", id, gotUses, wantUses)
	}
	if with := job.Steps[0].With; len(with) != 0 {
		t.Errorf("job %s checks out with %v, want the default depth (B5)", id, with)
	}
	setupGo := job.Steps[1]
	if setupGo.ID != "setup-go" || !reflect.DeepEqual(setupGo.With, map[string]string{"go-version-file": "go.mod", "cache": "false"}) {
		t.Errorf("job %s sets up Go as id %q with %v, want id setup-go, go-version-file go.mod and cache false (the caches are restored by their own steps)", id, setupGo.ID, setupGo.With)
	}

	testAt, checkAt := forkQualServedSteps(t, job)
	if testAt != 4 || checkAt != testAt+1 {
		t.Errorf("job %s runs go test at step %d and %q at step %d, want steps 4 and 5: the check step comes immediately after the step it checks (B7)", id, testAt, forkQualServedCheck, checkAt)
	}
	test, check := job.Steps[testAt], job.Steps[checkAt]

	if got := job.Steps[2].With["path"]; !strings.Contains(got, "pkg/mod") {
		t.Errorf("job %s restores the module cache into %q, want the Go module cache (a path under pkg/mod)", id, got)
	}
	if got := job.Steps[3].With["path"]; got != forkQualServedCache || test.Env["GOCACHE"] != got {
		t.Errorf("job %s restores the build cache into %q and the go test step builds in %q, want both %q: the non-race cache, since the job does not run with -race", id, got, test.Env["GOCACHE"], forkQualServedCache)
	}

	// B5: the three gates are set in the env of the one go test step, to 1,
	// and are named nowhere else in the file (a job-level or workflow-level
	// env, another step, a run script or a called workflow).
	where := map[string][]string{}
	walkYAML(node, "", func(path string, _ bool, value string) {
		for _, gate := range forkQualServedGates {
			if strings.Contains(value, gate) {
				where[gate] = append(where[gate], path)
			}
		}
	})
	for _, gate := range forkQualServedGates {
		want := fmt.Sprintf(".jobs.%s.steps[%d].env.%s", id, testAt, gate)
		if got := where[gate]; !slices.Equal(got, []string{want}) {
			t.Errorf("%s appears at %q, want only at %q: it must be set, in the env of the go test step alone, for the served tests to run and for a missing run to fail", gate, got, want)
		}
		if got := test.Env[gate]; got != "1" {
			t.Errorf("the go test step sets %s to %q, want \"1\"", gate, got)
		}
	}
	wantEnv := slices.Concat(forkQualServedGates, []string{"GOCACHE", forkQualServedLog})
	slices.Sort(wantEnv)
	if got := sortedKeys(test.Env); !slices.Equal(got, wantEnv) {
		t.Errorf("the go test step's env has %v, want %v", got, wantEnv)
	}
	if got := test.Env[forkQualServedLog]; got != forkQualServedLogVal {
		t.Errorf("the go test step's %s = %q, want %q", forkQualServedLog, got, forkQualServedLogVal)
	}
	if got := sortedKeys(check.Env); !slices.Equal(got, []string{forkQualServedLog}) || check.Env[forkQualServedLog] != forkQualServedLogVal {
		t.Errorf("the check step's env is %v, want only %s = %q: it reads the log the go test step wrote and nothing else (K6)", check.Env, forkQualServedLog, forkQualServedLogVal)
	}

	// B6, B7: both steps are nonoptional and run under bash with a strict
	// header, and the go test line is the ruled one, word for word.
	for what, step := range map[string]ciWorkflowStep{"go test": test, "check": check} {
		if step.If != "" || forkQualContinues(step.ContinueOnError) {
			t.Errorf("the %s step must be nonoptional (if %q, continue-on-error %v): a skipped or failed-and-ignored step is a run that never happened", what, step.If, step.ContinueOnError)
		}
		if step.Shell != "" && step.Shell != "bash" {
			t.Errorf("the %s step has shell %q, want the default or bash: both scripts are bash", what, step.Shell)
		}
		if lines := forkQualLogicalLines(step.Run); len(lines) == 0 || lines[0] != forkQualServedStrict {
			t.Errorf("the %s step's first command is %q, want %q: without pipefail the tee in the go test line, and without -e the check step's own commands, hide a failure", what, lines, forkQualServedStrict)
		}
	}
	lines := forkQualLogicalLines(test.Run)
	if len(lines) != 2 {
		t.Fatalf("the go test step runs %d commands %q, want %q and the one go test line", len(lines), lines, forkQualServedStrict)
	}
	if got := forkQualShellWords(lines[1]); !slices.Equal(got, forkQualServedCommand) {
		t.Errorf("the go test line is\n  %q\nwant\n  %q\n(B6): both packages, -v for the log the check step reads, -count=1, tags gms_pure_go and no -short, -skip, -run, -failfast or -race", got, forkQualServedCommand)
	}
}

// forkQualServedShim stands in for go. The check step makes one call, go list,
// and the shim records it and prints the fixture's import paths; any other call
// is a failure of the step's contract (K6: it reads the log and the go list
// output).
const forkQualServedShim = `#!/bin/bash
{
  printf '\001CALL\001\0'
  for arg in "$@"; do
    printf '%s\0' "$arg"
  done
} >> "$FORK_QUAL_GO_LOG"
if [ "${1:-}" = list ]; then
  printf '%s' "$FORK_QUAL_GO_LIST"
  exit "${FORK_QUAL_GO_LIST_RC:-0}"
fi
echo "go shim: unexpected call: go $*" >&2
exit 97
`

// A case of the check step: the import paths go list prints, the log the test
// step left, and what the step must do with them.
type forkQualServedCase struct {
	name, why  string
	list       []string
	log        string
	noLog      bool
	listRC     int
	passes     bool
	want, deny []string // text the step prints, and text it must not
}

func forkQualServedPass(name string) string {
	return "=== RUN   " + name + "\n--- PASS: " + name + " (0.01s)\n"
}

func forkQualServedSkip(name, reason string) string {
	return "=== RUN   " + name + "\n    served_test.go:12: " + reason + "\n--- SKIP: " + name + " (0.00s)\n"
}

func forkQualServedOK(path, seconds string) string {
	return "PASS\nok  \t" + path + "\t" + seconds + "s\n"
}

// forkQualServedGoodLog is what a run that did everything leaves: both packages
// ok, the five sentinels passed, one top-level skip that is a known divergence,
// and a skipped subtest that is not top level and so not the step's business.
var forkQualServedGoodLog = forkQualServedPass(forkQualServedSentinels[0]) +
	forkQualServedPass(forkQualServedSentinels[1]) +
	"=== RUN   TestSubtests\n=== RUN   TestSubtests/child\n--- PASS: TestSubtests (0.00s)\n" +
	"    --- SKIP: TestSubtests/child (0.00s)\n        served_test.go:12: nothing a divergence explains\n" +
	forkQualServedOK(forkQualServedHTTP, "12.345") +
	forkQualServedPass(forkQualServedSentinels[2]) +
	forkQualServedPass(forkQualServedSentinels[3]) +
	forkQualServedPass(forkQualServedSentinels[4]) +
	forkQualServedSkip("TestDivergent", forkQualServedDivergence) +
	forkQualServedOK(forkQualServedClient, "45.678")

func forkQualServedCases(t *testing.T) []forkQualServedCase {
	t.Helper()
	good := forkQualServedGoodLog
	roots := []string{forkQualServedHTTP, forkQualServedClient}
	short := map[string]string{forkQualServedHTTP: "http", forkQualServedClient: "client"}
	divergent := forkQualServedSkip("TestDivergent", forkQualServedDivergence)
	variant := func(old, replacement string) string { return forkQualServedVariant(t, good, old, replacement) }
	noOK := func(root string) string { return root + ": no ok line" }
	noSentinel := func(name string) string { return "sentinel " + name + " was not reported PASS" }
	tally := func(skips, bad int) string { return fmt.Sprintf("top-level skips=%d unexplained=%d", skips, bad) }

	cases := []forkQualServedCase{
		{name: "good", why: "both packages ok, five sentinels passed, one divergence skip", list: roots, log: good, passes: true,
			want: []string{tally(1, 0)}},
		{name: "no-skips", why: "no skip at all", list: roots, log: variant(divergent, ""), passes: true,
			want: []string{tally(0, 0)}},
		{name: "two-divergences", why: "two skips that are both known divergences", list: roots,
			log: variant(divergent, divergent+forkQualServedSkip("TestAlsoDivergent", forkQualServedDivergence)), passes: true,
			want: []string{tally(2, 0)}},
		{name: "divergence-under-cont", why: "a parallel test's divergence reported under === CONT", list: roots, passes: true,
			log: variant(divergent, "=== RUN   TestDivergent\n=== PAUSE TestDivergent\n=== RUN   TestOther\n--- PASS: TestOther (0.00s)\n"+
				"=== CONT  TestDivergent\n    served_test.go:12: "+forkQualServedDivergence+"\n--- SKIP: TestDivergent (0.00s)\n"),
			want: []string{tally(1, 0)}},
		{name: "divergence-under-name", why: "a parallel test's divergence reported under === NAME", list: roots, passes: true,
			log: variant(divergent, "=== RUN   TestDivergent\n=== PAUSE TestDivergent\n=== RUN   TestOther\n"+
				"=== NAME  TestDivergent\n    served_test.go:12: "+forkQualServedDivergence+"\n=== NAME  TestOther\n"+
				"--- PASS: TestOther (0.00s)\n--- SKIP: TestDivergent (0.00s)\n"),
			want: []string{tally(1, 0)}},
		{name: "unexplained-skip", why: "a skip no divergence explains, after one that is", list: roots,
			log:  variant(divergent, divergent+forkQualServedSkip("TestPlain", "needs a thing the runner lacks")),
			want: []string{"::error::unexplained skip: TestPlain", tally(2, 1)}, deny: []string{"unexplained skip: TestDivergent"}},
		{name: "count-0", why: "go list printing no package", list: nil, log: good,
			want: []string{"go list printed 0 packages, want 2"}},
		{name: "count-1", why: "go list printing one of the two packages, which has its ok line", list: roots[:1], log: good,
			want: []string{"go list printed 1 packages, want 2"}},
		{name: "count-3", why: "go list printing a third package, which has its ok line", list: append(slices.Clone(roots), forkQualServedExtra),
			log: good + forkQualServedOK(forkQualServedExtra, "1.000"), want: []string{"go list printed 3 packages, want 2"}},
		{name: "list-fails", why: "go list failing", list: nil, listRC: 1, log: good},
		{name: "no-log", why: "no log at all", list: roots, noLog: true,
			want: append([]string{noOK(forkQualServedHTTP), noOK(forkQualServedClient)}, mapStrings(forkQualServedSentinels, noSentinel)...)},
		{name: "empty-log", why: "an empty log", list: roots, log: "",
			want: append([]string{noOK(forkQualServedHTTP), noOK(forkQualServedClient)}, mapStrings(forkQualServedSentinels, noSentinel)...)},
		{name: "multi-violation", why: "a missing ok line, a third package without one, a missing sentinel and an unexplained skip",
			list: append(slices.Clone(roots), forkQualServedExtra),
			log: forkQualServedVariant(t, variant(forkQualServedOK(forkQualServedHTTP, "12.345"), "FAIL\n"+"FAIL\t"+forkQualServedHTTP+"\t0.500s\n"),
				forkQualServedPass(forkQualServedSentinels[3]), "") + forkQualServedSkip("TestPlain", "needs a thing the runner lacks"),
			want: []string{noOK(forkQualServedHTTP), noOK(forkQualServedExtra), "go list printed 3 packages, want 2",
				noSentinel(forkQualServedSentinels[3]), "::error::unexplained skip: TestPlain"}},
	}

	for _, root := range roots {
		ok := forkQualServedOK(root, map[string]string{forkQualServedHTTP: "12.345", forkQualServedClient: "45.678"}[root])
		parent := path.Dir(root)
		for kind, line := range map[string]string{
			"missing": "",
			"failed":  "FAIL\nFAIL\t" + root + "\t0.500s\n",
		} {
			cases = append(cases, forkQualServedCase{name: "no-ok-" + kind + "-" + short[root], why: "no ok line for " + root + " (" + kind + ")",
				list: roots, log: variant(ok, line), want: []string{noOK(root)}})
		}
		cases = append(cases,
			forkQualServedCase{name: "ok-longer-" + short[root], why: "an ok line for a package that extends " + root, list: roots,
				log: variant(ok, forkQualServedOK(root+"/extra", "1.000")), want: []string{noOK(root)}},
			forkQualServedCase{name: "ok-shorter-" + short[root], why: "an ok line for the parent of " + root, list: roots,
				log: variant(ok, forkQualServedOK(parent, "1.000")), want: []string{noOK(root)}})
	}

	for _, name := range forkQualServedSentinels {
		block := forkQualServedPass(name)
		for kind, replacement := range map[string]string{
			"skip":     forkQualServedSkip(name, forkQualServedDivergence),
			"absent":   "",
			"renamed":  forkQualServedPass(name + "Once"),
			"indented": "=== RUN   " + name + "\n    --- PASS: " + name + " (0.01s)\n",
		} {
			cases = append(cases, forkQualServedCase{name: "sentinel-" + kind + "-" + name, why: "the sentinel " + name + " " + kind,
				list: roots, log: variant(block, replacement), want: []string{noSentinel(name)}, deny: []string{"unexplained skip: " + name}})
		}
	}
	name := forkQualServedSentinels[0]
	cases = append(cases, forkQualServedCase{name: "sentinel-unexplained-skip-" + name, why: "the sentinel " + name + " skipped for no divergence",
		list: roots, log: variant(forkQualServedPass(name), forkQualServedSkip(name, "needs a thing the runner lacks")),
		want: []string{noSentinel(name), "unexplained skip: " + name}})
	return cases
}

// mapStrings returns f applied to each of items.
func mapStrings(items []string, f func(string) string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, f(item))
	}
	return out
}

// forkQualServedVariant swaps the one occurrence of old in text for
// replacement: the controls and the cases are built from the committed script
// and from one good log by exact anchor, so an anchor that has moved fails loudly.
func forkQualServedVariant(t *testing.T, text, old, replacement string) string {
	t.Helper()
	if n := strings.Count(text, old); n != 1 {
		t.Fatalf("construction: %q occurs %d times in the text it varies, want 1: the served cases and controls work on the committed check step by exact anchor (ruling be-s847fi Appendix B), so change the anchor with the step", old, n)
	}
	return strings.Replace(text, old, replacement, 1)
}

// forkQualServedCheckStep returns the committed check step.
func forkQualServedCheckStep(t *testing.T) ciWorkflowStep {
	t.Helper()
	job := forkQualLoad(t).job(t, forkQualServedJob)
	_, at := forkQualServedSteps(t, job)
	return job.Steps[at]
}

// forkQualRunServed runs the check step's own script in a scratch directory
// whose only go is the shim and whose only input is the log, and returns what
// the step got wrong.
func forkQualRunServed(t *testing.T, bash, scratch string, tc forkQualServedCase, step ciWorkflowStep) []string {
	t.Helper()
	dir := filepath.Join(scratch, tc.name)
	runnerTemp := filepath.Join(dir, "runner-temp")
	shimDir := filepath.Join(dir, "shim")
	work := filepath.Join(dir, "work")
	home := filepath.Join(dir, "home")
	for _, d := range []string{runnerTemp, shimDir, work, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(name, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(shimDir, "go"), forkQualServedShim, 0o755)
	if !tc.noLog {
		write(filepath.Join(runnerTemp, "served-http.log"), tc.log, 0o644)
	}
	logPath := filepath.Join(runnerTemp, "go-calls")
	list := strings.Join(tc.list, "\n")
	if list != "" {
		list += "\n"
	}

	strict := func(where, text string) string {
		t.Helper()
		text = forkQualWorkspaceExprRe.ReplaceAllLiteralString(text, work)
		text = forkQualRunnerTempExprRe.ReplaceAllLiteralString(text, runnerTemp)
		if expr := forkQualExprRe.FindString(text); expr != "" {
			t.Fatalf("the check step's %s uses %s: the pin evaluates only ${{ github.workspace }} and ${{ runner.temp }}", where, expr)
		}
		return text
	}
	vars := map[string]string{
		"HOME": home, "LC_ALL": "C", "RUNNER_TEMP": runnerTemp, "GITHUB_WORKSPACE": work,
		"PATH":             shimDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FORK_QUAL_GO_LOG": logPath, "FORK_QUAL_GO_LIST": list, "FORK_QUAL_GO_LIST_RC": fmt.Sprint(tc.listRC),
	}
	for key, value := range step.Env {
		vars[key] = strict("env "+key, value)
	}
	var env []string
	for _, key := range sortedKeys(vars) {
		env = append(env, key+"="+vars[key])
	}
	script := strict("run script", step.Run)

	flags := []string{"--noprofile", "--norc", "-e"}
	switch step.Shell {
	case "":
	case "bash":
		flags = []string{"--noprofile", "--norc", "-eo", "pipefail"}
	default:
		t.Fatalf("the check step has shell: %s; it runs under bash, the one shell the pin evaluates", step.Shell)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, append(flags, "-c", script)...)
	cmd.Dir = work
	cmd.Env = env
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	code := exitCode(err)

	var found []string
	bad := func(format string, args ...any) { found = append(found, fmt.Sprintf(format, args...)) }
	if ctx.Err() != nil {
		bad("the step did not finish within 60s:\n%s", forkQualTail(out))
		return found
	}
	switch {
	case tc.passes && code != 0:
		bad("the step exits %d on %s, want 0:\n%s", code, tc.why, forkQualTail(out))
	case !tc.passes && code == 0:
		bad("the step exits 0 on %s, want a failure: a run that is missing or hollow must fail the job\n%s", tc.why, forkQualTail(out))
	}
	for _, want := range tc.want {
		if !strings.Contains(string(out), want) {
			bad("on %s the step does not print %q:\n%s", tc.why, want, forkQualTail(out))
		}
	}
	for _, deny := range tc.deny {
		if strings.Contains(string(out), deny) {
			bad("on %s the step prints %q, which is not true of this log:\n%s", tc.why, deny, forkQualTail(out))
		}
	}
	if calls := forkQualReadCalls(t, logPath); len(calls) != 1 || !slices.Equal(calls[0], forkQualServedList) {
		bad("on %s the step called go %q, want exactly one call, go %s (K1: the two packages, with the tags the test step builds with)", tc.why, calls, strings.Join(forkQualServedList, " "))
	}
	return found
}

// forkQualServedViolations runs the step over every case, or over the cases
// whose name contains one of only. A name in only that selects no case is a
// mistake in the caller, not a pass.
func forkQualServedViolations(t *testing.T, step ciWorkflowStep, only ...string) []forkQualViolation {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the check step runs under bash with a go shim script")
	}
	bash := requireHostTool(t, "bash")
	requireHostTool(t, "awk")
	requireHostTool(t, "grep")
	scratch := t.TempDir()
	cases := forkQualServedCases(t)
	picked := func(name string) bool {
		return len(only) == 0 || slices.ContainsFunc(only, func(part string) bool { return strings.Contains(name, part) })
	}
	for _, part := range only {
		if !slices.ContainsFunc(cases, func(tc forkQualServedCase) bool { return strings.Contains(tc.name, part) }) {
			t.Fatalf("construction: no case of the check step contains %q", part)
		}
	}
	var violations []forkQualViolation
	for _, tc := range cases {
		if !picked(tc.name) {
			continue
		}
		for _, text := range forkQualRunServed(t, bash, scratch, tc, step) {
			violations = append(violations, forkQualViolation{scenario: tc.name, text: text})
		}
	}
	return violations
}

// TestForkQualificationServedCheckStep runs the check step's own script, with a
// go shim, over fixture logs: it passes the log of a run that did everything and
// fails, naming each fault, every way a run can be missing or hollow (K1 to K4).
func TestForkQualificationServedCheckStep(t *testing.T) {
	for _, v := range forkQualServedViolations(t, forkQualServedCheckStep(t)) {
		t.Error(v)
	}
}

// TestForkQualificationServedControls proves that harness can fail: the check
// step's own script is varied by exact anchor, one fault at a time, and every
// variant must be rejected by the cases that exist to catch it. The first four
// are the controls the ruling names (the roots loop, the sentinel loop, the skip
// check and the final exit); the rest are the ways each of those can be hollowed
// out without being deleted.
func TestForkQualificationServedControls(t *testing.T) {
	step := forkQualServedCheckStep(t)
	script := step.Run
	vary := func(pairs ...string) string {
		t.Helper()
		out := script
		for i := 0; i < len(pairs); i += 2 {
			out = forkQualServedVariant(t, out, pairs[i], pairs[i+1])
		}
		return out
	}

	const (
		okProgram  = `'$1 == "ok" && $2 == r { f = 1 } END { exit !f }'`
		sentinelRe = `"^--- PASS: ${name} \("`
		cleanSkip  = `if (!(t in kd)) { bad++;`
	)
	type control struct {
		name, script string
		flagged      []string
	}
	controls := []control{
		{"the roots loop never runs", vary(`for root in $roots; do`, `for root in; do`), []string{"no-ok-"}},
		{"an ok check that cannot fail", vary(okProgram, `'END { exit 0 }'`), []string{"no-ok-", "ok-longer-", "ok-shorter-"}},
		{"an ok match on a longer path", vary(`$2 == r`, `index($2, r) == 1`), []string{"ok-longer-"}},
		{"an ok match on a shorter path", vary(`$2 == r`, `index(r, $2) == 1`), []string{"ok-shorter-"}},
		{"no check of the package count", vary(`if [ "$n" -ne 2 ]; then`, `if false; then`), []string{"count-"}},
		{"a package count of at least two", vary(`"$n" -ne 2`, `"$n" -lt 2`), []string{"count-3"}},
		{"the sentinel loop never checks", vary(`if ! grep -q -E `+sentinelRe+` "$SERVED_LOG"; then`, `if false; then`), []string{"sentinel-"}},
		{"a sentinel pattern that allows indentation", vary(sentinelRe, `"^ *--- PASS: ${name} \("`), []string{"sentinel-indented-"}},
		{"a sentinel pattern without its closing parenthesis", vary(sentinelRe, `"^--- PASS: ${name}"`), []string{"sentinel-renamed-"}},
		{"a sentinel that may be skipped", vary(sentinelRe, `"^--- (PASS|SKIP): ${name} \("`), []string{"sentinel-skip-"}},
		{"a sentinel error that does not fail", vary("was not reported PASS\"\n    bad=1\n", "was not reported PASS\"\n"), []string{"sentinel-absent-"}},
		{"the skip check never flags", vary(cleanSkip, `if (0) { bad++;`), []string{"unexplained-skip"}},
		{"one divergence excusing every skip", vary(`kd[cur] = 1`, `kd["*"] = 1`, `if (!(t in kd))`, `if (!("*" in kd))`), []string{"unexplained-skip"}},
		{"a skip count that includes subtests", vary(`/^--- SKIP: /`, `/--- SKIP: /`), []string{"good"}},
		{"a divergence only tracked under === RUN", vary(`/^=== (RUN|CONT|NAME) +/`, `/^=== RUN +/`), []string{"divergence-under-"}},
		{"an awk status the shell ignores", vary(`exit (bad > 0) }`, `exit 0 }`), []string{"unexplained-skip"}},
		{"no final exit", vary(`exit "$bad"`, `true`), []string{"no-ok-"}},
		{"an exit at the first fault", vary("test binary did not succeed\"\n    bad=1\n", "test binary did not succeed\"\n    exit 1\n"), []string{"multi-violation"}},
		{"a tally that is not printed as such", vary(`top-level skips=`, `skips=`), []string{"good"}},
		{"go list without the build tags", vary(`go list -tags gms_pure_go \`, `go list \`), []string{"good"}},
		{"go list of one package", vary(`./backend/http ./internal/httpclient)"`, `./backend/http)"`), []string{"good"}},
	}
	// Each sentinel in turn left out of the loop, by checking its neighbour twice.
	for i, name := range forkQualServedSentinels {
		next := forkQualServedSentinels[(i+1)%len(forkQualServedSentinels)]
		controls = append(controls, control{"the sentinel " + name + " left out of the loop", vary(name, next), []string{name}})
	}

	if got := forkQualServedViolations(t, step); len(got) != 0 {
		t.Fatalf("the committed check step has violations, so a control could not be told from it: %v", got)
	}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			got := forkQualServedViolations(t, ciWorkflowStep{Run: c.script, Env: step.Env, Shell: step.Shell}, c.flagged...)
			if len(got) == 0 {
				t.Errorf("%s is not flagged by the cases %q: the check step could do the same and pass", c.name, c.flagged)
				return
			}
			t.Logf("flagged: %s", got[0])
		})
	}
}
