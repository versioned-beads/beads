package scripts_test

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It pins rule R6 (v4, be-4y7b5s 5.1 and 5.4): no workflow job a pull
// request in this repository can schedule may ask for a Blacksmith runner,
// because this repository has no pool and such a job queues forever.
//
// Upstream puts blacksmith-<N>vcpu-<os> labels on `runs-on` in three ways.
// (1) Behind needs.rbe.outputs.mode == 'remote' (bazel.yml's lanes): that needs
// the repository variable RBE_WEST_WORKERS and the secret RBE_WEST_EXECUTOR,
// which this repository does not have, and rbe's fork modes (fork-ro, fork-rw)
// stay GitHub-hosted. (2) Behind a same-repository pull_request / merge_group
// predicate: every pull request inside this repository is same-repository, so
// it is true for all of them. (3) Bare, as a literal or as the cells of a
// matrix (#7355's Windows and macOS jobs). The fork's edit to (2) and (3) is
// one conjunct, github.repository_owner == 'gastownhall', so anywhere else the
// expression is a GitHub-hosted label, and a missing context field fails
// towards the hosted runner. E1''-v4 writes the edit; this file proves it.
//
// The sweep is mechanical. It reads every .github/workflows/*.yml, evaluates
// every job's runs-on (a scalar, a list's entries, a mapping's labels) with
// upstream's own evaluator (ci_blacksmith_runner_test.go), and evaluates first
// each matrix cell that runs-on reads when the cell is itself an expression.
// No workflow and no job is named here, so a site upstream adds later is swept
// too.
//
//   - In the fork's world (any owner but gastownhall or none, every event, head
//     repository and actor, and every rbe mode this repository can reach: none
//     because rbe was skipped, skip, cache, local, fork-ro, fork-rw) no runs-on
//     may resolve to a Blacksmith label, and no matrix cell may spell one
//     unless runs-on reads it and the evaluation above clears it.
//   - In gastownhall's world (the same, plus rbe mode remote) a runs-on that
//     spells a Blacksmith label must still resolve to one: a guard must not
//     disable upstream's own pool (a typo in the owner, an inverted operator, a
//     mode that is never remote).
//   - A composite action cannot choose a runner, so label text in one fails.
//   - Fork-owned workflows (fork-*.yml) name no Blacksmith at all, in a comment
//     either, and resolve to GitHub-hosted labels only (rule R6 v4 rule 4).
//   - A workflow whose only trigger is a push to main never runs from this
//     tree: main is a byte-identical upstream mirror and runs its own copy. Its
//     sites are counted as exempt, not evaluated.
//
// What it cannot read it fails on, never passes. The evaluator reads an
// identifier the context does not mention as null, which can only make a
// comparison false, so a scan that forgets to supply something an expression
// reads can only err towards "never reaches Blacksmith": it passes, silently.
// It did, on F7b's include-matrix marker, whose key an earlier scan never read.
// So an expression the evaluator cannot evaluate, an identifier the scan does
// not supply, a matrix it cannot expand, a YAML merge key and a sweep that
// visits no site are all failures: extend this file or ask the architect, do
// not widen it silently.

const forkBlacksmithGuard = "github.repository_owner == 'gastownhall' && "

var (
	// forkBlacksmithName: a Blacksmith runner label spelled in text. Prose that
	// only says "Blacksmith" (a comment in a run script) does not match.
	forkBlacksmithName = regexp.MustCompile(`(?i)\bblacksmith-[0-9]+vcpu`)
	// forkBlacksmithAny: a runner label, resolved, that names Blacksmith at all.
	forkBlacksmithAny = regexp.MustCompile(`(?i)blacksmith`)
	// forkHostedLabel: a GitHub-hosted runner, which is all a fork-owned
	// workflow may resolve to.
	forkHostedLabel = regexp.MustCompile(`^(ubuntu|windows|macos)-`)
)

// forkModelledIdents: the identifiers forkRunnerContexts supplies. A runs-on or
// a matrix cell that reads any other identifier (and is not a matrix key of its
// own job) cannot be scanned: the evaluator would read it as null and the scan
// would pass without having looked.
var forkModelledIdents = map[string]bool{
	"github.event_name":                             true,
	"github.event.pull_request.head.repo.full_name": true,
	"github.repository":                             true,
	"github.repository_owner":                       true,
	"github.actor":                                  true,
	"needs.rbe.outputs.mode":                        true,
	"needs.rbe.outputs.enabled":                     true,
}

var forkRunnerEvents = []string{"pull_request", "pull_request_target", "merge_group", "push", "workflow_dispatch", "schedule", "workflow_call"}

// forkRunnerContexts: every context a runs-on expression can see when the
// repository's owner is one of owners ("" is no owner at all, the key left out)
// and its name is repo. Every event shape, head repository and actor is tried,
// realistic or not, because one that reaches a Blacksmith label is a violation
// whatever the combination; mode is each value rbe's decision step can leave in
// needs.rbe.outputs.mode, "" for the job that was skipped, with enabled
// derived from it the way that step derives it.
func forkRunnerContexts(owners []string, repo string, modes []string) []map[string]string {
	ctxs := []map[string]string{}
	for _, owner := range owners {
		for _, event := range forkRunnerEvents {
			for _, head := range []string{repo, "other/beads", ""} {
				for _, actor := range []string{"alice", "dependabot[bot]"} {
					for _, mode := range modes {
						ctx := map[string]string{
							"github.event_name":                             event,
							"github.event.pull_request.head.repo.full_name": head,
							"github.repository":                             repo,
							"github.actor":                                  actor,
							"needs.rbe.outputs.mode":                        mode,
							"needs.rbe.outputs.enabled":                     forkRunnerEnabled(mode),
						}
						if owner != "" {
							ctx["github.repository_owner"] = owner
						}
						ctxs = append(ctxs, ctx)
					}
				}
			}
		}
	}
	return ctxs
}

// forkRunnerEnabled: rbe's `enabled` output for a mode. It is true for the
// modes that use remote execution (remote and the two fork modes), false for
// the others, and empty when the job never ran.
func forkRunnerEnabled(mode string) string {
	switch {
	case mode == "":
		return ""
	case mode == "remote" || strings.HasPrefix(mode, "fork-"):
		return "true"
	}
	return "false"
}

var (
	// forkRunnerForkWorld: this repository, or a fork of it. Mode remote needs
	// RBE_WEST_WORKERS and RBE_WEST_EXECUTOR, which neither has.
	forkRunnerForkWorld = forkRunnerContexts([]string{"versioned-beads", "someone-else", ""}, "versioned-beads/beads",
		[]string{"", "skip", "cache", "local", "fork-ro", "fork-rw"})
	// forkRunnerUpstreamWorld: gastownhall/beads, where upstream's pool is.
	forkRunnerUpstreamWorld = forkRunnerContexts([]string{"gastownhall"}, "gastownhall/beads",
		[]string{"", "skip", "cache", "local", "fork-ro", "fork-rw", "remote"})
)

// forkExprTokens: expr, `${{ }}` included or not, as the evaluator reads it.
func forkExprTokens(expr string) ([]ghToken, error) {
	body := strings.TrimSpace(expr)
	body = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(body, "${{"), "}}"))
	return ghTokenize(body)
}

// forkExprIdents: the identifiers expr reads, sorted and unique. true, false
// and null are literals to the evaluator, not identifiers.
func forkExprIdents(expr string) ([]string, error) {
	toks, err := forkExprTokens(expr)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, tok := range toks {
		if tok.kind != "ident" || tok.val == "true" || tok.val == "false" || tok.val == "null" || seen[tok.val] {
			continue
		}
		seen[tok.val] = true
		out = append(out, tok.val)
	}
	sort.Strings(out)
	return out, nil
}

// forkRunnerWant adds label to want if it is a full Blacksmith runner label and
// want does not hold it yet. A word that only says "blacksmith" (a marker a
// matrix compares against) is not a label, so nothing is expected to select it.
func forkRunnerWant(want []string, label string) []string {
	label = strings.TrimSpace(label)
	if !forkBlacksmithName.MatchString(label) || slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(w, label) }) {
		return want
	}
	return append(want, label)
}

// forkExprLabels: the Blacksmith runner labels expr can select, in order and
// unique: the string literals it spells, except an operand of == or !=, which
// is compared with something and chosen by nothing.
func forkExprLabels(expr string) ([]string, error) {
	toks, err := forkExprTokens(expr)
	if err != nil {
		return nil, err
	}
	compares := func(i int) bool {
		return i >= 0 && i < len(toks) && toks[i].kind == "op" && (toks[i].val == "==" || toks[i].val == "!=")
	}
	var out []string
	for i, tok := range toks {
		if tok.kind == "string" && !compares(i-1) && !compares(i+1) {
			out = forkRunnerWant(out, tok.val)
		}
	}
	return out, nil
}

// forkRunnerPureExpr: s is one ${{ }} expression and nothing else around it.
func forkRunnerPureExpr(s string) bool {
	return strings.HasPrefix(s, "${{") && strings.HasSuffix(s, "}}") && strings.Count(s, "${{") == 1 && strings.Count(s, "}}") == 1
}

// forkYAMLValue follows aliases to the node they stand for.
func forkYAMLValue(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// forkYAMLGet: the value under key in the mapping m, or nil.
func forkYAMLGet(m *yaml.Node, key string) *yaml.Node {
	m = forkYAMLValue(m)
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return forkYAMLValue(m.Content[i+1])
		}
	}
	return nil
}

// forkMirrorOnly: the workflow's only trigger is a push to main.
func forkMirrorOnly(root *yaml.Node) bool {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "on" {
			continue
		}
		var on map[string]any
		if root.Content[i+1].Decode(&on) != nil {
			return false
		}
		return reflect.DeepEqual(on, map[string]any{"push": map[string]any{"branches": []any{"main"}}})
	}
	return false
}

// forkRunnerCell is one value a matrix key can take; include says it came from
// an include entry, so another combination may leave the key unset.
type forkRunnerCell struct {
	node    *yaml.Node
	include bool
}

// forkRunnerMatrix: a job's strategy.matrix, as far as it can be read. keys
// holds every cell each key can take, flattened: the cross product of those is
// a superset of what GitHub builds, which is the safe side for asking what a
// job can resolve to. base and entries keep the matrix's structure, for asking
// what each combination GitHub really builds resolves to (forkRunnerCombos).
// dynamic says part of it is an expression (fromJSON) the scan cannot expand.
type forkRunnerMatrix struct {
	keys    map[string][]forkRunnerCell
	base    map[string][]*yaml.Node // each base key's cells
	entries []map[string]*yaml.Node // each include entry's scalar values
	dynamic bool
}

// forkRunnerScalarCells: the cells val gives its key: itself if it is a scalar,
// a list's scalar entries. A mapping or a list as a cell feeds matrix.key.sub,
// a name the scan reports as never given a value if runs-on reads it.
func forkRunnerScalarCells(val *yaml.Node) []*yaml.Node {
	var out []*yaml.Node
	switch val.Kind {
	case yaml.ScalarNode:
		out = append(out, val)
	case yaml.SequenceNode:
		for _, c := range val.Content {
			if c = forkYAMLValue(c); c.Kind == yaml.ScalarNode {
				out = append(out, c)
			}
		}
	}
	return out
}

func (m *forkRunnerMatrix) add(key string, val *yaml.Node, include bool) {
	for _, c := range forkRunnerScalarCells(val) {
		m.keys[key] = append(m.keys[key], forkRunnerCell{c, include})
	}
}

// forkRunnerReadMatrix reads a job's matrix. An exclude entry feeds nothing:
// leaving a combination out only widens the combinations tried.
func forkRunnerReadMatrix(job *yaml.Node) forkRunnerMatrix {
	m := forkRunnerMatrix{keys: map[string][]forkRunnerCell{}, base: map[string][]*yaml.Node{}}
	node := forkYAMLGet(forkYAMLGet(job, "strategy"), "matrix")
	if node == nil {
		return m
	}
	if node.Kind != yaml.MappingNode {
		m.dynamic = true // matrix: ${{ fromJSON(...) }}
		return m
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, forkYAMLValue(node.Content[i+1])
		switch key {
		case "exclude":
		case "include":
			if val.Kind != yaml.SequenceNode {
				m.dynamic = true // include: ${{ fromJSON(...) }}
				continue
			}
			for _, entry := range val.Content {
				entry = forkYAMLValue(entry)
				if entry.Kind != yaml.MappingNode {
					m.dynamic = true
					continue
				}
				scalars := map[string]*yaml.Node{}
				for j := 0; j+1 < len(entry.Content); j += 2 {
					k, v := entry.Content[j].Value, forkYAMLValue(entry.Content[j+1])
					m.add(k, v, true)
					if v.Kind == yaml.ScalarNode {
						scalars[k] = v
					}
				}
				m.entries = append(m.entries, scalars)
			}
		default:
			m.add(key, val, false)
			if cells := forkRunnerScalarCells(val); len(cells) > 0 {
				m.base[key] = cells
			}
		}
	}
	return m
}

// forkRunsOnScalars: the scalars a job's runs-on is made of: the value itself,
// a list's entries, a mapping's labels. problems lists the shapes the scan
// cannot read. A runner group is not a label, so a group's name is left to the
// scan for label text outside runs-on.
func forkRunsOnScalars(n *yaml.Node) (out []*yaml.Node, problems []string) {
	if n == nil {
		return nil, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, []string{"is empty"}
		}
		return []*yaml.Node{n}, nil
	case yaml.SequenceNode:
		for _, e := range n.Content {
			e = forkYAMLValue(e)
			if e.Kind != yaml.ScalarNode {
				problems = append(problems, "is a list with an entry that is not a label")
				continue
			}
			out = append(out, e)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			switch key := n.Content[i].Value; key {
			case "group":
			case "labels":
				s, p := forkRunsOnScalars(forkYAMLValue(n.Content[i+1]))
				out, problems = append(out, s...), append(problems, p...)
			default:
				problems = append(problems, fmt.Sprintf("is a mapping with the key %q, which this scan does not read; extend this test or ask the architect", key))
			}
		}
	default:
		problems = append(problems, "has a shape this scan does not read; extend this test or ask the architect")
	}
	return out, problems
}

// forkRunnerBad: label is one the workflow must never resolve to. For a
// fork-owned workflow that is anything but a GitHub-hosted runner; for the rest
// it is a Blacksmith label.
func forkRunnerBad(label string, owned bool) bool {
	if owned {
		return !forkHostedLabel.MatchString(label)
	}
	return forkBlacksmithAny.MatchString(label)
}

// forkRunnerDescribe: label, and the context that produced it.
func forkRunnerDescribe(label string, ctx map[string]string) string {
	s := fmt.Sprintf("%q with owner %q, event %s, head %q, rbe mode %q", label, ctx["github.repository_owner"], ctx["github.event_name"], ctx["github.event.pull_request.head.repo.full_name"], ctx["needs.rbe.outputs.mode"])
	var keys []string
	for k := range ctx {
		if strings.HasPrefix(k, "matrix.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		s += fmt.Sprintf(", %s %q", k, ctx[k])
	}
	return s
}

// forkRunnerCellValues: every value the cells can give their key under ctx. A
// cell that is an expression is evaluated here, so the post-sync matrix form
// (the guard in the cells, runs-on: ${{ matrix.runner }} untouched) is read for
// what it resolves to, not matched as text.
func forkRunnerCellValues(cells []forkRunnerCell, ctx map[string]string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, c := range cells {
		text := strings.TrimSpace(c.node.Value)
		if c.include {
			add("") // a combination no include entry reaches leaves the key unset
		}
		if !strings.Contains(text, "${{") {
			add(text)
			continue
		}
		if !forkRunnerPureExpr(text) {
			return nil, fmt.Errorf("matrix cell %q mixes text with an expression", text)
		}
		v, err := evalGHExpr(text, ctx)
		if err != nil {
			return nil, fmt.Errorf("matrix cell %q: %w", text, err)
		}
		switch v := v.(type) {
		case string:
			add(v)
		case bool:
			add(strconv.FormatBool(v))
		case nil:
			add("")
		default:
			return nil, fmt.Errorf("matrix cell %q evaluates to %#v", text, v)
		}
	}
	return out, nil
}

// forkRunnerEach calls fn with ctx extended by every combination of vals (one
// list per key); fn returns true to stop. The context it is handed is reused.
func forkRunnerEach(ctx map[string]string, keys []string, vals [][]string, fn func(map[string]string) bool) {
	if len(keys) == 0 {
		fn(ctx)
		return
	}
	c := maps.Clone(ctx)
	idx := make([]int, len(keys))
	for {
		for i, key := range keys {
			c["matrix."+key] = vals[i][idx[i]]
		}
		if fn(c) {
			return
		}
		i := len(keys) - 1
		for ; i >= 0; i-- {
			if idx[i]++; idx[i] < len(vals[i]) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			return
		}
	}
}

// forkRunnerFirst evaluates expr in every context of world, with every
// combination of the matrix cells it reads, and describes the first label for
// which match holds ("" if there is none). An error is an expression the
// evaluator cannot evaluate: the caller fails closed.
func forkRunnerFirst(world []map[string]string, expr string, keys []string, m forkRunnerMatrix, match func(string) bool) (string, error) {
	for _, base := range world {
		vals := make([][]string, len(keys))
		for i, key := range keys {
			v, err := forkRunnerCellValues(m.keys[key], base)
			if err != nil {
				return "", err
			}
			vals[i] = v
		}
		var found string
		var failed error
		forkRunnerEach(base, keys, vals, func(ctx map[string]string) bool {
			v, err := evalGHExpr(expr, ctx)
			if err != nil {
				failed = err
				return true
			}
			label, ok := v.(string)
			if !ok {
				failed = fmt.Errorf("evaluates to %#v, not a runner label", v)
				return true
			}
			if match(label) {
				found = forkRunnerDescribe(label, ctx)
				return true
			}
			return false
		})
		if failed != nil {
			return "", failed
		}
		if found != "" {
			return found, nil
		}
	}
	return "", nil
}

// forkRunnerMissing evaluates expr in every context of world, with every
// combination of the matrix cells it reads, and returns the labels of want that
// no context resolves it to (in want's order). It stops once every label has
// been seen, so a site whose guard is right costs one pass over its first
// contexts. An error is an expression the evaluator cannot evaluate: the caller
// fails closed.
func forkRunnerMissing(world []map[string]string, expr string, keys []string, m forkRunnerMatrix, want []string) ([]string, error) {
	left := slices.Clone(want)
	for _, base := range world {
		vals := make([][]string, len(keys))
		for i, key := range keys {
			v, err := forkRunnerCellValues(m.keys[key], base)
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		var failed error
		forkRunnerEach(base, keys, vals, func(ctx map[string]string) bool {
			v, err := evalGHExpr(expr, ctx)
			if err != nil {
				failed = err
				return true
			}
			label, ok := v.(string)
			if !ok {
				failed = fmt.Errorf("evaluates to %#v, not a runner label", v)
				return true
			}
			left = slices.DeleteFunc(left, func(w string) bool { return strings.EqualFold(w, label) })
			return len(left) == 0
		})
		if failed != nil {
			return nil, failed
		}
		if len(left) == 0 {
			return nil, nil
		}
	}
	return left, nil
}

// forkRunnerLabels: the Blacksmith runner labels that the runs-on expression, or
// a matrix cell it reads, spells out: what the guard must leave selectable in
// gastownhall's world. A cell that only says "blacksmith" is a marker, not a
// label.
func forkRunnerLabels(expr string, keys []string, m forkRunnerMatrix) ([]string, error) {
	want, err := forkExprLabels(expr)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		for _, c := range m.keys[key] {
			text := strings.TrimSpace(c.node.Value)
			if !strings.Contains(text, "${{") {
				want = forkRunnerWant(want, text)
				continue
			}
			labels, err := forkExprLabels(text)
			if err != nil {
				return nil, err
			}
			for _, label := range labels {
				want = forkRunnerWant(want, label)
			}
		}
	}
	return want, nil
}

// forkRunnerCombos: the combinations GitHub builds from the matrix, each as the
// cell every key it defines takes; a key a combination does not define is
// unset there. The base keys give their cross product; an include entry joins
// each original combination it fits (it overwrites none of that combination's
// base values) and adds its other keys, overwriting what an earlier include
// added, and an entry that fits none is a combination of its own that no later
// entry joins. An exclude entry removes combinations, so ignoring it can only
// add some: the sweep reads a superset.
func forkRunnerCombos(m forkRunnerMatrix) []map[string]*yaml.Node {
	var product []map[string]*yaml.Node
	if len(m.base) > 0 {
		keys := slices.Sorted(maps.Keys(m.base))
		product = []map[string]*yaml.Node{{}}
		for _, key := range keys {
			var next []map[string]*yaml.Node
			for _, combo := range product {
				for _, cell := range m.base[key] {
					c := maps.Clone(combo)
					c[key] = cell
					next = append(next, c)
				}
			}
			product = next
		}
	}
	var created []map[string]*yaml.Node
	for _, entry := range m.entries {
		fitted := false
		for _, combo := range product {
			fits := true
			for key, cell := range entry {
				if _, base := m.base[key]; base && strings.TrimSpace(cell.Value) != strings.TrimSpace(combo[key].Value) {
					fits = false
					break
				}
			}
			if !fits {
				continue
			}
			fitted = true
			for key, cell := range entry {
				if _, base := m.base[key]; !base {
					combo[key] = cell
				}
			}
		}
		if !fitted {
			created = append(created, maps.Clone(entry))
		}
	}
	return append(product, created...)
}

// forkRunnerUnset evaluates expr in every context of world, once for each
// combination GitHub builds from the matrix, with a key the combination does not
// define left unset (the evaluator reads it as null), and describes the first
// that resolves to no runner label ("" if there is none). The cross product that
// forkRunnerFirst reads has combinations GitHub never builds, so an empty
// answer there proves nothing: this is the pass that asks whether a job can be
// scheduled at all, for instance a fallback form with one entry that sets no
// fallback.
func forkRunnerUnset(world []map[string]string, expr string, keys []string, m forkRunnerMatrix) (string, error) {
	combos := forkRunnerCombos(m)
	if len(keys) == 0 {
		combos = []map[string]*yaml.Node{{}}
	}
	for _, base := range world {
		for _, combo := range combos {
			ctx := maps.Clone(base)
			for _, key := range keys {
				cell, ok := combo[key]
				if !ok {
					continue
				}
				vals, err := forkRunnerCellValues([]forkRunnerCell{{node: cell}}, base)
				if err != nil {
					return "", err
				}
				ctx["matrix."+key] = vals[0]
			}
			v, err := evalGHExpr(expr, ctx)
			if err != nil {
				return "", err
			}
			if label, ok := v.(string); !ok || strings.TrimSpace(label) == "" {
				return forkRunnerDescribeUnset(v, combo, keys, ctx), nil
			}
		}
	}
	return "", nil
}

// forkRunnerDescribeUnset: what v is, and the context that produced it.
func forkRunnerDescribeUnset(v any, combo map[string]*yaml.Node, keys []string, ctx map[string]string) string {
	what := "an empty label"
	switch v := v.(type) {
	case nil:
		what = "null"
	case bool:
		what = strconv.FormatBool(v)
	}
	s := fmt.Sprintf("%s with owner %q, event %s, head %q, rbe mode %q", what, ctx["github.repository_owner"], ctx["github.event_name"], ctx["github.event.pull_request.head.repo.full_name"], ctx["needs.rbe.outputs.mode"])
	for _, key := range keys {
		if _, ok := combo[key]; ok {
			s += fmt.Sprintf(", matrix.%s %q", key, ctx["matrix."+key])
		} else {
			s += fmt.Sprintf(", matrix.%s unset", key)
		}
	}
	return s
}

// forkRunnerSite: what one runs-on scalar of a job can resolve to.
type forkRunnerSite struct {
	leak      string       // why it can resolve to a label the workflow must not ask for, or cannot be read
	evaluated bool         // the fork's world was evaluated through: the answer in reached means something
	spelled   bool         // the scalar, or a matrix cell it reads, spells a Blacksmith label
	reached   bool         // gastownhall's world resolves it to a Blacksmith label
	cutOff    []string     // Blacksmith labels it spells that no gastownhall context resolves it to, though another does
	read      []*yaml.Node // the scalar, and every matrix cell it reads: not stray label text
}

// forkRunnerResolve works out what the runs-on scalar node can resolve to in
// the fork's world and in gastownhall's. m is its job's matrix.
func forkRunnerResolve(node *yaml.Node, m forkRunnerMatrix, owned bool) forkRunnerSite {
	site := forkRunnerSite{read: []*yaml.Node{node}}
	text := strings.TrimSpace(node.Value)
	site.spelled = forkBlacksmithAny.MatchString(text)
	if !strings.Contains(text, "${{") {
		site.evaluated, site.reached = true, site.spelled
		if forkRunnerBad(text, owned) {
			site.leak = "is the literal label " + strconv.Quote(text) + forkRunnerWhyBad(owned)
		}
		return site
	}
	if !forkRunnerPureExpr(text) {
		site.leak = "mixes text with an expression, which this scan cannot read (" + strconv.Quote(text) + "); extend this test or ask the architect"
		return site
	}
	idents, err := forkExprIdents(text)
	if err != nil {
		site.leak = "cannot be evaluated (" + err.Error() + "); extend this test or ask the architect"
		return site
	}
	var keys []string
	for _, id := range idents {
		key, isMatrix := strings.CutPrefix(id, "matrix.")
		switch {
		case isMatrix && m.dynamic:
			site.leak = "reads " + id + ", but this job's strategy.matrix is an expression this scan cannot expand; extend this test or ask the architect"
			return site
		case isMatrix && len(m.keys[key]) == 0:
			site.leak = "reads " + id + ", which this job's strategy.matrix never gives a value (an unreadable matrix); extend this test or ask the architect"
			return site
		case isMatrix:
			keys = append(keys, key)
		case !forkModelledIdents[id]:
			site.leak = "reads " + id + ", which this scan does not supply (the evaluator reads it as null, so a scan that leaves it out can only pass); extend this test or ask the architect"
			return site
		}
	}
	for _, key := range keys {
		for _, c := range m.keys[key] {
			site.read = append(site.read, c.node)
			site.spelled = site.spelled || forkBlacksmithAny.MatchString(c.node.Value)
			if !strings.Contains(c.node.Value, "${{") {
				continue
			}
			cellIdents, err := forkExprIdents(c.node.Value)
			if err != nil {
				site.leak = "reads the matrix cell " + strconv.Quote(c.node.Value) + ", which cannot be evaluated (" + err.Error() + "); extend this test or ask the architect"
				return site
			}
			for _, id := range cellIdents {
				if !forkModelledIdents[id] {
					site.leak = "reads the matrix cell " + strconv.Quote(c.node.Value) + ", which reads " + id + ", which this scan does not supply; extend this test or ask the architect"
					return site
				}
			}
		}
	}
	found, err := forkRunnerFirst(forkRunnerForkWorld, text, keys, m, func(label string) bool { return forkRunnerBad(label, owned) })
	if err != nil {
		site.leak = "cannot be evaluated (" + err.Error() + "); extend this test or ask the architect"
		return site
	}
	site.evaluated = true
	if found != "" {
		site.leak = "resolves to " + found + forkRunnerWhyBad(owned)
	} else {
		// Not a label of any kind is no Blacksmith label, so the pass above lets
		// it by; it is still a job that cannot be scheduled.
		none, err := forkRunnerUnset(forkRunnerForkWorld, text, keys, m)
		if err != nil {
			site.leak, site.evaluated = "cannot be evaluated ("+err.Error()+"); extend this test or ask the architect", false
			return site
		}
		if none != "" {
			site.leak = "resolves to " + none + ", which is no runner label, so the job cannot be scheduled there (a matrix entry that leaves out a key runs-on reads, a fallback for one?)"
		}
	}
	if !owned {
		up, err := forkRunnerFirst(forkRunnerUpstreamWorld, text, keys, m, forkBlacksmithAny.MatchString)
		if err != nil {
			site.leak, site.evaluated = "cannot be evaluated in gastownhall's world ("+err.Error()+"); extend this test or ask the architect", false
			return site
		}
		site.reached = up != ""
		if site.reached {
			// One label that gastownhall's world reaches does not vouch for the
			// others the site spells: every label upstream's pool has here must
			// be one a gastownhall context selects.
			want, err := forkRunnerLabels(text, keys, m)
			if err == nil {
				site.cutOff, err = forkRunnerMissing(forkRunnerUpstreamWorld, text, keys, m, want)
			}
			if err != nil {
				site.leak, site.evaluated = "cannot be evaluated in gastownhall's world ("+err.Error()+"); extend this test or ask the architect", false
				return site
			}
		}
	}
	return site
}

func forkRunnerWhyBad(owned bool) string {
	if owned {
		return ", which is not a GitHub-hosted runner (ubuntu-*, windows-*, macos-*): a fork-owned workflow runs on those only"
	}
	return ", a Blacksmith label"
}

// forkRunnerScan is what a sweep found. leaks are the ways a job in this
// repository can ask for a runner it must not; dead are the sites whose guard
// has cut upstream's own pool off.
type forkRunnerScan struct {
	workflows       int // read
	exemptWorkflows int // whose only trigger is a push to main
	jobs            int // with a runs-on, outside the exempt workflows
	sites           int // runs-on values swept
	exempt          int // runs-on values not swept: the exempt workflows'
	composite       int // composite actions read
	spelled         int // swept sites that spell a Blacksmith label
	reached         int // ... and resolve to one in gastownhall's world
	leaks           []string
	dead            []string
}

func (s *forkRunnerScan) add(o forkRunnerScan) {
	s.workflows += o.workflows
	s.exemptWorkflows += o.exemptWorkflows
	s.jobs += o.jobs
	s.sites += o.sites
	s.exempt += o.exempt
	s.composite += o.composite
	s.spelled += o.spelled
	s.reached += o.reached
	s.leaks = append(s.leaks, o.leaks...)
	s.dead = append(s.dead, o.dead...)
}

// empty: a sweep that visited no site proves nothing.
func (s forkRunnerScan) empty() string {
	if s.sites > 0 {
		return ""
	}
	return fmt.Sprintf("the sweep visited no runs-on site in %d workflows (%d of them exempt, push to main only): the workflows moved, or this scan no longer reads their shape; ask the architect", s.workflows, s.exemptWorkflows)
}

func forkRunnerWhere(file, job string) string {
	if job == "" {
		return file
	}
	return file + " job " + job
}

// forkRunnerStray calls fn for every scalar value under n, except the targets
// of aliases (they are scanned where they are defined). A mapping key that
// spells a label is not a choice of runner, so keys are left alone.
func forkRunnerStray(n *yaml.Node, job string, fn func(job string, n *yaml.Node)) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			forkRunnerStray(c, job, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			forkRunnerStray(n.Content[i+1], job, fn)
		}
	case yaml.ScalarNode:
		fn(job, n)
	}
}

// forkRunnerScanWorkflow sweeps one workflow. file is the name it is reported
// under; a base name that starts "fork-" is a fork-owned workflow.
func forkRunnerScanWorkflow(file string, src []byte) forkRunnerScan {
	scan := forkRunnerScan{workflows: 1}
	owned := strings.HasPrefix(filepath.Base(file), "fork-")
	fail := func(job, msg string) {
		scan.leaks = append(scan.leaks, forkRunnerWhere(file, job)+": "+msg)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		fail("", fmt.Sprintf("does not parse as a workflow (%v); this scan cannot read it", err))
		return scan
	}
	root := doc.Content[0]
	if owned {
		if loc := forkBlacksmithAny.FindIndex(src); loc != nil {
			line := 1 + strings.Count(string(src[:loc[0]]), "\n")
			fail("", fmt.Sprintf("line %d: a fork-owned workflow names Blacksmith (%q); it says nothing of Blacksmith, comments included, and runs on GitHub-hosted runners (rule R6 v4 rule 4)", line, src[loc[0]:loc[1]]))
		}
	}
	exempt := forkMirrorOnly(root)
	if exempt {
		scan.exemptWorkflows = 1
	}
	jobs := forkYAMLGet(root, "jobs")
	accounted := map[*yaml.Node]bool{}
	if jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			id, job := jobs.Content[i].Value, forkYAMLValue(jobs.Content[i+1])
			if job == nil || job.Kind != yaml.MappingNode {
				continue
			}
			scalars, problems := forkRunsOnScalars(forkYAMLGet(job, "runs-on"))
			if exempt {
				scan.exempt += len(scalars)
				continue
			}
			if forkYAMLGet(job, "<<") != nil {
				fail(id, "uses a YAML merge key (<<), which this scan does not expand; extend this test or ask the architect")
				continue
			}
			for _, p := range problems {
				fail(id, "runs-on "+p)
			}
			if len(scalars) > 0 {
				scan.jobs++
			}
			matrix := forkRunnerReadMatrix(job)
			for _, node := range scalars {
				scan.sites++
				site := forkRunnerResolve(node, matrix, owned)
				for _, n := range site.read {
					accounted[n] = true
				}
				if site.leak != "" {
					fail(id, fmt.Sprintf("line %d: runs-on %s", node.Line, site.leak))
				}
				if owned || !site.spelled {
					continue
				}
				scan.spelled++
				switch {
				case site.reached && len(site.cutOff) == 0:
					scan.reached++
				case site.reached:
					scan.dead = append(scan.dead, fmt.Sprintf("%s: line %d: runs-on spells the Blacksmith label %s, which no gastownhall context resolves it to, so its guard has cut that part of upstream's own pool off (a typo in the owner in one matrix cell or one branch, an inverted operator, a mode that is never remote)", forkRunnerWhere(file, id), node.Line, strings.Join(site.cutOff, ", ")))
				case site.evaluated:
					scan.dead = append(scan.dead, fmt.Sprintf("%s: line %d: runs-on spells a Blacksmith label but resolves to none in any gastownhall context, so its guard has cut upstream's own pool off (a typo in the owner, an inverted operator, a mode that is never remote)", forkRunnerWhere(file, id), node.Line))
				}
			}
		}
	}
	if exempt {
		return scan
	}
	strayLabel := func(job string, n *yaml.Node) {
		if accounted[n] {
			return
		}
		if label := forkBlacksmithName.FindString(n.Value); label != "" {
			fail(job, fmt.Sprintf("line %d: %s is a Blacksmith label outside a runs-on value", n.Line, label))
		}
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if val := forkYAMLValue(root.Content[i+1]); root.Content[i].Value == "jobs" && val != nil && val.Kind == yaml.MappingNode {
			for j := 0; j+1 < len(val.Content); j += 2 {
				forkRunnerStray(val.Content[j+1], val.Content[j].Value, strayLabel)
			}
			continue
		}
		forkRunnerStray(root.Content[i+1], "", strayLabel)
	}
	return scan
}

// forkRunnerCompositeLeak: why a composite action's text may not spell a
// Blacksmith label ("" if it does not). A composite action cannot choose a
// runner, so a label there is a mistake or a change upstream made on purpose;
// either way a human reads it (be-4y7b5s 5.3).
func forkRunnerCompositeLeak(file string, src []byte) string {
	loc := forkBlacksmithName.FindIndex(src)
	if loc == nil {
		return ""
	}
	line := 1 + strings.Count(string(src[:loc[0]]), "\n")
	return fmt.Sprintf("%s: line %d: a composite action spells a Blacksmith label (%q), and cannot choose a runner; ask the architect (be-4y7b5s 5.3)", file, line, src[loc[0]:loc[1]])
}

// forkRunnerSweep sweeps every workflow and composite action in the repository.
func forkRunnerSweep(t *testing.T) forkRunnerScan {
	t.Helper()
	root := sourceRepoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows under %s: %v", root, err)
	}
	sort.Strings(files)
	var total forkRunnerScan
	for _, file := range files {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			t.Fatal(err)
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		total.add(forkRunnerScanWorkflow(filepath.ToSlash(rel), src))
	}
	// Composite actions exist or they do not: there is nothing to sweep without them.
	_ = filepath.WalkDir(filepath.Join(root, ".github", "actions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (filepath.Ext(path) != ".yml" && filepath.Ext(path) != ".yaml") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		total.composite++
		if why := forkRunnerCompositeLeak(filepath.ToSlash(rel), src); why != "" {
			total.leaks = append(total.leaks, why)
		}
		return nil
	})
	return total
}

// Every workflow in the repository, not a fixed list, so a Blacksmith label in
// a file upstream adds later is caught too. The counts it logs (go test -v) are
// the acceptance evidence that the sweep looked: sites visited and sites exempt.
func TestForkWorkflowsNeverAskForBlacksmithOutsideUpstream(t *testing.T) {
	scan := forkRunnerSweep(t)
	for _, v := range scan.leaks {
		t.Errorf("%s; a job a pull request inside this fork can schedule would queue for a Blacksmith runner the fork does not have: apply rule R6 v4 (be-4y7b5s 5.1, be-8ofr93, be-6i64d8)", v)
	}
	if why := scan.empty(); why != "" {
		t.Error(why)
	}
	t.Logf("swept %d runs-on sites in %d jobs of %d workflows; %d sites exempt in %d workflows whose only trigger is a push to main; %d composite actions read; %d of the swept sites spell a Blacksmith label",
		scan.sites, scan.jobs, scan.workflows-scan.exemptWorkflows, scan.exempt, scan.exemptWorkflows, scan.composite, scan.spelled)
}

// forkWorkflowDoc: a workflow with the one job a, whose body is body.
func forkWorkflowDoc(trigger, body string) string {
	return trigger + "jobs:\n  a:\n" + body
}

// Known-positive and known-negative controls, independent of what the
// workflows hold: the scan reports every shape it must not accept (upstream's
// own literals as shipped, the guard written wrong, a form it cannot read) and
// accepts the shapes it must, both of the post-sync matrix forms among them.
// The first cases prove the contexts can reach a Blacksmith label at all, so a
// scan that passes cannot be passing vacuously.
func TestForkRunnerLabelScanControls(t *testing.T) {
	const (
		sameRepo = "(github.event_name == 'merge_group' || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository && github.actor != 'dependabot[bot]'))"
		bs2      = "blacksmith-2vcpu-ubuntu-2404"
		bs4      = "blacksmith-4vcpu-ubuntu-2404"
		bsWin    = "blacksmith-4vcpu-windows-2025"
		bsMac    = "blacksmith-6vcpu-macos-26"
		pr       = "on:\n  pull_request:\n"
		matrix   = "    strategy:\n      matrix:\n        include:\n"
	)
	// upstream's F3 literal as shipped, and with the fork's guard in front.
	unguarded := "${{ " + sameRepo + " && '" + bs2 + "' || 'ubuntu-latest' }}"
	guarded := "${{ " + forkBlacksmithGuard + sameRepo + " && '" + bs2 + "' || 'ubuntu-latest' }}"
	typoGuard := strings.Replace(guarded, "'gastownhall'", "'gastownhal'", 1)
	flippedGuard := strings.Replace(guarded, "==", "!=", 1)
	firstOperandOnly := "${{ " + forkBlacksmithGuard + "(github.event_name == 'merge_group') || (github.event_name == 'pull_request') && '" + bs2 + "' || 'ubuntu-latest' }}"
	modeKeyed := "${{ needs.rbe.outputs.mode == 'remote' && '" + bs4 + "' || 'ubuntu-latest' }}"
	enabledKeyed := "${{ needs.rbe.outputs.enabled == 'true' && '" + bs4 + "' || 'ubuntu-latest' }}"
	noModeKeyed := "${{ !needs.rbe.outputs.mode && '" + bs4 + "' || 'ubuntu-latest' }}"
	matrixTernary := "    strategy:\n      matrix:\n        runner: [blacksmith, github]\n    runs-on: ${{ matrix.runner == 'blacksmith' && 'blacksmith-8vcpu-ubuntu-2404' || 'ubuntu-latest' }}\n"
	// The post-sync pr.yml shapes (be-4y7b5s 5.3): the guard in the matrix cells
	// with runs-on: ${{ matrix.runner }} untouched, or the guard in runs-on with a
	// hosted fallback beside each cell.
	cellGuard := func(label, hosted string) string {
		return "${{ " + forkBlacksmithGuard + "'" + label + "' || '" + hosted + "' }}"
	}
	primary := matrix + "          - runner: " + cellGuard(bsWin, "windows-latest") + "\n          - runner: " + cellGuard(bsMac, "macos-latest") + "\n    runs-on: ${{ matrix.runner }}\n"
	bareCells := matrix + "          - runner: " + bsWin + "\n          - runner: " + bsMac + "\n    runs-on: ${{ matrix.runner }}\n"
	fallbackCells := matrix + "          - runner: " + bsWin + "\n            fallback: windows-latest\n          - runner: " + bsMac + "\n            fallback: macos-latest\n"
	fallback := fallbackCells + "    runs-on: ${{ " + forkBlacksmithGuard + "matrix.runner || matrix.fallback }}\n"
	// A guard that is right for one cell and wrong for the other, a fallback one
	// entry leaves out, and a runs-on with several labels in it.
	primaryTypoSecond := matrix + "          - runner: " + cellGuard(bsWin, "windows-latest") + "\n          - runner: " + strings.Replace(cellGuard(bsMac, "macos-latest"), "'gastownhall'", "'gastownhal'", 1) + "\n    runs-on: ${{ matrix.runner }}\n"
	fallbackMissing := matrix + "          - runner: " + bsWin + "\n            fallback: windows-latest\n          - runner: " + bsMac + "\n    runs-on: ${{ " + forkBlacksmithGuard + "matrix.runner || matrix.fallback }}\n"
	byEvent := func(owner string) string {
		return "${{ " + forkBlacksmithGuard + "github.event_name == 'pull_request' && '" + bs4 + "' || github.repository_owner == '" + owner + "' && github.event_name == 'push' && '" + bs2 + "' || 'ubuntu-latest' }}"
	}
	// A matrix with base keys, where include entries join the original
	// combinations or make combinations of their own (GitHub's rules, which the
	// empty-label pass reads exactly).
	mixed := func(base, entries string) string {
		return "    strategy:\n      matrix:\n        os: " + base + "\n        include:\n" + entries + "    runs-on: ${{ " + forkBlacksmithGuard + "matrix.runner || matrix.fallback }}\n"
	}
	linuxEntry := "          - os: linux\n            runner: " + bs2 + "\n            fallback: ubuntu-latest\n"
	macEntry := "          - os: mac\n            runner: " + bsMac + "\n            fallback: macos-latest\n"
	macEntryBare := "          - os: mac\n            runner: " + bsMac + "\n"

	type scanCase struct {
		name  string
		file  string // under .github/workflows; a.yml when empty
		src   string
		leaks int
		dead  int
	}
	cases := []scanCase{
		// Upstream's shapes and the guard done wrong: each is a leak.
		{name: "upstream's F3 literal as shipped", src: forkWorkflowDoc(pr, "    runs-on: "+unguarded+"\n"), leaks: 1},
		{name: "upstream's 8 vCPU literal as shipped", src: forkWorkflowDoc(pr, "    runs-on: "+strings.Replace(unguarded, "2vcpu", "8vcpu", 1)+"\n"), leaks: 1},
		{name: "guard that covers only the first operand", src: forkWorkflowDoc(pr, "    runs-on: "+firstOperandOnly+"\n"), leaks: 1},
		{name: "lane keyed on rbe's enabled output", src: forkWorkflowDoc(pr, "    runs-on: "+enabledKeyed+"\n"), leaks: 1},
		{name: "lane that is true only when rbe produced no mode", src: forkWorkflowDoc(pr, "    runs-on: "+noModeKeyed+"\n"), leaks: 1},
		{name: "flipped guard, which keeps the label here and loses it upstream", src: forkWorkflowDoc(pr, "    runs-on: "+flippedGuard+"\n"), leaks: 1, dead: 1},
		{name: "bare Blacksmith literal in an expression (the Windows jobs, #7355)", src: forkWorkflowDoc(pr, "    runs-on: ${{ '"+bsWin+"' }}\n"), leaks: 1},
		{name: "literal label", src: forkWorkflowDoc(pr, "    runs-on: "+bs2+"\n"), leaks: 1},
		{name: "literal label on a workflow with a dispatch trigger beside a push to main", src: forkWorkflowDoc("on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n", "    runs-on: "+bs2+"\n"), leaks: 1},
		{name: "runs-on list", src: forkWorkflowDoc(pr, "    runs-on: [self-hosted, "+bs2+"]\n"), leaks: 1},
		{name: "runs-on mapping, labels list", src: forkWorkflowDoc(pr, "    runs-on:\n      group: big\n      labels: [ubuntu-latest, "+bs2+"]\n"), leaks: 1},
		{name: "runs-on mapping with a key this scan does not read", src: forkWorkflowDoc(pr, "    runs-on:\n      runner: ubuntu-latest\n"), leaks: 1},
		{name: "runs-on left empty", src: forkWorkflowDoc(pr, "    runs-on:\n"), leaks: 1},
		{name: "runs-on that mixes text with an expression", src: forkWorkflowDoc(pr, "    runs-on: blacksmith-${{ matrix.size }}-ubuntu-2404\n"), leaks: 1},
		{name: "label through an alias, in both jobs that use it", src: "on:\n  pull_request:\njobs:\n  a:\n    runs-on: &r " + bs2 + "\n  b:\n    runs-on: *r\n", leaks: 2},
		{name: "job built with a YAML merge key", src: "on:\n  pull_request:\nx-job: &job\n  runs-on: ubuntu-latest\njobs:\n  a:\n    <<: *job\n", leaks: 1},
		{name: "matrix entry", src: forkWorkflowDoc(pr, "    strategy:\n      matrix:\n        runner: ["+bs2+"]\n    runs-on: ${{ matrix.runner }}\n"), leaks: 1},
		{name: "matrix of bare Blacksmith cells behind runs-on: ${{ matrix.runner }} (pr.yml at upstream)", src: forkWorkflowDoc(pr, bareCells), leaks: 1},
		{name: "matrix cell guarded on the wrong thing", src: forkWorkflowDoc(pr, matrix+"          - runner: ${{ github.event_name == 'pull_request' && '"+bsWin+"' || 'windows-latest' }}\n    runs-on: ${{ matrix.runner }}\n"), leaks: 1},
		{name: "matrix cell that is an expression the evaluator cannot read", src: forkWorkflowDoc(pr, matrix+"          - runner: ${{ contains(github.ref, 'x') && '"+bsWin+"' || 'windows-latest' }}\n    runs-on: ${{ matrix.runner }}\n"), leaks: 1},
		{name: "fallback form without the guard", src: forkWorkflowDoc(pr, fallbackCells+"    runs-on: ${{ matrix.runner || matrix.fallback }}\n"), leaks: 1},
		{name: "matrix ternary on a pull_request workflow", src: forkWorkflowDoc(pr, matrixTernary), leaks: 1},
		{name: "matrix ternary on a workflow that also has a dispatch trigger", src: forkWorkflowDoc("on:\n  push:\n    branches: [main]\n  workflow_dispatch:\n", matrixTernary), leaks: 1},
		{name: "include key unset in a combination no entry reaches", src: forkWorkflowDoc(pr, "    strategy:\n      matrix:\n        os: [a, b]\n        include:\n          - os: a\n            runner: x\n    runs-on: ${{ matrix.runner != 'x' && '"+bs2+"' || 'ubuntu-latest' }}\n"), leaks: 1},
		{name: "fallback form where one entry sets no fallback", src: forkWorkflowDoc(pr, fallbackMissing), leaks: 1},
		{name: "runs-on that evaluates to an empty label", src: forkWorkflowDoc(pr, "    runs-on: ${{ '' }}\n"), leaks: 1},
		{name: "guard with no hosted fallback", src: forkWorkflowDoc(pr, "    runs-on: ${{ "+forkBlacksmithGuard+"'"+bs2+"' }}\n"), leaks: 1},
		{name: "base-key matrix where an original combination no include entry reaches has no fallback", src: forkWorkflowDoc(pr, mixed("[linux, mac]", linuxEntry)), leaks: 1},
		{name: "base-key matrix where an entry that makes a combination of its own has no fallback", src: forkWorkflowDoc(pr, mixed("[linux]", linuxEntry+macEntryBare)), leaks: 1},
		{name: "matrix key nobody gives", src: forkWorkflowDoc(pr, "    strategy:\n      matrix:\n        os: [ubuntu-latest]\n    runs-on: ${{ matrix.runner }}\n"), leaks: 1},
		{name: "matrix built by fromJSON", src: forkWorkflowDoc(pr, "    strategy:\n      matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}\n    runs-on: ${{ matrix.runner }}\n"), leaks: 1},
		{name: "label in an env value", src: forkWorkflowDoc(pr, "    runs-on: ubuntu-latest\n    env:\n      RUNNER: "+bs2+"\n"), leaks: 1},
		{name: "label in a workflow-level env value", src: "on:\n  pull_request:\nenv:\n  RUNNER: " + bs2 + "\njobs:\n  a:\n    runs-on: ubuntu-latest\n", leaks: 1},
		{name: "label in a matrix that runs-on does not read", src: forkWorkflowDoc(pr, "    strategy:\n      matrix:\n        runner: ["+bs2+"]\n    runs-on: ubuntu-latest\n"), leaks: 1},
		{name: "expression the evaluator cannot read", src: forkWorkflowDoc(pr, "    runs-on: ${{ contains(github.ref, 'x') && '"+bs2+"' || 'ubuntu-latest' }}\n"), leaks: 1},
		{name: "runs-on reads a context the scan does not supply", src: forkWorkflowDoc(pr, "    runs-on: ${{ vars.USE_BLACKSMITH == 'true' && '"+bs2+"' || 'ubuntu-latest' }}\n"), leaks: 1},
		{name: "fork-owned workflow that names Blacksmith in a comment", file: "fork-x.yml", src: forkWorkflowDoc(pr, "    # upstream runs this job on blacksmith-2vcpu\n    runs-on: ubuntu-latest\n"), leaks: 1},
		{name: "fork-owned workflow on a runner that is not GitHub-hosted", file: "fork-x.yml", src: forkWorkflowDoc(pr, "    runs-on: self-hosted\n"), leaks: 1},
		{name: "workflow that does not parse", src: "on: [\njobs: {\n", leaks: 1},

		// Guards done right: no leak, and upstream's pool still reached.
		{name: "guarded literal", src: forkWorkflowDoc(pr, "    runs-on: "+guarded+"\n")},
		{name: "mode-keyed literal", src: forkWorkflowDoc(pr, "    runs-on: "+modeKeyed+"\n")},
		{name: "guarded literal in an expression (the Windows jobs)", src: forkWorkflowDoc(pr, "    runs-on: "+cellGuard(bsWin, "windows-latest")+"\n")},
		{name: "owner guard in front of a whole condition (pr-risk.yml)", src: forkWorkflowDoc("on:\n  pull_request:\n  merge_group:\n", "    runs-on: ${{ "+forkBlacksmithGuard+"(github.event_name == 'merge_group' || github.event_name == 'pull_request') && '"+bs2+"' || 'ubuntu-latest' }}\n")},
		{name: "post-sync matrix form: the guard in the cells", src: forkWorkflowDoc(pr, primary)},
		{name: "post-sync matrix form: the guard in runs-on, a hosted fallback beside each cell", src: forkWorkflowDoc(pr, fallback)},
		{name: "chain of labels, every branch reachable", src: forkWorkflowDoc(pr, "    runs-on: "+byEvent("gastownhall")+"\n")},
		{name: "base-key matrix whose include entries join its combinations", src: forkWorkflowDoc(pr, mixed("[linux, mac]", linuxEntry+macEntry))},
		{name: "base-key matrix with an entry that makes a combination of its own", src: forkWorkflowDoc(pr, mixed("[linux]", linuxEntry+macEntry))},
		{name: "matrix of hosted runners", src: forkWorkflowDoc(pr, "    strategy:\n      matrix:\n        os: [ubuntu-latest, windows-latest, macos-latest]\n    runs-on: ${{ matrix.os }}\n")},
		{name: "hosted runner", src: forkWorkflowDoc(pr, "    runs-on: ubuntu-latest\n")},
		{name: "prose in a run script", src: forkWorkflowDoc(pr, "    runs-on: ubuntu-latest\n    steps:\n      - run: |\n          # the Blacksmith runner's environment caps argv\n          echo hi\n")},
		{name: "job that calls a reusable workflow", src: forkWorkflowDoc(pr, "    uses: ./.github/workflows/x.yml\n")},
		{name: "fork-owned workflow on hosted runners", file: "fork-x.yml", src: forkWorkflowDoc("on:\n  pull_request:\n    branches: [integration]\n", "    strategy:\n      matrix:\n        os: [ubuntu-latest, macos-latest]\n    runs-on: ${{ matrix.os }}\n")},
		{name: "matrix ternary on a workflow that only runs on a push to main", src: forkWorkflowDoc("on:\n  push:\n    branches: [main]\n", matrixTernary)},
		{name: "bare literals on a workflow that only runs on a push to main (main.yml)", file: "main.yml", src: forkWorkflowDoc("on:\n  push:\n    branches: [main]\n", "    runs-on: ${{ '"+bsMac+"' }}\n    env:\n      RUNNER: "+bs4+"\n")},

		// Guards that disable upstream's pool: not a leak here, a dead site there.
		{name: "owner guard with a typo", src: forkWorkflowDoc(pr, "    runs-on: "+typoGuard+"\n"), dead: 1},
		{name: "lane keyed on a mode rbe never sets", src: forkWorkflowDoc(pr, "    runs-on: ${{ needs.rbe.outputs.mode == 'remot' && '"+bs4+"' || 'ubuntu-latest' }}\n"), dead: 1},
		{name: "lane behind an event that never happens", src: forkWorkflowDoc(pr, "    runs-on: ${{ github.event_name == 'never' && '"+bs4+"' || 'ubuntu-latest' }}\n"), dead: 1},
		{name: "fallback form guarded on the wrong owner", src: forkWorkflowDoc(pr, fallbackCells+"    runs-on: ${{ github.repository_owner == 'gastownhal' && matrix.runner || matrix.fallback }}\n"), dead: 1},
		{name: "matrix cells guarded on the wrong owner", src: forkWorkflowDoc(pr, matrix+"          - runner: ${{ github.repository_owner == 'gastownhal' && '"+bsWin+"' || 'windows-latest' }}\n    runs-on: ${{ matrix.runner }}\n"), dead: 1},
		// One label that is reached does not vouch for the others the site spells.
		{name: "matrix cells where only the second guard has a typo in the owner", src: forkWorkflowDoc(pr, primaryTypoSecond), dead: 1},
		{name: "chain of labels where only the second branch has a typo in the owner", src: forkWorkflowDoc(pr, "    runs-on: "+byEvent("gastownhal")+"\n"), dead: 1},
	}
	for _, c := range cases {
		file := c.file
		if file == "" {
			file = "a.yml"
		}
		got := forkRunnerScanWorkflow(".github/workflows/"+file, []byte(c.src))
		if len(got.leaks) != c.leaks || len(got.dead) != c.dead {
			t.Errorf("%s: %d leaks %q and %d dead sites %q, want %d and %d", c.name, len(got.leaks), got.leaks, len(got.dead), got.dead, c.leaks, c.dead)
		}
	}

	// A dead site names the label that is cut off, and only that one; a job that
	// cannot be scheduled names the key the combination leaves unset.
	for _, c := range []struct{ name, src, cutOff, reached string }{
		{"matrix cells where only the second guard has a typo", forkWorkflowDoc(pr, primaryTypoSecond), bsMac, bsWin},
		{"chain of labels where only the second branch has a typo", forkWorkflowDoc(pr, "    runs-on: "+byEvent("gastownhal")+"\n"), bs2, bs4},
	} {
		got := forkRunnerScanWorkflow(".github/workflows/a.yml", []byte(c.src))
		if len(got.dead) != 1 || !strings.Contains(got.dead[0], c.cutOff) || strings.Contains(got.dead[0], c.reached) || got.reached != 0 {
			t.Errorf("%s: dead %q and %d sites counted as reaching Blacksmith, want one dead site that names %s and not %s, and none counted", c.name, got.dead, got.reached, c.cutOff, c.reached)
		}
	}
	if got := forkRunnerScanWorkflow(".github/workflows/a.yml", []byte(forkWorkflowDoc(pr, fallbackMissing))); len(got.leaks) != 1 || !strings.Contains(got.leaks[0], "matrix.fallback unset") {
		t.Errorf("a fallback one entry leaves out is reported as %q, want it to name matrix.fallback as unset", got.leaks)
	}
	// Only a label runs-on can choose is expected of gastownhall's world: an
	// operand of == or != is compared with something, and a marker is no label.
	if labels, err := forkExprLabels("${{ matrix.runner == '" + bs4 + "' && '" + bs2 + "' || '" + bs4 + "' != matrix.runner && 'blacksmith' || 'ubuntu-latest' }}"); err != nil || !reflect.DeepEqual(labels, []string{bs2}) {
		t.Errorf("the labels a runs-on can choose = %q, %v; want just %s", labels, err, bs2)
	}

	// The combinations GitHub builds from a matrix, on the example its
	// documentation gives for include (fruit, animal, color, shape), in which
	// an include entry joins every original combination it overwrites nothing
	// in, and one that fits none makes a combination that no later entry joins.
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("strategy:\n  matrix:\n    fruit: [apple, pear]\n    animal: [cat, dog]\n    include:\n      - color: green\n      - color: pink\n        animal: cat\n      - fruit: apple\n        shape: circle\n      - fruit: banana\n      - fruit: banana\n        animal: cat\n"), &doc); err != nil {
		t.Fatal(err)
	}
	var combos []string
	for _, combo := range forkRunnerCombos(forkRunnerReadMatrix(doc.Content[0])) {
		var kv []string
		for k, cell := range combo {
			kv = append(kv, k+"="+cell.Value)
		}
		sort.Strings(kv)
		combos = append(combos, strings.Join(kv, ","))
	}
	sort.Strings(combos)
	wantCombos := []string{
		"animal=cat,color=pink,fruit=apple,shape=circle",
		"animal=cat,color=pink,fruit=pear",
		"animal=cat,fruit=banana",
		"animal=dog,color=green,fruit=apple,shape=circle",
		"animal=dog,color=green,fruit=pear",
		"fruit=banana",
	}
	if !reflect.DeepEqual(combos, wantCombos) {
		t.Errorf("the combinations of the documentation's include example = %q, want %q", combos, wantCombos)
	}

	// A failure names the file and the job, so the fix is one edit in one place.
	got := forkRunnerScanWorkflow(".github/workflows/a.yml", []byte(forkWorkflowDoc(pr, "    runs-on: "+unguarded+"\n")))
	if len(got.leaks) != 1 || !strings.HasPrefix(got.leaks[0], ".github/workflows/a.yml job a: ") {
		t.Errorf("a leak is reported as %q, want it to start with the file and the job", got.leaks)
	}
	// The exempt workflows are counted, not evaluated, and never make the sweep empty.
	got = forkRunnerScanWorkflow(".github/workflows/main.yml", []byte(forkWorkflowDoc("on:\n  push:\n    branches: [main]\n", "    runs-on: ${{ '"+bsMac+"' }}\n")))
	if got.sites != 0 || got.exempt != 1 || got.exemptWorkflows != 1 || got.empty() == "" {
		t.Errorf("a push-to-main-only workflow was counted as %+v, want 0 sites swept, 1 exempt and an empty sweep", got)
	}
	// A sweep that visits no site is a failure, whatever else it found.
	for _, src := range []string{"on: pull_request\njobs: {}\n", forkWorkflowDoc(pr, "    uses: ./.github/workflows/x.yml\n"), "on: pull_request\n"} {
		if got = forkRunnerScanWorkflow(".github/workflows/a.yml", []byte(src)); got.sites != 0 || got.empty() == "" {
			t.Errorf("%q swept %d sites and is %q, want a sweep that is empty", src, got.sites, got.empty())
		}
	}
	got = forkRunnerScanWorkflow(".github/workflows/a.yml", []byte(forkWorkflowDoc(pr, "    runs-on: ubuntu-latest\n")))
	if got.sites != 1 || got.jobs != 1 || got.empty() != "" {
		t.Errorf("one hosted job was counted as %+v, want 1 site in 1 job", got)
	}

	// Composite actions cannot choose a runner.
	for _, c := range []struct {
		name string
		src  string
		hit  bool
	}{
		{"a step that passes a Blacksmith label on", "runs:\n  using: composite\n  steps:\n    - uses: x/y@v1\n      with:\n        runner: " + bs2 + "\n", true},
		{"a comment that names a label", "# runs on " + bsWin + "\nruns:\n  using: composite\n  steps: []\n", true},
		{"prose that only says Blacksmith", "runs:\n  using: composite\n  steps:\n    - run: echo \"the Blacksmith pool\"\n      shell: bash\n", false},
	} {
		if got := forkRunnerCompositeLeak("action.yml", []byte(c.src)) != ""; got != c.hit {
			t.Errorf("composite action with %s: reported %v, want %v", c.name, got, c.hit)
		}
	}

	// The two worlds are the ones the rule names. Mode remote is gastownhall's
	// alone, rbe's skipped job (no mode) and every other mode are both worlds',
	// and the fork's world includes a repository with no owner at all.
	forkModes, noOwner := map[string]bool{}, false
	for _, ctx := range forkRunnerForkWorld {
		forkModes[ctx["needs.rbe.outputs.mode"]] = true
		owner, ok := ctx["github.repository_owner"]
		noOwner = noOwner || !ok
		if owner == "gastownhall" {
			t.Fatalf("the fork's world holds a context owned by gastownhall: %v", ctx)
		}
	}
	upModes := map[string]bool{}
	for _, ctx := range forkRunnerUpstreamWorld {
		upModes[ctx["needs.rbe.outputs.mode"]] = true
		if ctx["github.repository_owner"] != "gastownhall" {
			t.Fatalf("gastownhall's world holds a context owned by %q: %v", ctx["github.repository_owner"], ctx)
		}
	}
	for _, mode := range []string{"", "skip", "cache", "local", "fork-ro", "fork-rw"} {
		if !forkModes[mode] || !upModes[mode] {
			t.Errorf("rbe mode %q is missing from the fork's world (%v) or gastownhall's (%v)", mode, forkModes[mode], upModes[mode])
		}
	}
	if forkModes["remote"] || !upModes["remote"] || !noOwner {
		t.Errorf("remote mode in the fork's world %v, in gastownhall's %v, a repository with no owner in the fork's world %v; want false, true, true", forkModes["remote"], upModes["remote"], noOwner)
	}

	// Not vacuous: outside this fork the unguarded literal does reach Blacksmith.
	// The upstream world is the one forkUpstreamWorld gives a context that names
	// no owner, which is how upstream's own evaluator tests mean it.
	evalLabel := func(expr string, ctx map[string]string) string {
		v, err := evalGHExpr(expr, forkUpstreamWorld(ctx))
		if err != nil {
			t.Fatalf("evalGHExpr(%q): %v", expr, err)
		}
		label, _ := v.(string)
		return label
	}
	upstream := map[string]string{
		"github.event_name": "pull_request", "github.repository": "gastownhall/beads",
		"github.event.pull_request.head.repo.full_name": "gastownhall/beads", "github.actor": "alice",
	}
	if got := evalLabel(unguarded, upstream); got != bs2 {
		t.Errorf("upstream's literal on a same-repository pull request in gastownhall = %q, want %s", got, bs2)
	}
	if got := evalLabel(guarded, upstream); got != bs2 {
		t.Errorf("the guarded literal on a same-repository pull request in gastownhall = %q, want it unchanged from upstream", got)
	}
	upstream["github.repository_owner"] = "versioned-beads"
	if got := evalLabel(guarded, upstream); got != "ubuntu-latest" {
		t.Errorf("the guarded literal in versioned-beads = %q, want ubuntu-latest", got)
	}
}

// forkUpstreamWorld: the context an upstream evaluator test means. Upstream's
// tests model a pull request in gastownhall/beads and know nothing of an owner,
// so a context that does not name one gets upstream's. Only the two helpers that
// evaluate a runs-on for upstream's tests call this (mustEvalGHRunsOn,
// resolveRunsOnLabel; rule R6's E1 puts the call in) and the controls above;
// the sweep calls evalGHExpr itself and always names the owner, because for the
// sweep a missing owner is a case to try, not a default.
func forkUpstreamWorld(ctx map[string]string) map[string]string {
	if _, ok := ctx["github.repository_owner"]; ok {
		return ctx
	}
	world := make(map[string]string, len(ctx)+1)
	for k, v := range ctx {
		world[k] = v
	}
	world["github.repository_owner"] = "gastownhall"
	return world
}

// The old home of this test was upstream's cache sweep
// (TestBlacksmithReachableAdvisoryJobsNeverSaveACache), which skips every job
// whose runs-on does not resolve to a Blacksmith label under its two trust
// contexts. Those contexts name no owner, so once the guard is in, a guard that
// disabled the pool would leave the sweep examining nothing and still passing:
// 16 of its 76 (job, context) pairs before rule R6, 0 after (the first R6 v2
// sync, vb #101). This pins the same thing for every workflow, without that
// sweep: every runs-on that spells a Blacksmith label still resolves to one in
// gastownhall's world (rbe mode remote included), so a guard that is wrong the
// other way fails here, not on a pull request to upstream.
func TestForkUpstreamSweepStillReachesBlacksmithJobs(t *testing.T) {
	scan := forkRunnerSweep(t)
	for _, d := range scan.dead {
		t.Errorf("%s: apply rule R6 v4 (be-4y7b5s 5.4)", d)
	}
	if scan.spelled == 0 {
		t.Errorf("no swept runs-on spells a Blacksmith label (%d sites in %d workflows): the sweep examines nothing, or upstream changed the shape; ask the architect", scan.sites, scan.workflows)
	}
	t.Logf("%d of %d swept runs-on sites spell a Blacksmith label, and %d of them resolve to one in gastownhall's world", scan.spelled, scan.sites, scan.reached)
}
