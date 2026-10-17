package scripts_test

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins the two jobs of .github/workflows/fork-embedded-gated.yml,
// the workflow behind the embedded-gated doctor tests (be-zbhceo, rule M1).
//
// A test gated on BEADS_TEST_EMBEDDED_DOLT=1 skips wherever the variable is
// unset, and go test exits 0 on a skip, so a gated test that no lane runs with
// the variable set ends as a skip on every pull request and a green check says
// nothing about it. cmd/bd/doctor holds two of them that no shard selects. The
// doctor-embedded job of the workflow runs exactly those with the gate set and
// fails unless each one is reported PASS by name; its lane-guards job runs the
// tests of this package that pin these lanes and fails the same way.
//
// The scan that fails when a package gains a gated test that no lane runs is
// in fork_embedded_gate_scan_pin_test.go. The two files are in different Bazel
// targets on purpose: this one is in scripts_test, which carries the workflows
// (and, for the lane-guards check below, the test files of scripts/ as data),
// and the scan is in go_sources_test, which carries every Go source of the
// module. Both share fork_embedded_gate_pin_helpers_test.go, and neither file
// may use an identifier the other defines.

// shellWords folds line continuations and all whitespace, so a pin compares
// what the shell reads and not the YAML indentation.
func shellWords(script string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(script, "\\\n", " ")), " ")
}

// The doctor-embedded job runs the gated doctor tests with the gate set, and
// its verify step reads each name's result line from the log, because go test
// exits 0 for a gated test that skipped and for a -run pattern that selects
// nothing (both test files are //go:build cgo). Neither step may be skipped
// or allowed to fail quietly, and the verify step is never replaced by go
// test's exit status.
func TestForkEmbeddedGatedLane(t *testing.T) {
	const (
		jobName    = "doctor-embedded"
		testStep   = "Test (gated doctor tests)"
		verifyStep = "Verify every gated test passed by name"
	)
	job := readCIWorkflow(t, forkEmbeddedGatedWorkflow).job(t, jobName)
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("%s runs on %q, want ubuntu-latest (this repository has no Blacksmith runners)", jobName, job.RunsOn)
	}
	if job.TimeoutMinutes != 30 || job.If != "" || job.ContinueOnError {
		t.Errorf("%s must stay bounded (30 minutes), unconditional and failure-propagating", jobName)
	}
	test := job.step(t, testStep)
	if got := test.Env[embeddedDoltGate]; got != "1" {
		t.Errorf("step %q sets %s = %q, want \"1\": without the gate both tests skip, and go test still exits 0", testStep, embeddedDoltGate, got)
	}
	if test.If != "" || test.ContinueOnError != nil {
		t.Errorf("step %q must be unconditional and failure-propagating", testStep)
	}
	wantTest := `set -o pipefail
go test -tags gms_pure_go -race -count=1 -v -timeout 20m \
  -run '^(` + strings.Join(forkDoctorGatedTests, "|") + `)$' \
  ./cmd/bd/doctor/ 2>&1 | tee "$RUNNER_TEMP/doctor-gated.log"`
	if got := shellWords(test.Run); got != shellWords(wantTest) {
		t.Errorf("step %q run = %q, want %q (every gated test by name, with -race and -v, the output kept for the verify step)", testStep, got, shellWords(wantTest))
	}
	verify := job.step(t, verifyStep)
	if got := strings.Fields(verify.Env["GATED_TESTS"]); !equalStrings(got, forkDoctorGatedTests) {
		t.Errorf("step %q GATED_TESTS = %v, want %v", verifyStep, got, forkDoctorGatedTests)
	}
	if verify.If != "" || verify.ContinueOnError != nil {
		t.Errorf("step %q must be unconditional and failure-propagating", verifyStep)
	}
	wantVerify := `status=0
for name in $GATED_TESTS; do
  if ! grep -E "^--- PASS: ${name} \(" "$RUNNER_TEMP/doctor-gated.log"; then
    echo "::error::${name} was not reported PASS (skipped, not run or failed)"
    status=1
  fi
done
exit "$status"`
	if got := shellWords(verify.Run); got != shellWords(wantVerify) {
		t.Errorf("step %q run = %q, want %q (a skip or a missing test exits 0 under go test, so each name must be read from the log)", verifyStep, got, shellWords(wantVerify))
	}
	assertStepsBefore(t, job, []string{testStep}, []string{verifyStep})
}

// The lane runs pull-request code, so it gets no privileged trigger, no token
// beyond read access to the repository and no secret, and it stays out of
// pr.yml's CI Gate: a lane that can neither delay nor fail that gate has its
// result read directly. A pull request whose base is not integration (a
// stacked one) does not trigger it.
func TestForkEmbeddedGatedLaneIsUnprivilegedAndOutsideTheGate(t *testing.T) {
	path := filepath.Join(sourceRepoRoot(t), ".github", "workflows", forkEmbeddedGatedWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	wantOn := map[string]any{
		"pull_request":      map[string]any{"branches": []any{"integration"}},
		"workflow_dispatch": nil,
	}
	if !reflect.DeepEqual(doc.On, wantOn) {
		t.Errorf("%s on = %v, want exactly %v (never pull_request_target, workflow_run or a push trigger: the lane runs pull-request code)", forkEmbeddedGatedWorkflow, doc.On, wantOn)
	}
	if !reflect.DeepEqual(doc.Permissions, map[string]string{"contents": "read"}) {
		t.Errorf("%s permissions = %v, want exactly contents: read", forkEmbeddedGatedWorkflow, doc.Permissions)
	}
	if bytes.Contains(data, []byte("secrets.")) {
		t.Errorf("%s reads a secret; the lane runs pull-request code and gets none", forkEmbeddedGatedWorkflow)
	}
	jobs := readCIWorkflow(t, forkEmbeddedGatedWorkflow).Jobs
	for _, name := range sortedKeys(jobs) {
		if jobs[name].Permissions != nil {
			t.Errorf("%s job %s sets its own permissions (%v); the workflow's contents: read is all it gets", forkEmbeddedGatedWorkflow, name, jobs[name].Permissions)
		}
	}
	gate := readCIWorkflow(t, "pr.yml").job(t, "ci-gate").Needs
	for _, name := range sortedKeys(readCIWorkflow(t, forkEmbeddedGatedWorkflow).Jobs) {
		if contains(gate, name) {
			t.Errorf("pr.yml's ci-gate needs %s; the jobs of the fork's embedded-gated workflow are separate checks", name)
		}
	}
}

// The lane guards (be-zbhceo). Bazel's test log prints a line only for a test
// that fails, so it does not show that a guard ran: one that was skipped,
// renamed or left out of the build looks like one that passed. The
// lane-guards job of fork-embedded-gated.yml runs every test of scripts/ whose
// name starts with TestForkEmbeddedGate with -v and fails unless each one is
// reported PASS by name. The convention is the prefix: a pin of these lanes
// that is to be seen by name starts with it.

const (
	forkGuardJob    = "lane-guards"
	forkGuardPrefix = "TestForkEmbeddedGate"
)

// forkGuardTags: the build tags the lane-guards job compiles scripts/ with.
var forkGuardTags = []string{"gms_pure_go"}

// laneBuildContext: the build context of a lane that compiles with tags, on
// linux/amd64 with cgo.
func laneBuildContext(tags []string) build.Context {
	ctx := build.Default
	ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled, ctx.BuildTags = "linux", "amd64", true, tags
	return ctx
}

// forkGuardTests: the top-level tests of dir whose names start with prefix, in
// the _test.go files that a build with tags compiles, sorted.
func forkGuardTests(t *testing.T, dir, prefix string, tags []string) []string {
	t.Helper()
	ctx := laneBuildContext(tags)
	paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	// 40 is the floor of TestScriptsTestTargetsPartitionTheTests: a directory
	// that holds fewer files is missing or partial, and an empty one must not pass.
	if err != nil || len(paths) < 40 {
		t.Fatalf("found %d _test.go files in %s (err %v), want at least 40; the listing is broken", len(paths), dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, p := range paths {
		ok, err := ctx.MatchFile(filepath.Dir(p), filepath.Base(p))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, prefix) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// The lane-guards job keeps its shape, and the list it verifies is the list
// the source says.
//   - It is unconditional and bounded, and runs one go test over scripts/ with
//     -v whose -run pattern is the prefix, so a new guard needs no edit of
//     the step, only a line in GUARD_TESTS.
//   - Its verify step reads each name's result line from the log, because go
//     test exits 0 for a guard that skipped and for a pattern that selects
//     nothing.
//   - K1: GUARD_TESTS is sorted, names no test twice and is name for name the
//     tests of scripts/ that start with the prefix and that the job compiles.
//     The source half runs under Bazel too: scripts_test declares the test
//     files of scripts/ as data (:repo_go_test_srcs in scripts/BUILD.bazel)
//     for it.
func TestForkEmbeddedGateGuardsAreRunByName(t *testing.T) {
	const (
		testStep   = "Test (lane guards)"
		verifyStep = "Verify every guard passed by name"
	)
	job := readCIWorkflow(t, forkEmbeddedGatedWorkflow).job(t, forkGuardJob)
	if job.RunsOn != "ubuntu-latest" {
		t.Errorf("%s runs on %q, want ubuntu-latest (this repository has no Blacksmith runners)", forkGuardJob, job.RunsOn)
	}
	if job.TimeoutMinutes != 20 || job.If != "" || job.ContinueOnError {
		t.Errorf("%s must stay bounded (20 minutes), unconditional and failure-propagating", forkGuardJob)
	}
	test := job.step(t, testStep)
	if test.If != "" || test.ContinueOnError != nil {
		t.Errorf("step %q must be unconditional and failure-propagating", testStep)
	}
	wantTest := `set -o pipefail
go test -tags ` + strings.Join(forkGuardTags, ",") + ` -count=1 -v -timeout 15m \
  -run '^` + forkGuardPrefix + `' \
  ./scripts/ 2>&1 | tee "$RUNNER_TEMP/lane-guards.log"`
	if got := shellWords(test.Run); got != shellWords(wantTest) {
		t.Errorf("step %q run = %q, want %q (every guard by name, with -v, the output kept for the verify step)", testStep, got, shellWords(wantTest))
	}
	verify := job.step(t, verifyStep)
	if verify.If != "" || verify.ContinueOnError != nil {
		t.Errorf("step %q must be unconditional and failure-propagating", verifyStep)
	}
	wantVerify := `status=0
for name in $GUARD_TESTS; do
  if ! grep -E "^--- PASS: ${name} \(" "$RUNNER_TEMP/lane-guards.log"; then
    echo "::error::${name} was not reported PASS (skipped, not run or failed)"
    status=1
  fi
done
exit "$status"`
	if got := shellWords(verify.Run); got != shellWords(wantVerify) {
		t.Errorf("step %q run = %q, want %q (a skip or a missing test exits 0 under go test, so each name must be read from the log)", verifyStep, got, shellWords(wantVerify))
	}
	assertStepsBefore(t, job, []string{testStep}, []string{verifyStep})

	have := strings.Fields(verify.Env["GUARD_TESTS"])
	if !sort.StringsAreSorted(have) {
		t.Errorf("GUARD_TESTS of %s is not sorted (byte order), which makes the diff the pin prints unreadable", forkGuardJob)
	}
	seen := map[string]bool{}
	for _, name := range have {
		if seen[name] {
			t.Errorf("GUARD_TESTS of %s names %s twice", forkGuardJob, name)
		}
		seen[name] = true
	}
	want := forkGuardTests(t, filepath.Join(sourceRepoRoot(t), "scripts"), forkGuardPrefix, forkGuardTags)
	if add, drop := listDiff(have, want); len(add)+len(drop) > 0 {
		t.Errorf("GUARD_TESTS of %s differs from the tests of scripts/ named %s*.\n  add to GUARD_TESTS (keep it sorted): %v\n  drop from GUARD_TESTS: %v\nA guard you add runs in the job and must pass by name; a guard you rename or delete leaves GUARD_TESTS with it (back to the architect, be-zbhceo)", forkGuardJob, forkGuardPrefix, add, drop)
	}
}
