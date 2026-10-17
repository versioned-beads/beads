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
	"strconv"
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

// shellLines keeps the line structure of a script and folds its continuations
// and its indentation. Two scripts that differ in where a command ends (a
// newline or a space) are different scripts.
func shellLines(script string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(script, "\\\n", " "), "\n") {
		if folded := strings.Join(strings.Fields(line), " "); folded != "" {
			lines = append(lines, folded)
		}
	}
	return strings.Join(lines, "\n")
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
	if got := shellLines(test.Run); got != shellLines(wantTest) {
		t.Errorf("step %q run = %q, want %q (every gated test by name, with -race and -v, the output kept for the verify step)", testStep, got, shellLines(wantTest))
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
	if got := shellLines(verify.Run); got != shellLines(wantVerify) {
		t.Errorf("step %q run = %q, want %q (a skip or a missing test exits 0 under go test, so each name must be read from the log)", verifyStep, got, shellLines(wantVerify))
	}
	assertStepsBefore(t, job, []string{testStep}, []string{verifyStep})
	forkScriptLineControls(t, forkPinScript{
		label: "test-script", shipped: test.Run, want: wantTest, continued: true,
		token: forkPinEdit{"a-different-count", "-count=1", "-count=2"},
	})
	forkScriptLineControls(t, forkPinScript{
		label: "verify-script", shipped: verify.Run, want: wantVerify,
		token: forkPinEdit{"a-different-status", "status=1", "status=0"}, named: forkVerifyLineEdits,
	})
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
	if got := shellLines(test.Run); got != shellLines(wantTest) {
		t.Errorf("step %q run = %q, want %q (every guard by name, with -v, the output kept for the verify step)", testStep, got, shellLines(wantTest))
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
	if got := shellLines(verify.Run); got != shellLines(wantVerify) {
		t.Errorf("step %q run = %q, want %q (a skip or a missing test exits 0 under go test, so each name must be read from the log)", verifyStep, got, shellLines(wantVerify))
	}
	assertStepsBefore(t, job, []string{testStep}, []string{verifyStep})
	forkScriptLineControls(t, forkPinScript{
		label: "test-script", shipped: test.Run, want: wantTest, continued: true,
		token: forkPinEdit{"a-different-count", "-count=1", "-count=2"},
	})
	forkScriptLineControls(t, forkPinScript{
		label: "verify-script", shipped: verify.Run, want: wantVerify,
		token: forkPinEdit{"a-different-status", "status=1", "status=0"}, named: forkVerifyLineEdits,
	})

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

// Line structure of the scripts the pins compare (be-glyo9v, the N1 follow-up
// of be-zbhceo). A comparison that folds every line break into a space keeps
// the YAML indentation out of the comparison, and it also hides the breaks the
// shell does read: `echo "..."` and `status=1` joined onto one line, or `exit`
// and `"$status"` split over two, still compare equal to the pinned script,
// and a verify step edited that way no longer fails for a skipped test. A pin
// has to take the shipped script and every edit that changes nothing the shell
// reads (indentation, blank lines, trailing blanks, a re-wrapped
// continuation), and has to reject every edit that does (a line break joined
// or made, a blank line or a blank after a continuation's backslash, a changed
// token). forkScriptLineControls holds each of the four scripts the guards
// above compare, the test step and the verify step of both jobs, to that.

// forkPinAccepts: whether the pins take got for the pinned script want. It is
// the one place the controls meet the comparison the guards make.
func forkPinAccepts(got, want string) bool {
	return shellLines(got) == shellLines(want)
}

// forkPinEdit: one named edit of a pinned script, old replaced by new where
// old occurs exactly once.
type forkPinEdit struct{ name, old, new string }

// forkPinVariant: a pinned script with one change, labelled for the subtest or
// failure that reports it.
type forkPinVariant struct{ label, script string }

// forkPinScript: one run: script a pin compares and what the controls need of
// it.
type forkPinScript struct {
	label     string        // names the subtest that holds the controls
	shipped   string        // the step's run: as the workflow ships it
	want      string        // the text the pin compares it with
	continued bool          // want has a backslash-newline continuation
	token     forkPinEdit   // a changed token, which every comparison rejects
	named     []forkPinEdit // line-break edits that each change what the shell runs
}

// forkVerifyLineEdits: single line-break edits of the verify scripts, the ones
// the architect ran under bash -e (be-zbhceo N1), each of which leaves the step
// green for a skipped or missing test. Both jobs' verify scripts have the same
// text, so they share the list.
var forkVerifyLineEdits = []forkPinEdit{
	// status=1 becomes an argument of echo, so nothing ever sets it.
	{"echo-and-status-joined", "failed)\"\n    status=1", "failed)\" status=1"},
	// A bare exit takes the last command's status; "$status" never runs.
	{"exit-and-status-split", "exit \"$status\"", "exit\n\"$status\""},
	// The if tests -E, which is not a command, so its branch never runs.
	{"grep-and-flag-split", "if ! grep -E", "if ! grep\n-E"},
	// The pattern becomes two, and ^--- also matches a --- SKIP line.
	{"pattern-split-over-two-lines", "\"^--- PASS: ${name}", "\"^---\nPASS: ${name}"},
}

// forkApplyEdit: want with e applied. The edit has to find its text exactly
// once, or the control would try nothing.
func forkApplyEdit(t *testing.T, want string, e forkPinEdit) string {
	t.Helper()
	if n := strings.Count(want, e.old); n != 1 {
		t.Fatalf("edit %s: %q occurs %d times in the pinned script, want once; change the edit with the script", e.name, e.old, n)
	}
	return strings.Replace(want, e.old, e.new, 1)
}

// forkMapLines: want with f applied to each of its lines.
func forkMapLines(want string, f func(line string) string) string {
	lines := strings.Split(want, "\n")
	for i, line := range lines {
		lines[i] = f(line)
	}
	return strings.Join(lines, "\n")
}

// forkHarmlessVariants: want with an edit the shell does not read. A blank
// line goes only after a line that does not end in a backslash, because after
// one it ends the continuation.
func forkHarmlessVariants(want string, continued bool) []forkPinVariant {
	blankLines := func(blank string) string {
		var b strings.Builder
		b.WriteString(blank + "\n")
		for _, line := range strings.Split(want, "\n") {
			b.WriteString(line + "\n")
			if !strings.HasSuffix(line, `\`) {
				b.WriteString(blank + "\n")
			}
		}
		return b.String()
	}
	variants := []forkPinVariant{
		{"indentation-removed", forkMapLines(want, func(l string) string { return strings.TrimLeft(l, " \t") })},
		{"indented-six-more-spaces", forkMapLines(want, func(l string) string {
			if strings.TrimSpace(l) == "" {
				return l
			}
			return "      " + l
		})},
		{"tab-indented", forkMapLines(want, func(l string) string { return "\t" + strings.TrimLeft(l, " \t") })},
		{"blank-lines", blankLines("")},
		{"whitespace-only-lines", blankLines("   \t ")},
		{"trailing-blanks", forkMapLines(want, func(l string) string {
			if strings.HasSuffix(l, `\`) {
				return l
			}
			return l + "  \t "
		})},
	}
	if continued {
		variants = append(variants, forkPinVariant{"rewrapped-continuation", strings.ReplaceAll(want, "\\\n", "\\\n        ")})
	}
	return variants
}

// forkLineJoins: want with one line break joined into a single space, for each
// pair of adjacent non-blank lines whose first does not end in a backslash.
func forkLineJoins(want string) []forkPinVariant {
	var out []forkPinVariant
	lines := strings.Split(want, "\n")
	for i := 0; i+1 < len(lines); i++ {
		a, b := strings.TrimRight(lines[i], " \t"), strings.TrimLeft(lines[i+1], " \t")
		if a == "" || b == "" || strings.HasSuffix(a, `\`) {
			continue
		}
		joined := append([]string{}, lines[:i]...)
		joined = append(joined, a+" "+b)
		joined = append(joined, lines[i+2:]...)
		out = append(out, forkPinVariant{
			"join line " + strconv.Itoa(i+1) + " and line " + strconv.Itoa(i+2),
			strings.Join(joined, "\n"),
		})
	}
	return out
}

// forkSpaceSplits: want with one space turned into a line break, for each
// single space between two words of a line (not indentation, not next to a
// blank or a line break, not after a backslash).
func forkSpaceSplits(want string) []forkPinVariant {
	var out []forkPinVariant
	for i := 1; i+1 < len(want); i++ {
		if want[i] != ' ' {
			continue
		}
		start := strings.LastIndex(want[:i], "\n") + 1
		if strings.TrimSpace(want[start:i]) == "" {
			continue
		}
		if want[i-1] == ' ' || want[i+1] == ' ' || want[i+1] == '\n' || want[i-1] == '\\' {
			continue
		}
		out = append(out, forkPinVariant{
			"split line " + strconv.Itoa(strings.Count(want[:i], "\n")+1) + " at column " + strconv.Itoa(i-start+1),
			want[:i] + "\n" + want[i+1:],
		})
	}
	return out
}

// forkContinuationBreaks: want with a break put in at each backslash-newline,
// one variant per continuation. edit gets the text up to and including the
// backslash and the text from the newline on.
func forkContinuationBreaks(want, kind string, edit func(upToBackslash, fromNewline string) string) []forkPinVariant {
	var out []forkPinVariant
	for from := 0; ; {
		i := strings.Index(want[from:], "\\\n")
		if i < 0 {
			return out
		}
		at := from + i
		out = append(out, forkPinVariant{
			kind + " after the continuation on line " + strconv.Itoa(strings.Count(want[:at], "\n")+1),
			edit(want[:at+1], want[at+1:]),
		})
		from = at + 2
	}
}

// forkScriptLineControls: the controls for one script, as the subtests of
// s.label. The shipped script and each edit the shell does not read have to be
// taken; each edit that changes what the shell reads has to be rejected, and
// the subtests that must reject say what they tried when the pin takes it.
func forkScriptLineControls(t *testing.T, s forkPinScript) {
	t.Helper()
	if has := strings.Contains(s.want, "\\\n"); has != s.continued {
		t.Errorf("%s: continued is %v but the pinned script has a continuation: %v; the continuation controls would try nothing or the wrong thing", s.label, s.continued, has)
		return
	}
	rejects := func(t *testing.T, what string, variants []forkPinVariant) {
		t.Helper()
		if len(variants) == 0 {
			t.Fatalf("no %s to try on the pinned script", what)
		}
		var taken []string
		example := ""
		for _, v := range variants {
			if v.script == s.want {
				t.Fatalf("%s %s leaves the pinned script as it was", what, v.label)
			}
			if forkPinAccepts(v.script, s.want) {
				if example == "" {
					example = v.script
				}
				taken = append(taken, v.label)
			}
		}
		if len(taken) > 0 {
			t.Errorf("the pin accepts %d of %d %s as the pinned script: %s\nfor example:\n%s", len(taken), len(variants), what, strings.Join(taken, "; "), example)
		}
	}
	t.Run(s.label, func(t *testing.T) {
		t.Run("shipped-equals-expected", func(t *testing.T) {
			if !forkPinAccepts(s.shipped, s.want) {
				t.Errorf("the pin does not take the shipped script\n%s\nfor the pinned one\n%s", s.shipped, s.want)
			}
		})
		for _, v := range forkHarmlessVariants(s.want, s.continued) {
			t.Run("accepts-"+v.label, func(t *testing.T) {
				if v.script == s.want {
					t.Fatalf("%s leaves the pinned script as it was", v.label)
				}
				if !forkPinAccepts(v.script, s.want) {
					t.Errorf("the pin does not take %s for the pinned script; the shell reads both the same:\n%s", v.label, v.script)
				}
			})
		}
		for _, e := range s.named {
			t.Run("rejects-"+e.name, func(t *testing.T) {
				rejects(t, "edits", []forkPinVariant{{e.name, forkApplyEdit(t, s.want, e)}})
			})
		}
		t.Run("rejects-every-single-edit", func(t *testing.T) {
			joins, splits := forkLineJoins(s.want), forkSpaceSplits(s.want)
			if len(joins) == 0 || len(splits) == 0 {
				t.Fatalf("%d line joins and %d space splits of the pinned script: a family with nothing in it tries nothing", len(joins), len(splits))
			}
			t.Logf("%d line joins + %d space splits = %d single edits of the pinned script", len(joins), len(splits), len(joins)+len(splits))
			edits := make([]forkPinVariant, 0, len(joins)+len(splits))
			edits = append(edits, joins...)
			edits = append(edits, splits...)
			rejects(t, "single line-break edits", edits)
		})
		t.Run("rejects-a-changed-token", func(t *testing.T) {
			rejects(t, "edits", []forkPinVariant{{s.token.name, forkApplyEdit(t, s.want, s.token)}})
		})
		if s.continued {
			t.Run("rejects-a-blank-line-after-a-continuation", func(t *testing.T) {
				rejects(t, "blank lines after a continuation", append(
					forkContinuationBreaks(s.want, "empty line", func(upTo, from string) string { return upTo + "\n" + from }),
					forkContinuationBreaks(s.want, "whitespace-only line", func(upTo, from string) string { return upTo + "\n   \t" + from })...))
			})
			t.Run("rejects-a-blank-after-a-backslash", func(t *testing.T) {
				rejects(t, "blanks after a backslash", append(
					forkContinuationBreaks(s.want, "space", func(upTo, from string) string { return upTo + " " + from }),
					forkContinuationBreaks(s.want, "tab", func(upTo, from string) string { return upTo + "\t" + from })...))
			})
		}
	})
}
