package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins .github/workflows/fork-heavy-tiers.yml, the temporary
// workflow that keeps the heavy test tiers running on every pull request into
// integration while the sync replaces pr-risk.yml's copy of them (work package
// W1, be-fxkoxb; ruling be-4y7b5s sections 3.4, 5.1 rule 4, 8.3 and 11, as
// amended by be-bg8llq).
//
// The workflow is lifted, not written: its eight jobs come out of the git blobs
// of pr-risk.yml and proxied-local-smoke.yml at forkHeavyBase with five edits
// and nothing else (a "Fork Heavy " name prefix, needs cut down to
// build-embedded, the tier if: dropped, a Blacksmith runs-on made
// ubuntu-latest, and, for managed-local-smoke alone, its Blacksmith-only cache
// unit cut: the comment above it and the steps "Compute cache date" and
// "Restore Blacksmith setup-go cache", which leaves six of its eight steps),
// plus one aggregator, "Fork Heavy Tiers / Required", that fails unless every
// other job's result is exactly success. The acceptance criteria of the bead
// are pinned like this:
//
//   - 1, the job set and the aggregator: TestForkHeavyTiersJobSetAndAggregator
//     and TestForkHeavyTiersAggregatorFailsUnlessEveryNeedSucceeded.
//   - 2, the trigger shape, the text the ruling bars, the helper paths and the
//     lift diff: TestForkHeavyTiersRunsUnprivilegedOnIntegrationPullRequests,
//     TestForkHeavyTiersNamesNothingTheRulingBars,
//     TestForkHeavyTiersNamesOnlyHelpersThatExist,
//     TestForkHeavyTiersJobsKeepTheirShape and
//     TestForkHeavyTiersLiftMatchesSourceBlobs.
//   - 3, R8 and R9 over both workflow files: fork_proxied_lane_pin_test.go.
//
// Criteria 4 and 5 are runs, not pins, and nothing in this file stands in for
// them: the three-row table of ruling be-bg8llq section 3.5 (upstream's scripts
// package, whose workflow sweeps are bazel_policy_test.go, ci_f7c_advisory_test.go,
// pull_dolt_image_test.go and check_doc_freshness_test.go, run in a throwaway
// worktree at upstream main without the lifted file, with it, and with it plus
// the one-entry patch of section 3.1), the applicable Bazel gates, and the fork
// pull request that runs the lifted workflow with its aggregator green at the
// exact reviewed head.
//
// Every pin here asserts a pre-sync shape, so each is gone or re-pointed in the
// sync pull request (ruling section 11). When rbe-fork opens to vb and the
// workflow is retired, delete the workflow, this file, its line in
// scripts/BUILD.bazel, its entry in tools/bazel/equivalence_allowlist.txt and
// the "fork-heavy-tiers.yml": 3 entry, with its comment, that W4 adds to
// scripts/pull_dolt_image_test.go after the sync (be-bg8llq sections 3.1 and
// 4.4), together. A retirement that leaves that entry behind fails nothing,
// which is why it is on this list.

const (
	forkHeavyTiersWorkflow   = "fork-heavy-tiers.yml"
	forkHeavyTiersAggregator = "Fork Heavy Tiers / Required"
	forkHeavyNamePrefix      = "Fork Heavy "
	forkHeavyCheckout        = "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1"

	// vb/integration when W1 was cut. The lift's source blobs are read here, and
	// the table below is a table of facts about them.
	forkHeavyBase = "356275a13290064fe903ace31984c14d1b9f7ad4"

	// Section 3.4: 58 matrix legs and three single jobs.
	forkHeavyLegs = 61
)

// forkHeavyJob is one lifted job as it stands at forkHeavyBase.
type forkHeavyJob struct {
	id         string
	source     string         // the workflow at forkHeavyBase that holds the job
	name       string         // its name: there, before the prefix
	needsBuild bool           // the lift leaves needs: [build-embedded]; otherwise none
	timeout    int            // timeout-minutes, 0 for unset
	matrix     map[string]any // strategy.matrix, nil for a job that has none
	steps      []string       // the lifted job's step labels in order: the name, else the uses
}

var forkHeavyJobs = []forkHeavyJob{
	{
		id: "build-embedded", source: "pr-risk.yml", name: "Build (Embedded Dolt)",
		steps: []string{
			forkHeavyCheckout, "Set up Go", "Restore Go module cache",
			"Restore race Go build cache", "Restore non-race Go build cache",
			"Build embedded bd binary", "Build embedded storage test binary",
			"Build embedded cmd test binary", "Build proxied bd subprocess binary",
			"Build server Dolt conformance test binary", "Upload binaries",
		},
	},
	{
		id: "test-embedded-storage", source: "pr-risk.yml",
		name:       "Test (Embedded Dolt Storage ${{ matrix.shard }}/${{ strategy.job-total }})",
		needsBuild: true, timeout: 25,
		matrix: map[string]any{"shard": forkHeavyShards(5)},
		steps:  []string{forkHeavyCheckout, "Download binaries", "Fix permissions", "Configure Git", "Test"},
	},
	{
		id: "test-embedded-conformance", source: "pr-risk.yml",
		name:       "Test (Embedded Dolt Conformance - ${{ matrix.partition }})",
		needsBuild: true, timeout: 35,
		matrix: map[string]any{"partition": []any{"core", "audit"}},
		steps: []string{
			forkHeavyCheckout, "Download binaries", "Fix permissions", "Configure Git",
			"Test core conformance", "Test audit conformance",
		},
	},
	{
		id: "test-embedded-cmd", source: "pr-risk.yml",
		name:       "Test (Embedded Dolt Cmd ${{ matrix.shard }}/${{ strategy.job-total }})",
		needsBuild: true,
		matrix:     map[string]any{"shard": forkHeavyShards(20)},
		steps:      []string{forkHeavyCheckout, "Download binaries", "Fix permissions", "Configure Git", "Test"},
	},
	{
		id: "test-proxied-cmd", source: "pr-risk.yml",
		name:       "Test (Proxied Dolt Cmd ${{ matrix.shard }}/${{ strategy.job-total }})",
		needsBuild: true, timeout: 30,
		matrix: map[string]any{"shard": forkHeavyShards(15)},
		steps: []string{
			forkHeavyCheckout, "Download binaries", "Fix permissions", "Install Dolt CLI",
			"Configure Git and Dolt identity", "Pull Dolt sql-server image", "Test proxied-server cmd shard",
		},
	},
	{
		id: "test-server-storage", source: "pr-risk.yml", name: "Test (Server Dolt Conformance)",
		needsBuild: true, timeout: 20,
		steps: []string{
			forkHeavyCheckout, "Download binaries", "Fix permissions", "Configure Git",
			"Install Dolt", "Configure Dolt", "Pull Dolt sql-server image", "Test",
		},
	},
	{
		id: "test-server-storage-full", source: "pr-risk.yml",
		name:       "Test (Server Dolt Full Suite ${{ matrix.shard }}/${{ strategy.job-total }})",
		needsBuild: true, timeout: 20,
		matrix: map[string]any{"shard": forkHeavyShards(16)},
		steps: []string{
			forkHeavyCheckout, "Download binaries", "Fix permissions", "Configure Git",
			"Install Dolt", "Configure Dolt", "Pull Dolt sql-server image", "Test",
		},
	},
	{
		id: "managed-local-smoke", source: "proxied-local-smoke.yml",
		name: "Managed-local proxied lifecycle (Linux, offline)", timeout: 25,
		// Six of the source's eight steps: edit 5 cuts the cache unit.
		steps: []string{
			forkHeavyCheckout, "Set up Go", "Install pinned Dolt CLI",
			"Build bd and compile the test binary (online)",
			"Assert the managed-local lifecycle tests are compiled in",
			"Run managed-local lifecycle lane offline (loopback-only netns)",
		},
	},
}

// Edit 5, ruled in be-bg8llq section 2: managed-local-smoke loses its
// Blacksmith-only cache unit, these two steps and the comment above them. The
// YAML oracle sees the steps; TestForkHeavyTiersNamesNothingTheRulingBars sees
// the comment. No other job has a step by either name.
var forkHeavyDroppedSteps = []string{"Compute cache date", "Restore Blacksmith setup-go cache"}

// The keys a lifted job may have, all of which the source jobs have too.
var forkHeavyJobKeys = []string{"name", "needs", "runs-on", "timeout-minutes", "strategy", "env", "steps"}

func forkHeavyShards(n int) []any {
	shards := make([]any, n)
	for i := range shards {
		shards[i] = i + 1
	}
	return shards
}

func forkHeavyJobIDs() []string {
	ids := make([]string, 0, len(forkHeavyJobs))
	for _, job := range forkHeavyJobs {
		ids = append(ids, job.id)
	}
	sort.Strings(ids)
	return ids
}

// forkHeavyRead returns the workflow's text. The first thing every pin does, so
// that until W1 lands each one fails for the same stated reason.
func forkHeavyRead(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(sourceRepoRoot(t), ".github", "workflows", forkHeavyTiersWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is missing: W1 (be-fxkoxb, ruling be-4y7b5s section 3.4) adds it: %v", forkHeavyTiersWorkflow, err)
	}
	return data
}

func forkHeavyWorkflow(t *testing.T) ciWorkflow {
	t.Helper()
	forkHeavyRead(t)
	return readCIWorkflow(t, forkHeavyTiersWorkflow)
}

// forkHeavyRawJobs is the workflow's jobs as plain YAML data, which is the form
// the lift is compared in.
func forkHeavyRawJobs(t *testing.T) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Jobs map[string]map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(forkHeavyRead(t), &doc); err != nil {
		t.Fatalf("parse %s: %v", forkHeavyTiersWorkflow, err)
	}
	return doc.Jobs
}

// forkHeavyAggregator finds the all-success job by its name: the ruling fixes
// the name and the semantics, not the job id.
func forkHeavyAggregator(t *testing.T, wf ciWorkflow) (string, ciWorkflowJob) {
	t.Helper()
	var ids []string
	for id, job := range wf.Jobs {
		if job.Name == forkHeavyTiersAggregator {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		t.Fatalf("%d jobs in %s are named %q (%v), want exactly 1: the aggregator", len(ids), forkHeavyTiersWorkflow, forkHeavyTiersAggregator, ids)
	}
	return ids[0], wf.Jobs[ids[0]]
}

// forkHeavyIf normalizes a job or step condition: the ${{ }} wrapper is
// optional in GitHub's syntax.
func forkHeavyIf(condition string) string {
	condition = strings.TrimSpace(condition)
	if strings.HasPrefix(condition, "${{") && strings.HasSuffix(condition, "}}") {
		condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	}
	return condition
}

// forkHeavyNeeds reads a needs: value, a job id or a list of them, as a sorted
// set.
func forkHeavyNeeds(value any) []string {
	var needs []string
	switch v := value.(type) {
	case nil:
	case string:
		needs = []string{v}
	case []any:
		for _, item := range v {
			needs = append(needs, fmt.Sprint(item))
		}
	default:
		needs = []string{fmt.Sprint(v)}
	}
	sort.Strings(needs)
	return needs
}

func forkHeavyStepLabel(step any) string {
	fields, _ := step.(map[string]any)
	if name, ok := fields["name"].(string); ok {
		return name
	}
	uses, _ := fields["uses"].(string)
	return uses
}

func forkHeavyStepLabels(job map[string]any) []string {
	steps, _ := job["steps"].([]any)
	labels := make([]string, 0, len(steps))
	for _, step := range steps {
		labels = append(labels, forkHeavyStepLabel(step))
	}
	return labels
}

// forkHeavyKeptSteps is a source job's steps less the ones edit 5 drops.
func forkHeavyKeptSteps(steps []any) []any {
	kept := make([]any, 0, len(steps))
	for _, step := range steps {
		if !slices.Contains(forkHeavyDroppedSteps, forkHeavyStepLabel(step)) {
			kept = append(kept, step)
		}
	}
	return kept
}

func forkHeavyShow(value any) string {
	text := fmt.Sprintf("%v", value)
	if len(text) > 120 {
		text = text[:120] + "..."
	}
	return text
}

// forkHeavyDiff lists, one line per leaf, where got departs from want.
func forkHeavyDiff(path string, want, got any) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a mapping, got %s", path, forkHeavyShow(got))}
		}
		keys := map[string]bool{}
		for key := range w {
			keys[key] = true
		}
		for key := range g {
			keys[key] = true
		}
		var diffs []string
		for _, key := range sortedKeys(keys) {
			wantValue, inWant := w[key]
			gotValue, inGot := g[key]
			switch {
			case !inGot:
				diffs = append(diffs, fmt.Sprintf("%s.%s: missing, want %s", path, key, forkHeavyShow(wantValue)))
			case !inWant:
				diffs = append(diffs, fmt.Sprintf("%s.%s: unexpected %s", path, key, forkHeavyShow(gotValue)))
			default:
				diffs = append(diffs, forkHeavyDiff(path+"."+key, wantValue, gotValue)...)
			}
		}
		return diffs
	case []any:
		g, ok := got.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: want a list, got %s", path, forkHeavyShow(got))}
		}
		var diffs []string
		if len(g) != len(w) {
			diffs = append(diffs, fmt.Sprintf("%s: %d items, want %d", path, len(g), len(w)))
		}
		for i := 0; i < len(w) && i < len(g); i++ {
			diffs = append(diffs, forkHeavyDiff(fmt.Sprintf("%s[%d]", path, i), w[i], g[i])...)
		}
		return diffs
	}
	if reflect.DeepEqual(want, got) {
		return nil
	}
	return []string{fmt.Sprintf("%s: want %s, got %s", path, forkHeavyShow(want), forkHeavyShow(got))}
}

// Criterion 1. The file holds the eight lifted jobs and one aggregator, and the
// aggregator needs every one of the eight.
func TestForkHeavyTiersJobSetAndAggregator(t *testing.T) {
	wf := forkHeavyWorkflow(t)
	aggregatorID, aggregator := forkHeavyAggregator(t, wf)
	want := forkHeavyJobIDs()

	var got []string
	for id := range wf.Jobs {
		if id != aggregatorID {
			got = append(got, id)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("%s jobs besides the aggregator = %v, want exactly %v (section 3.4: the eight lifted job ids, no more)", forkHeavyTiersWorkflow, got, want)
	}

	needs := append([]string(nil), aggregator.Needs...)
	sort.Strings(needs)
	if !slices.Equal(needs, want) {
		t.Errorf("aggregator %s needs %v, want every other job in the file: %v", aggregatorID, needs, want)
	}
	if forkHeavyIf(aggregator.If) != "always()" {
		t.Errorf("aggregator %s has if: %q, want always() (a skipped or failed need must reach the aggregator, not skip it)", aggregatorID, aggregator.If)
	}
	if aggregator.ContinueOnError {
		t.Errorf("aggregator %s sets continue-on-error; its failure is the signal", aggregatorID)
	}
	for i, step := range aggregator.Steps {
		if step.ContinueOnError != nil {
			t.Errorf("aggregator %s step %d (%q) sets continue-on-error; its failure is the signal", aggregatorID, i, step.Name)
		}
	}
	if !forkHeavyHostedRunnerRe.MatchString(aggregator.RunsOn) {
		t.Errorf("aggregator %s runs-on = %q, want a GitHub-hosted ubuntu runner like every other job in the file (section 5.1 rule 4)", aggregatorID, aggregator.RunsOn)
	}
}

var (
	forkHeavyHostedRunnerRe = regexp.MustCompile(`^ubuntu-(latest|[0-9]{2}\.[0-9]{2})$`)
	forkHeavyNeedsResultRe  = regexp.MustCompile(`\$\{\{\s*needs\.([A-Za-z0-9_-]+)\.result\s*\}\}`)
	forkHeavyExprRe         = regexp.MustCompile(`\$\{\{.*?\}\}`)
)

// forkHeavyEval resolves ${{ needs.<job>.result }}, the one expression the
// aggregator simulation understands. Any other expression fails the test, so the
// simulation cannot silently drift from the workflow (runPRGateStep does the
// same for pr.yml's gate).
func forkHeavyEval(t *testing.T, where, text string, results map[string]string) string {
	t.Helper()
	text = forkHeavyNeedsResultRe.ReplaceAllStringFunc(text, func(expr string) string {
		id := forkHeavyNeedsResultRe.FindStringSubmatch(expr)[1]
		result, ok := results[id]
		if !ok {
			t.Fatalf("%s reads needs.%s.result, and %s is not a job in this file", where, id, id)
		}
		return result
	})
	if expr := forkHeavyExprRe.FindString(text); expr != "" {
		t.Fatalf("%s uses %s: the aggregator may read only ${{ needs.<job>.result }}, the one expression the simulation evaluates (pr-risk.yml's ci-gate is the shape to follow)", where, expr)
	}
	return text
}

// forkHeavyRunAggregator runs the aggregator's own run steps, under GitHub's
// bash flags, with every need at the result the scenario gives it, and reports
// whether the job would pass. Steps that use an action (the checkout) are
// skipped: the files are already here.
func forkHeavyRunAggregator(t *testing.T, bash, id string, job ciWorkflowJob, results map[string]string) (bool, string) {
	t.Helper()
	root := sourceRepoRoot(t)
	var log strings.Builder
	ran := 0
	for i, step := range job.Steps {
		where := fmt.Sprintf("aggregator %s step %d (%q)", id, i, step.Name)
		if step.Uses != "" {
			continue
		}
		switch forkHeavyIf(step.If) {
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
				env = append(env, key+"="+forkHeavyEval(t, where+" env "+key, value, results))
			}
		}
		cmd := exec.Command(bash, append(flags, "-c", forkHeavyEval(t, where+" run", step.Run, results))...)
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

// Criterion 1. The aggregator fails on any non-success need, skipped included:
// the only way to a green Fork Heavy Tiers / Required is that all eight jobs
// ran and passed. A skipped heavy tier is exactly the silent gap this workflow
// exists to close.
func TestForkHeavyTiersAggregatorFailsUnlessEveryNeedSucceeded(t *testing.T) {
	bash := requireHostTool(t, "bash")
	aggregatorID, aggregator := forkHeavyAggregator(t, forkHeavyWorkflow(t))
	ids := forkHeavyJobIDs()
	scenario := func(id, result string) map[string]string {
		results := map[string]string{}
		for _, need := range ids {
			results[need] = "success"
		}
		if id != "" {
			results[id] = result
		}
		return results
	}

	if ok, out := forkHeavyRunAggregator(t, bash, aggregatorID, aggregator, scenario("", "")); !ok {
		t.Errorf("the aggregator fails although all %d jobs succeeded:\n%s", len(ids), out)
	}
	for _, id := range ids {
		for _, result := range []string{"failure", "cancelled", "skipped"} {
			if ok, out := forkHeavyRunAggregator(t, bash, aggregatorID, aggregator, scenario(id, result)); ok {
				t.Errorf("the aggregator passes when needs.%s.result is %s; it must fail unless every job's result is exactly success (skipped is not acceptable):\n%s", id, result, out)
			}
		}
	}
}

// Criterion 2, the shape. The workflow runs pull-request code, so like
// fork-macos.yml it may only run unprivileged: triggers that carry no secrets, a
// read-only token, no job that widens its own permissions. It is the integration
// pull requests' lane, a path filter on it would let a heavy tier go unrun, and
// merge_group is out because vb has no merge queue (ruling section 3.4: if it
// ever enables one, re-rule).
func TestForkHeavyTiersRunsUnprivilegedOnIntegrationPullRequests(t *testing.T) {
	rel := ".github/workflows/" + forkHeavyTiersWorkflow
	var wf struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]any    `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Concurrency map[string]any    `yaml:"concurrency"`
		Jobs        map[string]struct {
			Permissions any `yaml:"permissions"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(forkHeavyRead(t), &wf); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}

	wantOn := map[string]any{"pull_request": map[string]any{"branches": []any{"integration"}}, "workflow_dispatch": nil}
	if !reflect.DeepEqual(wf.On, wantOn) {
		t.Errorf("%s triggers = %v, want %v (no path filter, no merge_group, never pull_request_target or workflow_run: it runs pull-request code)", rel, wf.On, wantOn)
	}
	if !reflect.DeepEqual(wf.Permissions, map[string]any{"contents": "read"}) {
		t.Errorf("%s permissions = %v, want exactly contents: read", rel, wf.Permissions)
	}
	wantEnv := map[string]string{"BD_DISABLE_METRICS": "1", "BD_DISABLE_EVENT_FLUSH": "1"}
	if !reflect.DeepEqual(wf.Env, wantEnv) {
		t.Errorf("%s env = %v, want %v (the workflow-level env the lifted jobs rely on, as in fork-macos.yml)", rel, wf.Env, wantEnv)
	}
	wantConcurrency := map[string]any{
		"group":              "${{ github.workflow }}-${{ github.event_name }}-${{ github.event.pull_request.number || github.ref }}",
		"cancel-in-progress": true,
	}
	if !reflect.DeepEqual(wf.Concurrency, wantConcurrency) {
		t.Errorf("%s concurrency = %v, want %v", rel, wf.Concurrency, wantConcurrency)
	}
	for id, job := range wf.Jobs {
		if job.Permissions != nil {
			t.Errorf("%s job %s sets its own permissions %v", rel, id, job.Permissions)
		}
	}
}

// Text the ruling bars from the file. Upstream's sweeps read every workflow file
// as text and fork-macos.yml already passes them, so this file has to as well
// (section 3.4); the same words are barred from all three fork-owned workflows
// by R6 (section 5.1 rule 4), which says "NO Blacksmith text", not "no
// Blacksmith runner". Comments count.
var forkHeavyBarredText = []string{
	"golangci", "blacksmith", "secrets.", "run-id", "workflow_run", "rbe_", "pull_request_target",
}

var forkHeavyMakeRe = regexp.MustCompile(`\bmake\b`)

// forkHeavyLine is one line of a scalar value: a run script, a name, a condition.
type forkHeavyLine struct{ at, text string }

// forkHeavyLines returns the workflow's values line by line, YAML comments and
// shell comment lines left out: what the runner would act on.
func forkHeavyLines(t *testing.T) []forkHeavyLine {
	t.Helper()
	forkHeavyRead(t)
	var lines []forkHeavyLine
	walkYAML(readYAMLNode(t, ".github/workflows/"+forkHeavyTiersWorkflow), "", func(path string, key bool, value string) {
		if key {
			return
		}
		for _, line := range strings.Split(value, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				lines = append(lines, forkHeavyLine{path, line})
			}
		}
	})
	return lines
}

func TestForkHeavyTiersNamesNothingTheRulingBars(t *testing.T) {
	text := strings.ToLower(string(forkHeavyRead(t)))
	for _, barred := range forkHeavyBarredText {
		if i := strings.Index(text, barred); i >= 0 {
			t.Errorf("%s:%d names %q; section 3.4 and R6 (section 5.1 rule 4) bar it from the file, comments included", forkHeavyTiersWorkflow, strings.Count(text[:i], "\n")+1, barred)
		}
	}
	for _, line := range forkHeavyLines(t) {
		if forkHeavyMakeRe.MatchString(line.text) {
			t.Errorf("%s %s calls make (%q); section 3.4 bars a make call here, and upstream's TestWorkflowsNameTheirTestEngine flags the ones that run the primary engine", forkHeavyTiersWorkflow, line.at, strings.TrimSpace(line.text))
		}
	}
}

// A path the workflow names under .github/scripts, scripts or tools, ignoring
// matches inside a longer path (internal/scripts/..., $WORKSPACE/scripts/...).
var forkHeavyHelperPathRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])(?:\./)?((?:\.github/scripts|scripts|tools)/[A-Za-z0-9_./-]+)`)

// The helpers the lifted jobs run. If the scan below stops finding one, it has
// stopped reading the file and the existence check is passing for nothing.
var forkHeavyKnownHelpers = []string{
	".github/scripts/embedded-storage-test-shard.sh",
	".github/scripts/embedded-test-shard.sh",
	".github/scripts/proxied-test-shard.sh",
	".github/scripts/server-storage-test-shard.sh",
	"scripts/ci/install-dolt.sh",
	"scripts/ci/pull-dolt-image.sh",
}

// Criterion 2 and the self-protection pin of section 3.4: every local helper
// the file names exists. If upstream deletes or moves one, this fails loudly
// instead of the lane rotting until a pull request runs it.
func TestForkHeavyTiersNamesOnlyHelpersThatExist(t *testing.T) {
	root := sourceRepoRoot(t)
	seen := map[string]string{}
	for _, line := range forkHeavyLines(t) {
		for _, match := range forkHeavyHelperPathRe.FindAllStringSubmatch(line.text, -1) {
			path := strings.TrimRight(match[1], ".,;:")
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = line.at
			if strings.HasPrefix(path, "tools/") && os.Getenv("TEST_SRCDIR") != "" {
				continue // scripts_test's runfiles hold no tools/; go test checks it
			}
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
				t.Errorf("%s %s runs %s, which is not in the tree: if upstream moved or deleted it, follow it here in the same change (ruling section 3.4): %v", forkHeavyTiersWorkflow, line.at, path, err)
			}
		}
	}
	for _, path := range forkHeavyKnownHelpers {
		if _, ok := seen[path]; !ok {
			t.Errorf("%s does not name %s, a helper its lifted jobs run (or the scan no longer finds it)", forkHeavyTiersWorkflow, path)
		}
	}
}

// Criterion 2, the lifted shape without git: each job keeps the ids, names,
// needs, timeouts, matrices and steps of its source, less managed-local-smoke's
// two cache steps (edit 5, be-bg8llq), so a retype or a trim shows even where
// history is not at hand (TestForkHeavyTiersLiftMatchesSourceBlobs then compares
// every byte of the values).
func TestForkHeavyTiersJobsKeepTheirShape(t *testing.T) {
	jobs := forkHeavyRawJobs(t)
	legs := 0
	for _, want := range forkHeavyJobs {
		job, ok := jobs[want.id]
		if !ok {
			t.Errorf("job %s is missing", want.id)
			continue
		}
		if wantName := forkHeavyNamePrefix + want.name; job["name"] != wantName {
			t.Errorf("job %s: name = %q, want %q (the lift prefixes %q, edit 1)", want.id, job["name"], wantName, forkHeavyNamePrefix)
		}
		var wantNeeds []string
		if want.needsBuild {
			wantNeeds = []string{"build-embedded"}
		}
		if gotNeeds := forkHeavyNeeds(job["needs"]); !slices.Equal(gotNeeds, wantNeeds) {
			t.Errorf("job %s: needs = %v, want %v (edit 2: build-embedded for the test jobs, none for build-embedded and managed-local-smoke)", want.id, gotNeeds, wantNeeds)
		}
		if _, ok := job["if"]; ok {
			t.Errorf("job %s: still has its tier if: %v (edit 3 drops it: the workflow runs everything)", want.id, job["if"])
		}
		if job["runs-on"] != "ubuntu-latest" {
			t.Errorf("job %s: runs-on = %v, want ubuntu-latest (edit 4)", want.id, job["runs-on"])
		}
		for key := range job {
			if !slices.Contains(forkHeavyJobKeys, key) && key != "if" {
				t.Errorf("job %s: has %s, which its source does not", want.id, key)
			}
		}
		gotTimeout, set := job["timeout-minutes"]
		if want.timeout == 0 && set || want.timeout != 0 && gotTimeout != want.timeout {
			t.Errorf("job %s: timeout-minutes = %v, want %d (0 is unset); timeouts never loosen", want.id, gotTimeout, want.timeout)
		}
		if want.matrix == nil {
			if job["strategy"] != nil {
				t.Errorf("job %s: strategy = %v, want none", want.id, job["strategy"])
			}
		} else if wantStrategy := (map[string]any{"fail-fast": false, "matrix": want.matrix}); !reflect.DeepEqual(job["strategy"], wantStrategy) {
			t.Errorf("job %s: strategy = %v, want %v", want.id, job["strategy"], wantStrategy)
		}
		if gotSteps := forkHeavyStepLabels(job); !slices.Equal(gotSteps, want.steps) {
			t.Errorf("job %s: steps = %q, want %q (step names stay verbatim; managed-local-smoke alone loses its cache unit, edit 5 of be-bg8llq, and keeps six)", want.id, gotSteps, want.steps)
		}

		count := 1
		if strategy, ok := job["strategy"].(map[string]any); ok {
			if matrix, ok := strategy["matrix"].(map[string]any); ok {
				for _, values := range matrix {
					if list, ok := values.([]any); ok {
						count *= len(list)
					}
				}
			}
		}
		legs += count
	}
	if legs != forkHeavyLegs {
		t.Errorf("the lifted jobs expand to %d legs, want %d (section 3.4: all of them, on every integration pull request)", legs, forkHeavyLegs)
	}
}

// forkHeavyLift applies the five ruled edits to a source job: the name gains the
// prefix, needs go (the caller compares them as a set), the tier if: goes, a
// Blacksmith runs-on becomes ubuntu-latest, and the steps of forkHeavyDroppedSteps
// go (be-bg8llq; the comment above them is not YAML, so it is not here).
func forkHeavyLift(t *testing.T, id string, source map[string]any) map[string]any {
	t.Helper()
	job := map[string]any{}
	for key, value := range source {
		job[key] = value
	}
	name, ok := job["name"].(string)
	if !ok {
		t.Fatalf("source job %s has name %v, want a string", id, job["name"])
	}
	job["name"] = forkHeavyNamePrefix + name
	delete(job, "needs")
	delete(job, "if")
	if runsOn, ok := job["runs-on"].(string); ok && strings.Contains(strings.ToLower(runsOn), "blacksmith") {
		job["runs-on"] = "ubuntu-latest"
	}
	if steps, ok := job["steps"].([]any); ok {
		job["steps"] = forkHeavyKeptSteps(steps)
	}
	return job
}

// Criterion 2, the lift diff. Every lifted job equals its source blob at
// forkHeavyBase after the five ruled edits (section 3.4 as amended by be-bg8llq
// section 2), and nothing else: not a retyped command, a loosened timeout or a
// reworded step. Edit 5 fires for managed-local-smoke alone and takes exactly
// the two steps of forkHeavyDroppedSteps, which the last check below counts.
func TestForkHeavyTiersLiftMatchesSourceBlobs(t *testing.T) {
	if os.Getenv("TEST_SRCDIR") != "" {
		t.Skip("the source blobs come from git history, which scripts_test's runfiles do not hold; runs under go test")
	}
	git := requireHostTool(t, "git")
	root := sourceRepoRoot(t)
	if err := exec.Command(git, "-C", root, "cat-file", "-e", forkHeavyBase+"^{commit}").Run(); err != nil {
		t.Skipf("commit %s is not in this clone (a shallow checkout?), so the lift cannot be compared with its source blobs; TestForkHeavyTiersJobsKeepTheirShape still pins its shape", forkHeavyBase)
	}
	got := forkHeavyRawJobs(t)

	sources := map[string]map[string]any{}
	sourceJobs := func(file string) map[string]any {
		if jobs, ok := sources[file]; ok {
			return jobs
		}
		blob, err := exec.Command(git, "-C", root, "show", forkHeavyBase+":.github/workflows/"+file).Output()
		if err != nil {
			t.Fatalf("git show %s:.github/workflows/%s: %v", forkHeavyBase, file, err)
		}
		var doc struct {
			Jobs map[string]any `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(blob, &doc); err != nil {
			t.Fatalf("parse %s at %s: %v", file, forkHeavyBase, err)
		}
		sources[file] = doc.Jobs
		return doc.Jobs
	}

	dropped := map[string]int{} // job id -> source steps edit 5 takes out of it
	for _, want := range forkHeavyJobs {
		source, ok := sourceJobs(want.source)[want.id].(map[string]any)
		if !ok {
			t.Fatalf("%s at %s has no job %s", want.source, forkHeavyBase, want.id)
		}
		lifted, ok := got[want.id]
		if !ok {
			t.Errorf("job %s is missing", want.id)
			continue
		}
		var wantNeeds []string
		if want.needsBuild {
			wantNeeds = []string{"build-embedded"}
		}
		if gotNeeds := forkHeavyNeeds(lifted["needs"]); !slices.Equal(gotNeeds, wantNeeds) {
			t.Errorf("job %s: needs = %v, want %v", want.id, gotNeeds, wantNeeds)
		}
		delete(lifted, "needs")

		expected := forkHeavyLift(t, want.id, source)
		if diffs := forkHeavyDiff(want.id, expected, lifted); len(diffs) > 0 {
			t.Errorf("job %s differs from %s:%s at %s by more than the five ruled edits (be-bg8llq):\n  %s", want.id, want.source, want.id, forkHeavyBase[:12], strings.Join(diffs, "\n  "))
		}
		sourceSteps, _ := source["steps"].([]any)
		keptSteps, _ := expected["steps"].([]any)
		if cut := len(sourceSteps) - len(keptSteps); cut > 0 {
			dropped[want.id] = cut
		}
	}
	if wantDropped := map[string]int{"managed-local-smoke": len(forkHeavyDroppedSteps)}; !reflect.DeepEqual(dropped, wantDropped) {
		t.Errorf("edit 5 took steps %v out of the source jobs, want %v: it fires for managed-local-smoke alone and takes exactly %q (be-bg8llq section 2)", dropped, wantDropped, forkHeavyDroppedSteps)
	}
}
