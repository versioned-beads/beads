package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins the integration trigger (be-4y7b5s 6.5).
//
// This fork's pull requests target the branch integration, not main: main is a
// byte-identical mirror of upstream. Upstream's pr.yml and pr-risk.yml filter
// their pull_request trigger on branches [main, release/**, hotfix/**], and the
// fork appends integration to that list. Nothing fails when a sync takes
// upstream's file wholesale and the entry goes: the checks simply never appear
// on a pull request to integration, and CI Gate never reports. So the entry is
// pinned, here, at both ends: it passes on the tree as it is and on the tree
// after a sync that re-applied the entry.
//
// The rule is read from the YAML, not from text:
//   - A workflow whose on.pull_request carries a branches list must list
//     integration. A workflow with no such list (docs-mintlify.yml) runs on a
//     pull request to any branch and is skipped.
//   - A fork-owned workflow (a file name that starts "fork-") lists exactly
//     [integration]: it exists for this fork and runs on nothing else.
//     workflow_dispatch beside it is fine, and is not looked at.
//   - pull_request_target is out of scope: bazel-farm.yml and
//     update-vendor-hash.yml filter ['main'] in the fork and upstream alike, and
//     the fork never extended them.
//   - The sweep fails if it visits no workflow, or does not visit pr.yml. It
//     does not require pr-risk.yml: if upstream drops that filter, nothing needs
//     re-adding.
//
// A new upstream pull_request workflow with a branch filter fails this on the
// next sync, and the answer is the same one-entry edit (append integration).

const forkIntegrationBranch = "integration"

// forkIntegrationResult is what one sweep found.
type forkIntegrationResult struct {
	visited  []string // workflows whose on.pull_request carries a branches list
	problems []string
}

// forkIntegrationGet: the value under key in the mapping m, aliases followed,
// or nil. merged says the mapping also takes keys from a YAML merge key (<<),
// which this pin does not expand.
func forkIntegrationGet(m *yaml.Node, key string) (val *yaml.Node, merged bool) {
	for m != nil && m.Kind == yaml.AliasNode {
		m = m.Alias
	}
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		switch m.Content[i].Value {
		case "<<":
			merged = true
		case key:
			val = m.Content[i+1]
			for val.Kind == yaml.AliasNode {
				val = val.Alias
			}
		}
	}
	return val, merged
}

// forkIntegrationBranches reads on.pull_request.branches of one workflow.
// carries is false when the workflow has no such list, which is not an error.
// A branches value that is a name rather than a list is a list of one.
func forkIntegrationBranches(src []byte) (list []string, carries bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, false, fmt.Errorf("does not parse as YAML (%v)", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("is not a workflow: it has no top-level mapping")
	}
	node := doc.Content[0]
	for _, key := range []string{"on", "pull_request", "branches"} {
		next, merged := forkIntegrationGet(node, key)
		if merged {
			return nil, false, fmt.Errorf("builds %q with a YAML merge key (<<), which this pin does not expand; extend this test or ask the architect", key)
		}
		if next == nil {
			return nil, false, nil
		}
		node = next
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{node.Value}, true, nil
	case yaml.SequenceNode:
		for _, c := range node.Content {
			for c.Kind == yaml.AliasNode {
				c = c.Alias
			}
			if c.Kind != yaml.ScalarNode {
				return nil, false, fmt.Errorf("has an on.pull_request.branches entry that is not a branch name; extend this test or ask the architect")
			}
			list = append(list, c.Value)
		}
		return list, true, nil
	}
	return nil, false, fmt.Errorf("has an on.pull_request.branches that is neither a list nor a branch name; extend this test or ask the architect")
}

// forkIntegrationScan sweeps workflows, a map from the name each is reported
// under to its source.
func forkIntegrationScan(files map[string][]byte) forkIntegrationResult {
	var res forkIntegrationResult
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		list, carries, err := forkIntegrationBranches(files[name])
		if err != nil {
			res.problems = append(res.problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !carries {
			continue
		}
		res.visited = append(res.visited, name)
		owned := strings.HasPrefix(filepath.Base(name), "fork-")
		switch {
		case owned && !slices.Equal(list, []string{forkIntegrationBranch}):
			res.problems = append(res.problems, fmt.Sprintf("%s: a fork-owned workflow runs on pull requests to %s and nothing else, but its on.pull_request.branches is %v; make it exactly [%s] (be-4y7b5s 6.5)", name, forkIntegrationBranch, list, forkIntegrationBranch))
		case !owned && !slices.Contains(list, forkIntegrationBranch):
			res.problems = append(res.problems, fmt.Sprintf("%s: on.pull_request.branches is %v, so a pull request to %s never starts it and CI Gate never reports; append %s to its on.pull_request.branches (be-4y7b5s 6.5)", name, list, forkIntegrationBranch, forkIntegrationBranch))
		}
	}
	switch {
	case len(res.visited) == 0:
		res.problems = append(res.problems, "the sweep visited no workflow with an on.pull_request.branches list: the workflows moved, or this pin no longer reads their shape; ask the architect (be-4y7b5s 6.5)")
	case !slices.ContainsFunc(res.visited, func(name string) bool { return filepath.Base(name) == "pr.yml" }):
		res.problems = append(res.problems, fmt.Sprintf("the sweep did not visit pr.yml (visited %v): upstream's pr.yml lost its on.pull_request.branches list, or the file moved; ask the architect (be-4y7b5s 6.5)", res.visited))
	}
	return res
}

// Every workflow in the repository, so a pull_request workflow upstream adds
// later is swept too. What it visited is logged (go test -v) as the evidence
// that the sweep looked.
func TestForkPullRequestWorkflowsTriggerOnIntegration(t *testing.T) {
	root := sourceRepoRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no workflows under %s: %v", root, err)
	}
	files := map[string][]byte{}
	for _, path := range paths {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.ToSlash(rel)] = src
	}
	res := forkIntegrationScan(files)
	for _, p := range res.problems {
		t.Error(p)
	}
	t.Logf("%d of %d workflows carry an on.pull_request.branches list: %s", len(res.visited), len(files), strings.Join(res.visited, " "))
}

// Known-positive and known-negative controls on synthetic workflows, in both
// YAML list styles, so the sweep is not passing because it read nothing.
func TestForkIntegrationTriggerScanControls(t *testing.T) {
	const jobs = "jobs:\n  a:\n    runs-on: ubuntu-latest\n"
	pr := func(branches string) string { return "on:\n  pull_request:\n" + branches + jobs }
	flow := func(list string) string { return pr("    branches: " + list + "\n") }
	block := func(entries ...string) string {
		return pr("    branches:\n      - " + strings.Join(entries, "\n      - ") + "\n")
	}
	const prFile = ".github/workflows/pr.yml"

	type scanCase struct {
		name     string
		file     string // under .github/workflows; x.yml when empty
		src      string
		visited  bool
		problems int
	}
	cases := []scanCase{
		// An upstream-owned workflow lists integration.
		{name: "flow list with integration, as upstream's pr.yml and the fork write it", file: "pr.yml", src: flow("[ main, 'release/**', 'hotfix/**', integration ]"), visited: true},
		{name: "block list with integration", file: "pr.yml", src: block("main", "'release/**'", "integration"), visited: true},
		{name: "quoted integration", file: "pr.yml", src: flow("['main', 'integration']"), visited: true},
		{name: "integration first", file: "pr.yml", src: flow("[integration, main]"), visited: true},
		{name: "a name rather than a list", file: "pr.yml", src: pr("    branches: integration\n"), visited: true},
		{name: "list through an alias", file: "pr.yml", src: "x-branches: &b [main, integration]\non:\n  pull_request:\n    branches: *b\n" + jobs, visited: true},
		{name: "workflow_dispatch and a push filter beside the pull_request one", file: "pr.yml", src: "on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n  pull_request:\n    branches: [main, integration]\n" + jobs, visited: true},
		{name: "flow list without integration", file: "pr.yml", src: flow("[ main, 'release/**', 'hotfix/**' ]"), visited: true, problems: 1},
		{name: "block list without integration", file: "pr.yml", src: block("main", "'release/**'", "'hotfix/**'"), visited: true, problems: 1},
		{name: "an empty list", file: "pr.yml", src: flow("[]"), visited: true, problems: 1},
		{name: "a name that is not integration", file: "pr.yml", src: pr("    branches: main\n"), visited: true, problems: 1},
		{name: "integration spelled with a capital", file: "pr.yml", src: flow("[main, Integration]"), visited: true, problems: 1},
		{name: "integration only as a pattern that happens to match", file: "pr.yml", src: flow("['**']"), visited: true, problems: 1},
		{name: "integration only in branches-ignore", file: "pr.yml", src: pr("    branches: [main]\n    branches-ignore: [integration]\n"), visited: true, problems: 1},
		{name: "integration only under push", file: "pr.yml", src: "on:\n  push:\n    branches: [main, integration]\n  pull_request:\n    branches: [main]\n" + jobs, visited: true, problems: 1},
		{name: "integration only under pull_request_target", file: "pr.yml", src: "on:\n  pull_request_target:\n    branches: [main, integration]\n  pull_request:\n    branches: [main]\n" + jobs, visited: true, problems: 1},
		{name: "a list entry that is not a name", src: pr("    branches:\n      - [main]\n"), problems: 1},
		{name: "branches that is a mapping", src: pr("    branches:\n      main: true\n"), problems: 1},
		{name: "on built with a merge key", src: "x: &t\n  pull_request:\n    branches: [main, integration]\non:\n  <<: *t\n" + jobs, problems: 1},
		{name: "a workflow that does not parse", src: "on: [\njobs: {\n", problems: 1},
		{name: "a file that is not a mapping", src: "- on\n- jobs\n", problems: 1},
		{name: "a new upstream workflow with a branch filter and no integration", src: flow("[main]"), visited: true, problems: 1},
		{name: "a new upstream workflow with a branch filter and integration", src: flow("[main, integration]"), visited: true},

		// Skipped: nothing to pin.
		{name: "pull_request with no branches list (docs-mintlify.yml)", src: pr("    paths: ['docs/**']\n")},
		{name: "pull_request with no body", src: "on:\n  pull_request:\n" + jobs},
		{name: "on as a list of event names", src: "on: [pull_request, push]\n" + jobs},
		{name: "on as an event name", src: "on: pull_request\n" + jobs},
		{name: "pull_request_target with a branches list that lacks integration", src: "on:\n  pull_request_target:\n    branches: ['main']\n" + jobs},
		{name: "push and merge_group only", src: "on:\n  push:\n    branches: [main]\n  merge_group:\n" + jobs},
		{name: "workflow_call only", src: "on:\n  workflow_call:\n" + jobs},

		// A fork-owned workflow lists exactly integration.
		{name: "fork-owned: [integration] with workflow_dispatch beside it (fork-macos.yml)", file: "fork-x.yml", src: "on:\n  pull_request:\n    branches: [ integration ]\n  workflow_dispatch:\n" + jobs, visited: true},
		{name: "fork-owned: block list [integration]", file: "fork-x.yml", src: block("integration"), visited: true},
		{name: "fork-owned: integration and main", file: "fork-x.yml", src: flow("[main, integration]"), visited: true, problems: 1},
		{name: "fork-owned: integration twice", file: "fork-x.yml", src: flow("[integration, integration]"), visited: true, problems: 1},
		{name: "fork-owned: main only", file: "fork-x.yml", src: flow("[main]"), visited: true, problems: 1},
		{name: "fork-owned: a release pattern beside integration", file: "fork-x.yml", src: block("integration", "'release/**'"), visited: true, problems: 1},
		{name: "fork-owned: dispatch only", file: "fork-x.yml", src: "on:\n  workflow_dispatch:\n" + jobs},
	}
	for _, c := range cases {
		file := ".github/workflows/" + c.file
		if c.file == "" {
			file = ".github/workflows/x.yml"
		}
		// pr.yml stands in the sweep, so that a case does not also trip the rule
		// that pr.yml must be visited; the case under test is the other file.
		files := map[string][]byte{file: []byte(c.src)}
		if file != prFile {
			files[prFile] = []byte(flow("[main, integration]"))
		}
		res := forkIntegrationScan(files)
		visited := slices.Contains(res.visited, file)
		if visited != c.visited || len(res.problems) != c.problems {
			t.Errorf("%s: visited %v with %d problems %q, want %v and %d", c.name, visited, len(res.problems), res.problems, c.visited, c.problems)
		}
	}

	// Both ends of the pin's own rules.
	good := []byte(flow("[main, integration]"))
	if res := forkIntegrationScan(map[string][]byte{}); len(res.problems) != 1 || len(res.visited) != 0 {
		t.Errorf("a sweep of no workflows reported %q, want one problem: it visited nothing", res.problems)
	}
	if res := forkIntegrationScan(map[string][]byte{".github/workflows/docs.yml": []byte(pr("    paths: ['x']\n"))}); len(res.problems) != 1 {
		t.Errorf("a sweep that skipped every workflow reported %q, want one problem: it visited nothing", res.problems)
	}
	if res := forkIntegrationScan(map[string][]byte{".github/workflows/conformance.yml": good}); len(res.problems) != 1 || !strings.Contains(res.problems[0], "pr.yml") {
		t.Errorf("a sweep that did not visit pr.yml reported %q, want one problem naming pr.yml", res.problems)
	}
	if res := forkIntegrationScan(map[string][]byte{prFile: good}); len(res.problems) != 0 {
		t.Errorf("a sweep that visited pr.yml alone reported %q, want none: pr-risk.yml is not required", res.problems)
	}
	if res := forkIntegrationScan(map[string][]byte{prFile: good, ".github/workflows/pr-risk.yml": good}); len(res.problems) != 0 || len(res.visited) != 2 {
		t.Errorf("pr.yml and pr-risk.yml both with integration reported %q, visited %v, want no problems and both visited", res.problems, res.visited)
	}

	// A failure names the workflow and carries the fix (be-4y7b5s 6.5).
	res := forkIntegrationScan(map[string][]byte{prFile: []byte(flow("[main]")), ".github/workflows/pr-risk.yml": good})
	if len(res.problems) != 1 || !strings.HasPrefix(res.problems[0], prFile+": ") || !strings.Contains(res.problems[0], "append integration to its on.pull_request.branches (be-4y7b5s 6.5)") {
		t.Errorf("a pr.yml without integration reported %q, want one problem that names .github/workflows/pr.yml and says to append integration to its on.pull_request.branches (be-4y7b5s 6.5)", res.problems)
	}
}
