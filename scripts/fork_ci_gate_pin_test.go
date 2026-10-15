package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict on
// it). It pins E2, the one-line edit the fork keeps in pr.yml's CI Gate step
// (ruling be-4y7b5s section 4, as carried by be-jvluf7).
//
// Upstream's gate treats every Bazel mode but remote, fork-ro and fork-rw as "the
// lanes did not run remotely" and goes red. This fork's pull requests carry no CI
// secrets and rbe-fork's mint is closed to it, so they are always in mode cache
// and the gate would be red on every one of them. E2 lets mode cache pass, and
// only mode cache, and only when GITHUB_REPOSITORY_OWNER is not gastownhall.
// What the gate then stops proving, the fork-owned aggregators carry:
// fork-heavy-tiers.yml and fork-qualification.yml.
//
// The pin is self-contained on purpose. Upstream's runPRGateStep and the other
// gate-harness helpers may be renamed or deleted at any sync target, so this
// file reads pr.yml as text, cuts out the shell block E2 lives in, and runs that
// block under bash against the truth table of the ruling. It asserts exactly that
// table: anything else the block does fails.
//
// If a sync rewrites the gate step so that the block can no longer be found, the
// pin fails with "E2 gate block not found: re-rule (be-4y7b5s)" and the person
// syncing takes it back to the architect. It is not theirs to re-shape.
//
// Retire this file together with E2: when rbe-fork opens to this fork, the mode
// becomes fork-ro or fork-rw, the cache branch never fires, and the edit, this
// file, its line in scripts/BUILD.bazel and fork-heavy-tiers.yml go out as one
// change (fork_heavy_tiers_pin_test.go lists the rest).

const (
	forkCIGateWorkflow      = ".github/workflows/pr.yml"
	forkCIGateBlockStart    = "BAZEL_REMOTE=success"
	forkCIGateBlockEnd      = "export BAZEL_REMOTE"
	forkCIGateNotFound      = "E2 gate block not found: re-rule (be-4y7b5s)"
	forkCIGateSentinel      = "FORK_CI_GATE_RESULT:"
	forkCIGateAnnotation    = "::error::"
	forkCIGateUpstreamOwner = "gastownhall"
)

// forkCIGateOwnerUnset stands for "GITHUB_REPOSITORY_OWNER is not in the
// environment", which is what upstream's own harness runs the block with.
const forkCIGateOwnerUnset = "<unset>"

var (
	forkCIGateOwners = []string{forkCIGateUpstreamOwner, "versioned-beads", "quad341", forkCIGateOwnerUnset}
	forkCIGateModes  = []string{"remote", "fork-ro", "fork-rw", "cache", "local", "skip", ""}
)

// forkCIGateWant is the ruling's truth table: the value of BAZEL_REMOTE after the
// block, and whether the block may print its ::error:: annotation.
//
//	remote, fork-ro, fork-rw: success for every owner.
//	cache: success when the owner is set and is not gastownhall, otherwise
//	       not-run-remotely (an unset owner means gastownhall, as upstream's
//	       harness runs it).
//	local, skip, empty: not-run-remotely for every owner. They mean the rbe job
//	       failed or was bypassed, so they stay red.
func forkCIGateWant(owner, mode string) (result string, annotates bool) {
	switch mode {
	case "remote", "fork-ro", "fork-rw":
		return "success", false
	case "cache":
		if owner != forkCIGateOwnerUnset && owner != forkCIGateUpstreamOwner {
			return "success", false
		}
	}
	return "not-run-remotely", true
}

// forkCIGateExtract cuts the block out of the workflow's text: from the line
// BAZEL_REMOTE=success through the line export BAZEL_REMOTE, inclusive. Each
// marker has to occur once as a whole line, the start before the end, and the
// block has to be plain shell.
func forkCIGateExtract(workflow string) (string, error) {
	lines := strings.Split(workflow, "\n")
	start, end := -1, -1
	starts, ends := 0, 0
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case forkCIGateBlockStart:
			start = i
			starts++
		case forkCIGateBlockEnd:
			end = i
			ends++
		}
	}
	switch {
	case starts != 1:
		return "", fmt.Errorf("%s: the line %q occurs %d times, want exactly once", forkCIGateNotFound, forkCIGateBlockStart, starts)
	case ends != 1:
		return "", fmt.Errorf("%s: the line %q occurs %d times, want exactly once", forkCIGateNotFound, forkCIGateBlockEnd, ends)
	case start > end:
		return "", fmt.Errorf("%s: %q (line %d) comes after %q (line %d)", forkCIGateNotFound, forkCIGateBlockStart, start+1, forkCIGateBlockEnd, end+1)
	}
	block := strings.Join(lines[start:end+1], "\n")
	if strings.Contains(block, "${{") {
		return "", fmt.Errorf("%s: the block holds a ${{ expression, so it is no longer plain shell and this pin's method does not apply", forkCIGateNotFound)
	}
	return block, nil
}

// forkCIGateRun runs the block under GitHub's bash flags with a minimal
// environment and returns what BAZEL_REMOTE holds afterwards and the block's own
// output. The sentinel line is printed by the harness after the block.
func forkCIGateRun(bash, block, owner, mode string) (result, output string, err error) {
	script := block + "\nprintf '%s%s\\n' '" + forkCIGateSentinel + "' \"${BAZEL_REMOTE-unset}\"\n"
	cmd := exec.Command(bash, "--noprofile", "--norc", "-eo", "pipefail", "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "BAZEL_RBE_MODE=" + mode}
	if owner != forkCIGateOwnerUnset {
		cmd.Env = append(cmd.Env, "GITHUB_REPOSITORY_OWNER="+owner)
	}
	out, err := cmd.CombinedOutput()
	output = string(out)
	var sentinels []string
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, forkCIGateSentinel); ok {
			sentinels = append(sentinels, value)
		}
	}
	if err != nil {
		return "", output, fmt.Errorf("the block exited non-zero under bash -eo pipefail: %v", err)
	}
	if len(sentinels) != 1 {
		return "", output, fmt.Errorf("found %d result lines after the block, want 1", len(sentinels))
	}
	return sentinels[0], output, nil
}

// forkCIGateViolations runs the block through every row of the table and lists
// each place its behavior departs from it. An empty list means the block is
// exactly E2.
func forkCIGateViolations(bash, block string) []string {
	var violations []string
	for _, owner := range forkCIGateOwners {
		for _, mode := range forkCIGateModes {
			row := fmt.Sprintf("owner %s, mode %q", owner, mode)
			want, wantAnnotation := forkCIGateWant(owner, mode)
			got, output, err := forkCIGateRun(bash, block, owner, mode)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: %v\n%s", row, err, output))
				continue
			}
			if got != want {
				violations = append(violations, fmt.Sprintf("%s: BAZEL_REMOTE is %q, want %q", row, got, want))
			}
			if hasAnnotation := strings.Contains(output, forkCIGateAnnotation); hasAnnotation != wantAnnotation {
				violations = append(violations, fmt.Sprintf("%s: the block's output has an %s annotation = %t, want %t (a rejected run keeps upstream's annotation, a passing run prints none)", row, forkCIGateAnnotation, hasAnnotation, wantAnnotation))
			}
		}
	}
	return violations
}

// E2. The CI Gate step of pr.yml tolerates mode cache on a fork and nothing else:
// the truth table of the ruling, row by row.
func TestForkCIGateToleratesNonRemoteBazel(t *testing.T) {
	bash := requireHostTool(t, "bash")
	text, err := os.ReadFile(filepath.Join(sourceRepoRoot(t), filepath.FromSlash(forkCIGateWorkflow)))
	if err != nil {
		t.Fatalf("read %s: %v", forkCIGateWorkflow, err)
	}
	block, err := forkCIGateExtract(string(text))
	if err != nil {
		t.Fatalf("%s: %v", forkCIGateWorkflow, err)
	}
	if violations := forkCIGateViolations(bash, block); len(violations) > 0 {
		t.Errorf("%s: the gate block departs from the E2 truth table in %d rows (ruling be-4y7b5s section 4.3: only mode cache on a non-gastownhall owner is tolerated; fork-ro and fork-rw pass for every owner; local, skip and an empty mode stay red):\n%s\nthe block under test:\n%s",
			forkCIGateWorkflow, len(violations), strings.Join(violations, "\n"), block)
	}
}

// The five blocks below are samples, written out in full so that a later change
// to pr.yml's real block cannot break the controls. forkCIGateSampleE2 is the
// block as E2 rules it; the others are the blocks a careless sync or a careless
// edit would leave behind.
const forkCIGateSampleE2 = `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && "$BAZEL_RBE_MODE" == "cache" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`

// forkCIGateMutations are the ways the block can be wrong. Each has to be
// rejected by forkCIGateViolations, or the pin's truth table is not checking
// what it says it checks.
var forkCIGateMutations = []struct{ name, block string }{
	{"upstream's gate before E2: mode cache is red on a fork", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"every mode tolerated: the condition is gone", `BAZEL_REMOTE=success
export BAZEL_REMOTE`},
	{"mode cache tolerated for every owner, gastownhall included", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw|cache)$ ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"an empty mode tolerated on a fork", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && ( "$BAZEL_RBE_MODE" == "cache" || -z "$BAZEL_RBE_MODE" ) ) ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"mode local and skip tolerated on a fork", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && "$BAZEL_RBE_MODE" != "" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"an unset owner treated as a fork", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER-fork}" != "gastownhall" && "$BAZEL_RBE_MODE" == "cache" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"fork-ro dropped from the remote modes", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && "$BAZEL_RBE_MODE" == "cache" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
  echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
fi
export BAZEL_REMOTE`},
	{"an annotation printed on the tolerated path", `BAZEL_REMOTE=success
echo "::error::The Bazel lanes did not run remotely on rbe-west (mode '${BAZEL_RBE_MODE:-none}')."
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && "$BAZEL_RBE_MODE" == "cache" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
fi
export BAZEL_REMOTE`},
	{"the red path loses its annotation", `BAZEL_REMOTE=success
if [[ ! "$BAZEL_RBE_MODE" =~ ^(remote|fork-ro|fork-rw)$ && ! ( "${GITHUB_REPOSITORY_OWNER:-gastownhall}" != "gastownhall" && "$BAZEL_RBE_MODE" == "cache" ) ]]; then
  BAZEL_REMOTE=not-run-remotely
fi
export BAZEL_REMOTE`},
	{"the block fails outright under bash -e", `BAZEL_REMOTE=success
false
export BAZEL_REMOTE`},
}

// The truth-table verifier has to accept E2 and reject each wrong block. These
// controls pass without pr.yml: they are what makes the real pin able to fail.
func TestForkCIGateVerifierAcceptsE2AndRejectsWrongBlocks(t *testing.T) {
	bash := requireHostTool(t, "bash")
	if violations := forkCIGateViolations(bash, forkCIGateSampleE2); len(violations) > 0 {
		t.Errorf("the verifier rejects the block as E2 rules it, so the truth table disagrees with the ruling:\n%s", strings.Join(violations, "\n"))
	}
	for _, mutation := range forkCIGateMutations {
		if violations := forkCIGateViolations(bash, mutation.block); len(violations) == 0 {
			t.Errorf("the verifier accepts a wrong block (%s), so the pin would pass a gate that is not E2:\n%s", mutation.name, mutation.block)
		}
	}
}

// The extraction has to fail loudly, with the re-rule message, on every shape the
// method does not cover, and has to find the block in a real-looking step.
func TestForkCIGateExtractionFailsClosed(t *testing.T) {
	step := func(body string) string {
		return "        run: |\n          export CI_GATE_REQUIRED\n" + body + "          bash .github/scripts/ci-gate.sh\n"
	}
	indent := func(block string) string {
		return "          " + strings.ReplaceAll(block, "\n", "\n          ") + "\n"
	}

	good := step(indent(forkCIGateSampleE2))
	block, err := forkCIGateExtract(good)
	if err != nil {
		t.Fatalf("the extraction refuses a gate step that holds the block once: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(block), forkCIGateBlockStart) || !strings.HasSuffix(strings.TrimSpace(block), forkCIGateBlockEnd) {
		t.Errorf("the extracted block runs from %q to %q, got:\n%s", forkCIGateBlockStart, forkCIGateBlockEnd, block)
	}
	if strings.Contains(block, "ci-gate.sh") || strings.Contains(block, "export CI_GATE_REQUIRED") {
		t.Errorf("the extracted block reaches outside the markers:\n%s", block)
	}

	for _, refused := range []struct{ name, text string }{
		{"no start marker", step("          export BAZEL_REMOTE\n")},
		{"no end marker", step("          BAZEL_REMOTE=success\n")},
		{"neither marker", step("")},
		{"the start marker twice", step(indent(forkCIGateSampleE2) + indent(forkCIGateSampleE2))},
		{"the end marker twice", step(indent(forkCIGateSampleE2) + "          export BAZEL_REMOTE\n")},
		{"the end before the start", step("          export BAZEL_REMOTE\n          BAZEL_REMOTE=success\n")},
		{"an expression inside the block", step(indent(strings.Replace(forkCIGateSampleE2, `"cache"`, `"${{ env.MODE }}"`, 1)))},
	} {
		_, err := forkCIGateExtract(refused.text)
		if err == nil {
			t.Errorf("the extraction accepts %s, and the pin would then test a block it did not find", refused.name)
		} else if !strings.Contains(err.Error(), forkCIGateNotFound) {
			t.Errorf("the extraction refuses %s without saying %q: %v", refused.name, forkCIGateNotFound, err)
		}
	}
}
