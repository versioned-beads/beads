package scripts_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the fork's own (upstream has no copy, so syncs never conflict
// on it). It is the scan half of the pins of the embedded-gated doctor lane
// (be-zbhceo, rule M1): TestForkEmbeddedGateHasALane fails when a package
// gains a test gated on BEADS_TEST_EMBEDDED_DOLT=1 that no lane runs with the
// variable set. A gated test skips wherever the variable is unset, and go test
// exits 0 on a skip, so such a test ends as a skip on every pull request and a
// green check says nothing about it. cmd/bd/doctor holds two that no shard
// selects; the doctor-embedded job of .github/workflows/fork-embedded-gated.yml
// runs exactly those, and fork_embedded_gated_lane_pin_test.go pins that job.
//
// The scan reads every Go source of the module, test and non-test, so this
// file is in go_sources_test, which carries exactly that; the lane pin reads
// the workflows and is in scripts_test. Both share
// fork_embedded_gate_pin_helpers_test.go. go_sources_test compiles no other
// helper of scripts/ than sourceRepoRoot and isFileOrFileLink
// (repo_helpers_test.go), hence the fork-prefixed copies at the end of this
// file, which an edit of the originals cannot change.

// forkDoctorPackage: the package whose gated tests the doctor-embedded job runs
// by name.
const forkDoctorPackage = "cmd/bd/doctor"

// embeddedGateLanes: the packages whose tests spell the gate, and the lane
// that runs them with it set. With embeddedGateGaps, which lists the packages
// that have no verified lane, it is compared with the packages the scan finds
// (TestForkEmbeddedGateHasALane); only the keys are checked and the values are
// for the reader. cmd/bd's lane selects only TestEmbedded*
// (embedded-test-shard.sh), so this table is package level and is test level
// only for the fork's own lane, cmd/bd/doctor: a gated test of cmd/bd outside
// that prefix is an upstream gap it does not close.
var embeddedGateLanes = map[string]string{
	"cmd/bd":                        "pr-risk.yml test-embedded-cmd (TestEmbedded* shards) and its Bazel twin",
	"cmd/bd/doctor":                 forkEmbeddedGatedWorkflow + " doctor-embedded",
	"internal/storage/embeddeddolt": "pr-risk.yml test-embedded-storage and its Bazel twin",
}

// embeddedGateGaps: the packages whose tests spell the gate and that no lane
// verified to run on this fork runs with it set. A key claims no lane: it
// acknowledges a gap that is older than the scan and keeps the scan green, and
// its value says what the ruling it cites did and did not check. The day such a
// package gets a lane, its key moves to embeddedGateLanes. A key is added only
// by a ruling, never by whoever finds the directory. Only the keys are checked
// (TestForkEmbeddedGateHasALane), and no key is in both tables.
var embeddedGateGaps = map[string]string{
	"backend/http": "be-adi2q6 acknowledges this gap. Its upstream Bazel twin is //backend/http:http_served_test, which sets " +
		"BEADS_TEST_EMBEDDED_DOLT=1 and BEADS_HTTP_TEST_REQUIRED=1 and is selected by `bazel test //... --config=embedded` " +
		"(job bazel-embedded). There is no verified fork-hosted lane; PR126 `Bazel / embedded Dolt tier` was SKIPPED " +
		"(its if: was not examined). BEADS_HTTP_TEST_REQUIRED=1 turns a missing gate into a failure only in that twin. " +
		"Gap acknowledged, not closed",
	"backend/http/corpus": "be-s847fi section 7 acknowledges this gap, in the form of be-adi2q6 D6. Its upstream Bazel twin is " +
		"//backend/http/corpus:corpus_served_test, which sets BEADS_TEST_EMBEDDED_DOLT=1 and BEADS_HTTP_TEST_REQUIRED=1 and is " +
		"selected by `bazel test //... --config=embedded` (job bazel-embedded). The gap is that no fork-hosted lane runs it and " +
		"Scope S does not cover it (S is the served-http job of be-s847fi, limited to backend/http and internal/httpclient). " +
		"The failure on a missing gate comes from the test code in any lane that sets BEADS_HTTP_TEST_REQUIRED=1. " +
		"Gap acknowledged, not closed",
	"internal/httpclient": "be-adi2q6 acknowledges this gap. Its upstream Bazel twin is //internal/httpclient:httpclient_served_test, which sets " +
		"BEADS_TEST_EMBEDDED_DOLT=1 and BEADS_HTTP_TEST_REQUIRED=1 and is selected by `bazel test //... --config=embedded` " +
		"(job bazel-embedded). There is no verified fork-hosted lane; PR126 `Bazel / embedded Dolt tier` was SKIPPED " +
		"(its if: was not examined). BEADS_HTTP_TEST_REQUIRED=1 turns a missing gate into a failure only in that twin. " +
		"Gap acknowledged, not closed",
}

// Every package whose tests spell the gate has a lane that runs them with it
// set or an acknowledged gap, and every gated test of cmd/bd/doctor is one the
// doctor-embedded job runs by name. A gate is easy to copy into a new test
// file, and a gated test that no lane selects is a skip on every pull request.
//   - G1: the directories whose _test.go files spell the gate in a string
//     literal are the keys of embeddedGateLanes and embeddedGateGaps together,
//     both ways, and no directory is a key of both.
//   - G2: the top-level tests of cmd/bd/doctor that read the gate are
//     forkDoctorGatedTests, both ways (see gatedTests for what reading is).
//   - G3: no non-test .go file spells the gate, since a test that calls what
//     it defines would escape G1 and G2.
//
// The scan reads the source tree, which go_sources_test declares, and runs
// under go test and under Bazel alike. It first checks that it saw a
// plausible number of files, so a tree that is empty or only partly there
// fails as a broken scan and never passes for want of anything to find.
func TestForkEmbeddedGateHasALane(t *testing.T) {
	const minTestFiles, minNonTestFiles = 1000, 500
	scan, err := scanEmbeddedGate(sourceRepoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if scan.testFiles < minTestFiles || scan.nonTestFiles < minNonTestFiles {
		t.Fatalf("the scan is broken: it saw %d _test.go and %d other .go files, want at least %d and %d, so the tree it was handed is missing or partial and finding nothing would say nothing", scan.testFiles, scan.nonTestFiles, minTestFiles, minNonTestFiles)
	}
	known := map[string]bool{}
	for dir := range embeddedGateLanes {
		known[dir] = true
	}
	for dir := range embeddedGateGaps {
		if _, both := embeddedGateLanes[dir]; both {
			t.Errorf("%s is a key of embeddedGateLanes and of embeddedGateGaps; a package has a lane that runs its gated tests or an acknowledged gap, not both", dir)
		}
		known[dir] = true
	}
	add, drop := listDiff(forkGateSortedKeys(known), scan.testDirs)
	for _, dir := range add {
		t.Errorf("%s has tests gated on %s and no entry in embeddedGateLanes or embeddedGateGaps. A gated test skips wherever the variable is unset, and go test exits 0 on a skip: run its gated tests by name in %s, as the doctor-embedded job does, add the directory to embeddedGateLanes, and go back to the architect (be-zbhceo). A gap that stays open is added to embeddedGateGaps only by a ruling, as be-adi2q6 did for the first two", dir, embeddedDoltGate, forkEmbeddedGatedWorkflow)
	}
	for _, dir := range drop {
		if _, ok := embeddedGateLanes[dir]; ok {
			t.Errorf("embeddedGateLanes lists %s, whose tests no longer spell %s; drop it", dir, embeddedDoltGate)
		}
		if _, ok := embeddedGateGaps[dir]; ok {
			t.Errorf("embeddedGateGaps lists %s, whose tests no longer spell %s; drop it", dir, embeddedDoltGate)
		}
	}
	listed := append([]string(nil), forkDoctorGatedTests...)
	sort.Strings(listed)
	if add, drop := listDiff(listed, gatedTests(scan.doctor)); len(add)+len(drop) > 0 {
		t.Errorf("the gated tests of %s differ from forkDoctorGatedTests.\n  add to forkDoctorGatedTests: %v\n  drop from forkDoctorGatedTests: %v\nUpdate the list, the -run pattern and GATED_TESTS of the doctor-embedded job together, so that each one runs by name and is reported PASS (be-zbhceo)", forkDoctorPackage, add, drop)
	}
	for _, file := range scan.nonTest {
		t.Errorf("%s is not a test file and spells %s; a test that calls what it defines would escape the checks above, which read test files: gate the test in its own _test.go file", file, embeddedDoltGate)
	}
}

// embeddedGateScan: where the source tree spells the gate, and how many Go
// files the scan saw, so that a caller can tell a scan of the module from a
// scan of nothing.
type embeddedGateScan struct {
	testDirs     []string    // module-relative directories with a _test.go file that spells it, sorted
	nonTest      []string    // the non-test .go files that spell it, sorted
	doctor       []*ast.File // every _test.go file of forkDoctorPackage
	testFiles    int         // the _test.go files the walk admitted, those directly in scripts/ included
	nonTestFiles int         // the other .go files the walk admitted, those directly in scripts/ included
}

// scanEmbeddedGate walks the module rooted at root. It skips .git,
// node_modules, .beads and bazel-* trees, nested modules and the Go files
// directly in scripts/ (their tests name the gate as data); it reads a symlink
// to a file as the file, since Bazel's runfiles trees are symlink forests. A
// file is filtered on its bytes and parsed only when it holds the gate's name,
// except the _test.go files of forkDoctorPackage, which are all parsed: a test
// there can reach the gate through a helper in a file that does not spell it.
// Every .go file the walk admits is counted, the scripts/ ones too.
func scanEmbeddedGate(root string) (embeddedGateScan, error) {
	var scan embeddedGateScan
	dirs := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (name == ".git" || name == "node_modules" || name == ".beads" || strings.HasPrefix(name, "bazel-")) {
				return filepath.SkipDir
			}
			if p != root {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || !isFileOrFileLink(p, d) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		isTest := strings.HasSuffix(p, "_test.go")
		if isTest {
			scan.testFiles++
		} else {
			scan.nonTestFiles++
		}
		if dir == "scripts" {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		inDoctor := isTest && dir == forkDoctorPackage
		if !inDoctor && !bytes.Contains(src, []byte(embeddedDoltGate)) {
			return nil
		}
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return err
		}
		if inDoctor {
			scan.doctor = append(scan.doctor, f)
		}
		if readsGate(f, nil) {
			if isTest {
				dirs[dir] = true
			} else {
				scan.nonTest = append(scan.nonTest, rel)
			}
		}
		return nil
	})
	scan.testDirs = forkGateSortedKeys(dirs)
	sort.Strings(scan.nonTest)
	return scan, err
}

// gatedTests: the top-level tests among files (the _test.go files of one
// directory) that read the gate, sorted. A test reads it when its body spells
// the gate, names a package-level const or var that holds it, or names a
// function of its package that reads it, to any depth. A _test package shares
// the directory but not the identifiers, so packages are told apart. Methods
// and qualified names are not followed, so this is a lower bound.
func gatedTests(files []*ast.File) []string {
	byPkg := map[string][]*ast.File{}
	for _, f := range files {
		byPkg[f.Name.Name] = append(byPkg[f.Name.Name], f)
	}
	gated := map[string]bool{}
	for _, pkg := range byPkg {
		reads := gateReaders(pkg)
		for _, f := range pkg {
			for _, decl := range f.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && forkGateTestFunc.MatchString(fn.Name.Name) && forkGateTakesTestingT(fn) && reads[fn.Name.Name] {
					gated[fn.Name.Name] = true
				}
			}
		}
	}
	return forkGateSortedKeys(gated)
}

// gateReaders: the package-level funcs, consts and vars of the package made
// of files that read the gate, to any depth: the closure of "spells the gate
// or names a reader".
func gateReaders(files []*ast.File) map[string]bool {
	defs := map[string][]ast.Node{}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Body != nil {
					defs[d.Name.Name] = append(defs[d.Name.Name], d.Body)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok {
						for _, name := range v.Names {
							for _, value := range v.Values {
								defs[name.Name] = append(defs[name.Name], value)
							}
						}
					}
				}
			}
		}
	}
	reads := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, nodes := range defs {
			for _, n := range nodes {
				if !reads[name] && readsGate(n, reads) {
					reads[name] = true
					changed = true
				}
			}
		}
	}
	return reads
}

// readsGate: n holds a string literal that spells the gate, or names one of
// reads. The Sel of a selector is a method, a field or a qualified name and is
// not followed.
func readsGate(n ast.Node, reads map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if s, err := strconv.Unquote(n.Value); err == nil && strings.Contains(s, embeddedDoltGate) {
					found = true
				}
			}
		case *ast.Ident:
			found = found || reads[n.Name]
		case *ast.SelectorExpr:
			found = found || readsGate(n.X, reads)
			return false
		}
		return !found
	})
	return found
}

// forkGateSortedKeys: the keys of m, sorted. A copy of sortedKeys, which
// go_sources_test does not compile.
func forkGateSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// forkGateTestFunc: the names of top-level tests. A copy of the pattern of
// bazelGuardTestFunc, which go_sources_test does not compile.
var forkGateTestFunc = regexp.MustCompile(`^Test([^a-z].*)?$`)

// forkGateTakesTestingT: fn has exactly one parameter, of type *X.T. A copy of
// takesTestingT, which go_sources_test does not compile.
func forkGateTakesTestingT(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if len(params) != 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "T"
}
