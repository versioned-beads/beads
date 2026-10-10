package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins the fork's retained-evidence step in .github/workflows/bazel.yml
// (be-4y7b5s 6.4b).
//
// Upstream's job bazel-test uploads its test logs only when the test step
// failed. The fork uploads whenever the test step ran, and adds the XML result
// of every target and the build event file to what it keeps:
//
//	# Retain successful shard evidence too: a green target alone does not
//	# establish that every graph test ran in exactly one native shard.
//	- name: Upload test logs
//	  if: ${{ always() && steps.test.outcome != '' && steps.test.outcome != 'skipped' }}
//	  with:
//	    name: bazel-testlogs
//	    path: |
//	      bazel-testlogs/**/test.log
//	      bazel-testlogs/**/test.xml
//	      ${{ runner.temp }}/bazel-bep.json
//
// The Q1 proof reads test.xml of the PASSED fork-only labels to see an internal
// t.Skip (a <skipped> element), which a green label hides, and upstream's
// failure-only upload never retains it. The two hunks merge clean, so losing
// them in a later sync would be invisible. This pin is why it is not.
//
// It reads the step from the YAML, not from text. It pins what the proof needs
// and no more: the job and exactly one step of that name, an if that has
// always() in it, the artifact's name, and test.xml and the build event file in
// its path. if-no-files-found, the retention and the other path entries are
// left alone. It has two failures, and they have two different fixes:
//   - "Job or step not found": upstream renamed or moved the step. Re-apply the
//     shape on the renamed step and re-point this pin. If test.xml or the build
//     event file no longer exist upstream, re-rule 3.6 step 5.
//   - "Shape lost": the step is there and the fork's hunk is not. Re-apply it.

const (
	forkBazelEvidenceFile = ".github/workflows/bazel.yml"
	forkBazelEvidenceJob  = "bazel-test"
	forkBazelEvidenceStep = "Upload test logs"
	forkBazelEvidenceName = "bazel-testlogs"

	forkBazelEvidenceNotFound = "Job or step not found"
	forkBazelEvidenceLost     = "Shape lost"
)

// forkBazelEvidencePaths are the entries with.path must carry.
var forkBazelEvidencePaths = []string{"bazel-testlogs/**/test.xml", "${{ runner.temp }}/bazel-bep.json"}

func forkBazelEvidenceNode(m *yaml.Node, key string) *yaml.Node {
	for m != nil && m.Kind == yaml.AliasNode {
		m = m.Alias
	}
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	var val *yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			val = m.Content[i+1]
		}
	}
	for val != nil && val.Kind == yaml.AliasNode {
		val = val.Alias
	}
	return val
}

// forkBazelEvidenceProblems checks the source of bazel.yml; nothing is a pass.
func forkBazelEvidenceProblems(src []byte) []string {
	notFound := func(format string, args ...any) []string {
		return []string{fmt.Sprintf("%s: %s", forkBazelEvidenceNotFound, fmt.Sprintf(format, args...)) +
			"; upstream renamed or moved it: re-apply the retained-evidence shape on the renamed step and re-point this pin, and if test.xml or the BEP file ($RUNNER_TEMP/bazel-bep.json) no longer exist upstream, RE-RULE 3.6 step 5 (be-4y7b5s 6.4b)"}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return notFound("%s does not parse as YAML (%v)", forkBazelEvidenceFile, err)
	}
	if len(doc.Content) == 0 {
		return notFound("%s is empty", forkBazelEvidenceFile)
	}
	job := forkBazelEvidenceNode(forkBazelEvidenceNode(doc.Content[0], "jobs"), forkBazelEvidenceJob)
	if job == nil {
		return notFound("%s has no job %q", forkBazelEvidenceFile, forkBazelEvidenceJob)
	}
	var steps []*yaml.Node
	if list := forkBazelEvidenceNode(job, "steps"); list != nil && list.Kind == yaml.SequenceNode {
		for _, step := range list.Content {
			if name := forkBazelEvidenceNode(step, "name"); name != nil && name.Kind == yaml.ScalarNode && name.Value == forkBazelEvidenceStep {
				steps = append(steps, step)
			}
		}
	}
	if len(steps) != 1 {
		return notFound("job %q of %s holds %d steps named %q, want exactly one", forkBazelEvidenceJob, forkBazelEvidenceFile, len(steps), forkBazelEvidenceStep)
	}

	var problems []string
	lost := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: step %q of job %q in %s %s; the fork's retained-evidence hunk is gone (a sync that took upstream's failure-only upload does this): re-apply the hunk (be-4y7b5s 6.4b)",
			forkBazelEvidenceLost, forkBazelEvidenceStep, forkBazelEvidenceJob, forkBazelEvidenceFile, fmt.Sprintf(format, args...)))
	}
	step := steps[0]
	if cond := forkBazelEvidenceNode(step, "if"); cond == nil || cond.Kind != yaml.ScalarNode {
		lost("has no if, so it runs only after every step before it succeeded; want an if with always() in it")
	} else if !strings.Contains(cond.Value, "always()") {
		lost("has if %q, which lacks always(), so a green run keeps no evidence", cond.Value)
	}
	with := forkBazelEvidenceNode(step, "with")
	if with == nil || with.Kind != yaml.MappingNode {
		lost("has no with: block, so it names no artifact and no path")
		return problems
	}
	if name := forkBazelEvidenceNode(with, "name"); name == nil || name.Kind != yaml.ScalarNode || name.Value != forkBazelEvidenceName {
		got := "nothing"
		if name != nil {
			got = fmt.Sprintf("%q", name.Value)
		}
		lost("has with.name %s, want %q", got, forkBazelEvidenceName)
	}
	path := forkBazelEvidenceNode(with, "path")
	if path == nil || path.Kind != yaml.ScalarNode {
		lost("has no with.path text, want entries that include %s", strings.Join(forkBazelEvidencePaths, " and "))
		return problems
	}
	// with.path of upload-artifact is one entry per line, and an entry is matched
	// whole: test.xml.bak, or the entry behind a # (which a block scalar keeps as
	// text), is not the entry.
	have := map[string]bool{}
	for _, line := range strings.Split(path.Value, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	for _, want := range forkBazelEvidencePaths {
		if !have[want] {
			lost("has with.path %q, which lacks the entry %s", strings.TrimSpace(path.Value), want)
		}
	}
	return problems
}

// bazel.yml in the repository today, and after a sync that kept the hunk.
func TestForkBazelRetainsPassingTestEvidence(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), filepath.FromSlash(forkBazelEvidenceFile)))
	if err != nil {
		t.Fatalf("%s: %s: %v; upstream moved or deleted it: re-point this pin, and RE-RULE 3.6 step 5 if the Bazel test job no longer exists (be-4y7b5s 6.4b)", forkBazelEvidenceNotFound, forkBazelEvidenceFile, err)
	}
	for _, p := range forkBazelEvidenceProblems(src) {
		t.Error(p)
	}
}

// Known-positive and known-negative controls on synthetic workflows: every way
// the step can go missing or lose its shape fails with the right one of the two
// messages, and the shapes the pin leaves alone pass.
func TestForkBazelEvidenceScanControls(t *testing.T) {
	const cond = "${{ always() && steps.test.outcome != '' && steps.test.outcome != 'skipped' }}"
	var (
		logs = "bazel-testlogs/**/test.log"
		xml  = "bazel-testlogs/**/test.xml"
		bep  = "${{ runner.temp }}/bazel-bep.json"
	)
	// step renders one step. An empty ifs, artifact or no entries leaves that key
	// out, and more is raw text for the end of the with: block.
	step := func(name, ifs, artifact string, entries []string, more string) string {
		s := "      - name: " + name + "\n"
		if ifs != "" {
			s += "        if: " + ifs + "\n"
		}
		s += "        uses: actions/upload-artifact@v7\n"
		if artifact != "" || len(entries) != 0 || more != "" {
			s += "        with:\n"
			if artifact != "" {
				s += "          name: " + artifact + "\n"
			}
			if len(entries) != 0 {
				s += "          path: |\n"
				for _, e := range entries {
					s += "            " + e + "\n"
				}
			}
			s += more
		}
		return s
	}
	const head = "jobs:\n  bazel-test:\n    steps:\n      - name: Test\n        id: test\n        run: bazel test //...\n"
	doc := func(steps ...string) string { return head + strings.Join(steps, "") }
	all := []string{logs, xml, bep}
	good := step(forkBazelEvidenceStep, cond, "bazel-testlogs", all, "          if-no-files-found: error\n")
	with := func(ifs, artifact string, entries []string) string {
		return doc(step(forkBazelEvidenceStep, ifs, artifact, entries, ""))
	}

	type evidenceCase struct {
		name string
		src  string
		want string // the message the problem starts with; "" for a pass
	}
	cases := []evidenceCase{
		// The shape the fork keeps, and what the pin leaves alone.
		{name: "the fork's step", src: doc(good)},
		{name: "if-no-files-found ignore (left unpinned)", src: doc(step(forkBazelEvidenceStep, cond, "bazel-testlogs", all, "          if-no-files-found: ignore\n"))},
		{name: "retention and overwrite (left unpinned)", src: doc(step(forkBazelEvidenceStep, cond, "bazel-testlogs", all, "          overwrite: true\n          retention-days: 7\n"))},
		{name: "no if-no-files-found", src: with(cond, "bazel-testlogs", all)},
		{name: "an if that is only always()", src: with("${{ always() }}", "bazel-testlogs", all)},
		{name: "more path entries than the pin needs", src: with(cond, "bazel-testlogs", append([]string{"bazel-testlogs/**/coverage.dat"}, all...))},
		{name: "path entries in another order", src: with(cond, "bazel-testlogs", []string{bep, xml})},
		{name: "the same step name in another job as well", src: doc(good) + "  other:\n    steps:\n" + step(forkBazelEvidenceStep, "${{ failure() }}", "other-logs", []string{"x"}, "")},
		{name: "another step in the job under another name", src: doc(good, step("Upload coverage", "${{ failure() }}", "coverage", []string{"x"}, ""))},

		// Upstream renamed or moved it, or two steps share the name.
		{name: "no bazel-test job", src: "jobs:\n  other:\n    steps:\n" + good, want: forkBazelEvidenceNotFound},
		{name: "the job renamed", src: strings.Replace(doc(good), "bazel-test:", "bazel-tests:", 1), want: forkBazelEvidenceNotFound},
		{name: "the step renamed", src: doc(step("Upload bazel logs", cond, "bazel-testlogs", all, "")), want: forkBazelEvidenceNotFound},
		{name: "the step removed", src: doc(), want: forkBazelEvidenceNotFound},
		{name: "the step in another job only", src: doc() + "  other:\n    steps:\n" + good, want: forkBazelEvidenceNotFound},
		{name: "two steps of that name in the job", src: doc(good, good), want: forkBazelEvidenceNotFound},
		{name: "a job with no steps", src: "jobs:\n  bazel-test:\n    runs-on: ubuntu-latest\n", want: forkBazelEvidenceNotFound},
		{name: "a workflow with no jobs", src: "on: push\n", want: forkBazelEvidenceNotFound},
		{name: "a workflow that does not parse", src: "jobs: {\n", want: forkBazelEvidenceNotFound},
		{name: "an empty file", src: "", want: forkBazelEvidenceNotFound},

		// The step is there and the fork's hunk is not (upstream's own text, among others).
		{name: "upstream's failure-only if", src: with("${{ failure() && steps.test.outcome == 'failure' }}", "bazel-testlogs", all), want: forkBazelEvidenceLost},
		{name: "no if at all", src: with("", "bazel-testlogs", all), want: forkBazelEvidenceLost},
		{name: "an if that mentions always but does not call it", src: with("${{ steps.test.outcome != 'always' }}", "bazel-testlogs", all), want: forkBazelEvidenceLost},
		{name: "another artifact name", src: with(cond, "bazel-test-logs", all), want: forkBazelEvidenceLost},
		{name: "no artifact name", src: with(cond, "", all), want: forkBazelEvidenceLost},
		{name: "path without test.xml", src: with(cond, "bazel-testlogs", []string{logs, bep}), want: forkBazelEvidenceLost},
		{name: "path without the build event file (upstream's own path)", src: with(cond, "bazel-testlogs", []string{logs, xml}), want: forkBazelEvidenceLost},
		{name: "path at another build event file", src: with(cond, "bazel-testlogs", []string{xml, "${{ runner.temp }}/bep.json"}), want: forkBazelEvidenceLost},
		{name: "test.xml only as part of another entry", src: with(cond, "bazel-testlogs", []string{logs, xml + ".bak", bep}), want: forkBazelEvidenceLost},
		{name: "the test.xml entry commented out", src: with(cond, "bazel-testlogs", []string{logs, "# " + xml, bep}), want: forkBazelEvidenceLost},
		{name: "no path", src: doc(step(forkBazelEvidenceStep, cond, "bazel-testlogs", nil, "          if-no-files-found: error\n")), want: forkBazelEvidenceLost},
		{name: "no with block", src: with(cond, "", nil), want: forkBazelEvidenceLost},
	}
	for _, c := range cases {
		got := forkBazelEvidenceProblems([]byte(c.src))
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: %q, want a pass", c.name, got)
		case c.want != "" && len(got) == 0:
			t.Errorf("%s: passed, want %q", c.name, c.want)
		case c.want != "":
			other := forkBazelEvidenceLost
			if c.want == forkBazelEvidenceLost {
				other = forkBazelEvidenceNotFound
			}
			for _, p := range got {
				if !strings.HasPrefix(p, c.want+": ") || strings.HasPrefix(p, other) {
					t.Errorf("%s: %q, want every problem to start with %q", c.name, p, c.want)
				}
			}
		}
	}

	// The two messages carry their own fixes, and not each other's.
	notFound := forkBazelEvidenceProblems([]byte(doc(step("Upload bazel logs", cond, "bazel-testlogs", all, ""))))
	lost := forkBazelEvidenceProblems([]byte(with("${{ failure() }}", "bazel-testlogs", all)))
	if len(notFound) != 1 || !strings.Contains(notFound[0], "re-point this pin") || !strings.Contains(notFound[0], "RE-RULE 3.6 step 5") || strings.Contains(notFound[0], "re-apply the hunk") {
		t.Errorf("a renamed step reported %q, want re-point this pin and RE-RULE 3.6 step 5, not re-apply the hunk", notFound)
	}
	if len(lost) != 1 || !strings.Contains(lost[0], "re-apply the hunk") || strings.Contains(lost[0], "RE-RULE") {
		t.Errorf("a lost shape reported %q, want re-apply the hunk, not RE-RULE", lost)
	}
	// Every way the shape can be lost is reported at once.
	if every := forkBazelEvidenceProblems([]byte(with("${{ failure() }}", "x", []string{"y"}))); len(every) != 4 {
		t.Errorf("a step that lost its if, name, test.xml and BEP path reported %d problems %q, want 4", len(every), every)
	}
}
