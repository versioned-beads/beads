package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins the fork's own proxied shard split, scripts/ci/fork-proxied-cmd-shards.txt,
// which the test-proxied-cmd job of fork-heavy-tiers.yml hands to
// .github/scripts/proxied-test-shard.sh through BEADS_TEST_SHARD_MANIFEST (the
// sixth edit of that workflow's lift, pinned in fork_heavy_tiers_pin_test.go;
// ruling be-jvluf7 sections 3 and 4). Upstream's own manifest keeps only its
// Bazel lane's block after the sync, so the fork's 15-shard split lives in a
// file the fork owns and a sync never merges fork shard lines into an
// upstream-owned file.
//
//   - R8 (be-sudqo0, moved here by be-jvluf7 section 4.2), the split's coverage:
//     TestForkLegacyProxiedBlockCoversEveryTest. The 15-shard block lists every
//     discovered TestProxiedServer* and TestServerMode* test exactly once. The
//     script places a test the block does not list by hash(name) % 15, which is
//     how a sync's new tests landed unbalanced on shard 5.
//   - The override, TestForkProxiedShardScriptReadsTheManifestOverride: the
//     shard script still reads the manifest from BEADS_TEST_SHARD_MANIFEST. If
//     a sync dropped that, the sixth edit would stay in the workflow and do
//     nothing, the lane would hash-distribute every test, and no other test
//     would go red; this pin goes red first.
//
// R9, the lane's three timeouts, stays in fork_proxied_lane_pin_test.go. The
// two files differ in their Bazel target on purpose: this one is in
// go_test_sources_test (the generator reads the cmd/bd sources, which only that
// target carries) and R9 is in scripts_test, so neither file may use an
// identifier the other defines, and each defines its own names here.
//
// Both pins read lines a sync takes from upstream or a file only the fork
// edits, so each fails in the sync pull request, names what to put back, and
// leaves the fix to a person. The lane is temporary: when it retires with
// fork-heavy-tiers.yml, delete this file with its entry in go_test_sources_test
// and scripts/ci/fork-proxied-cmd-shards.txt with its data entry there
// (be-jvluf7 section 11.2).

const (
	forkProxiedManifestFile   = "scripts/ci/fork-proxied-cmd-shards.txt"
	forkProxiedManifestScript = ".github/scripts/proxied-test-shard.sh"
	forkProxiedManifestEnv    = "BEADS_TEST_SHARD_MANIFEST"

	// The lane's total: the shard count of the matrix in fork-heavy-tiers.yml
	// and the block of the manifest this file checks.
	forkProxiedManifestShards = 15
)

// The generator's check of the fork's 15-shard split. The command is the one the
// header of the manifest gives; its --manifest is what points it away from
// upstream's file.
func forkProxiedManifestCheck(root, python string) *exec.Cmd {
	cmd := exec.Command(python, "scripts/ci/gen_proxied_shard_manifest.py",
		strconv.Itoa(forkProxiedManifestShards), "--manifest", forkProxiedManifestFile, "--check")
	cmd.Dir = root
	return cmd
}

// R8. The fork's 15-block lists every discovered TestProxiedServer* and
// TestServerMode* test exactly once. The generator's --check gates on name
// coverage alone (it never compares shard assignments), which is the whole of
// the rule: the block is topped up, never repacked.
//
// Upstream's TestProxiedShardManifestGeneratorNotStale checks only the Bazel
// lane's 34-block of upstream's own manifest and never reads this file, so
// without this test a new proxied test could be missing from the fork's split
// and nothing would say so. R8 tops the fork's block up, so the same check
// passes here and this test holds it.
func TestForkLegacyProxiedBlockCoversEveryTest(t *testing.T) {
	python := requireHostTool(t, "python3")
	out, err := forkProxiedManifestCheck(sourceRepoRoot(t), python).CombinedOutput()
	if err == nil {
		return
	}
	t.Errorf("the 15-shard block of %s does not list every discovered TestProxiedServer*/TestServerMode* test exactly once (R8, be-sudqo0; the fork's own manifest since be-jvluf7): %v\n%s\n"+
		"Fix: run `python3 scripts/ci/gen_proxied_shard_manifest.py 15 --weights=inits --write --manifest %s`, the plain incremental top-up (it keeps every existing line and places only the new names), and commit the result. "+
		"The `Run:` line the generator prints above leaves out --manifest, so following it would write upstream's manifest instead of this one; add --manifest %s to it and keep --weights=inits. "+
		"Never pass --repack: the 15-block is not repacked. "+
		"If the top-up puts a new heavy test on a shard that recent job times show to be busy, relocate that one new line by hand, never a line that already existed, and say so in the pull request.",
		forkProxiedManifestFile, err, out, forkProxiedManifestFile, forkProxiedManifestFile)
}

// The one line of the shard script that reads the manifest path: the override
// variable with a default.
var forkProxiedManifestOverrideRe = regexp.MustCompile(`(?m)^MANIFEST="\$\{` + forkProxiedManifestEnv + `:-([^}"\s]+)\}"[ \t]*$`)

// The override. proxied-test-shard.sh reads its manifest from MANIFEST="${BEADS_TEST_SHARD_MANIFEST:-<default>}",
// and the sixth edit of fork-heavy-tiers.yml sets the variable to the fork's split. If
// upstream drops the override, the edit stays in the workflow and does nothing, the lane
// silently hash-distributes every test, and no other test goes red. So this holds the
// line, and then holds what the line is for: a manifest handed over the variable decides
// the shard, which only a run of the script can show.
func TestForkProxiedShardScriptReadsTheManifestOverride(t *testing.T) {
	root := sourceRepoRoot(t)
	script := readPolicyFile(t, root, forkProxiedManifestScript)
	if found := forkProxiedManifestOverrideRe.FindAllStringSubmatch(script, -1); len(found) != 1 {
		t.Errorf("%s: found %d lines of the form MANIFEST=\"${%s:-<default>}\", want exactly 1; the fork's lane hands its own split to the script through that variable (sixth edit of fork-heavy-tiers.yml), so without it the lane hash-distributes every test. This needs a re-rule, not a patch here: take it to the architect (be-jvluf7 section 3)",
			forkProxiedManifestScript, len(found), forkProxiedManifestEnv)
		return
	}

	// A one-entry manifest: the first test of the fork's split, moved to the next
	// shard. A script that honors the override reports that entry on the new shard
	// ("manifest: 1"); one that reads its own default reports none, because the
	// default holds no 15-block (it is upstream's), and places the test by hash.
	var name string
	var shard int
	for _, line := range strings.Split(readPolicyFile(t, root, forkProxiedManifestFile), "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != strconv.Itoa(forkProxiedManifestShards) {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("%s: shard %q of %s is not a number: %v", forkProxiedManifestFile, fields[1], fields[2], err)
		}
		name, shard = fields[2], n%forkProxiedManifestShards+1
		break
	}
	if name == "" {
		t.Fatalf("%s has no %d-shard entry to build the probe manifest from", forkProxiedManifestFile, forkProxiedManifestShards)
	}
	probe := filepath.Join(t.TempDir(), "probe-shards.txt")
	entry := strconv.Itoa(forkProxiedManifestShards) + " " + strconv.Itoa(shard) + " " + name + "\n"
	if err := os.WriteFile(probe, []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	bash := requireHostTool(t, "bash")

	// The shard script uses bash 4 associative arrays (declare -A), and macOS
	// ships bash 3.2. Probe the bash it is about to run with, so the dynamic
	// half below runs wherever the script can run and is skipped (failed
	// under Bazel) where its prerequisite is missing.
	if err := exec.Command(bash, "-c", "declare -A probe=()").Run(); err != nil {
		skipOrFailWithoutHostTool(t, "bash lacks associative arrays (bash >= 4 required by %s)", forkProxiedManifestScript)
	}

	cmd := exec.Command(bash, forkProxiedManifestScript, strconv.Itoa(shard), strconv.Itoa(forkProxiedManifestShards))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "BEADS_TEST_SHARD_LIST_ONLY=1", forkProxiedManifestEnv+"="+probe)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %d %d with %s=%s: %v\n%s", forkProxiedManifestScript, shard, forkProxiedManifestShards, forkProxiedManifestEnv, probe, err, out)
	}
	listing := string(out)
	if !regexp.MustCompile(`(?m)^  manifest: 1, fallback: \d+$`).MatchString(listing) || !strings.Contains(listing, "\n  "+name+"\n") {
		t.Errorf("%s %d %d with %s=%s (a manifest holding the single entry %q) did not place %s on shard %d from it:\n%s\n"+
			"The script no longer takes its manifest from %s, so the fork's own split is not what the lane runs. This needs a re-rule, not a patch here: take it to the architect (be-jvluf7 section 3)",
			forkProxiedManifestScript, shard, forkProxiedManifestShards, forkProxiedManifestEnv, probe, strings.TrimSpace(entry), name, shard, listing, forkProxiedManifestEnv)
	}
}
